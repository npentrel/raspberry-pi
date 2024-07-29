//go:build linux && (arm64 || arm) && !no_pigpio && !no_cgo

// Package rpiservo implements pi servo
package rpiservo

/*
	This driver contains various functionalities of a servo motor used in
	conjunction with a Raspberry Pi. The servo connects via a GPIO pin and
	uses the pi module's pigpio daemon library to control the servo motor.
	The servo pin will override the default pin configuration of of the pi
	module, including PWM frequency and width.

	Servo hardware model: DigiKey - SER0006 DFRobot
	https://www.digikey.com/en/products/detail/dfrobot/SER0006/7597224?WT.mc_id=frommaker.io

	Servo datasheet:
	http://www.ee.ic.ac.uk/pcheung/teaching/DE1_EE/stores/sg90_datasheet.pdf
*/

// #include <stdlib.h>
// #include <pigpiod_if2.h>
// #cgo LDFLAGS: -lpigpio
// #include "../rpi/pi.h"
import "C"

import (
	"context"
	"fmt"
	"time"

	rpiutils "viamrpi/utils"

	"github.com/pkg/errors"
	"go.viam.com/utils"

	"go.viam.com/rdk/components/servo"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/operation"
	"go.viam.com/rdk/resource"
)

var Model = resource.NewModel("viam", "raspberry-pi", "rpi-servo")

// Default configuration collected from data sheet
var (
	holdTime                = 250000000 // 250ms in nanoseconds
	servoDefaultMaxRotation = 180
)

// init registers a pi servo based on pigpio.
func init() {
	resource.RegisterComponent(
		servo.API,
		Model,
		resource.Registration[servo.Servo, *ServoConfig]{
			Constructor: newPiServo,
		},
	)
}

// Validate and set piPigpioServo fields based on the configuration.
func (s *piPigpioServo) validateAndSetConfiguration(conf *ServoConfig) error {
	if conf.Min > 0 {
		s.min = uint32(conf.Min)
	}
	if conf.Max > 0 {
		s.max = uint32(conf.Max)
	}
	s.maxRotation = uint32(conf.MaxRotation)
	if s.maxRotation == 0 {
		s.maxRotation = uint32(servoDefaultMaxRotation)
	}
	if s.maxRotation < s.min {
		return errors.New("maxRotation is less than minimum")
	}
	if s.maxRotation < s.max {
		return errors.New("maxRotation is less than maximum")
	}

	s.pinname = conf.Pin

	return nil
}

func newPiServo(
	ctx context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (servo.Servo, error) {
	// Parse the configuration
	newConf, err := parseConfig(conf)
	if err != nil {
		return nil, err
	}

	// Validate the configuration
	if err := validateConfig(newConf); err != nil {
		return nil, err
	}

	// Get Broadcom pin from hardware label
	bcom, err := getBroadcomPin(newConf.Pin)
	if err != nil {
		return nil, err
	}

	// Create and initialize the servo
	theServo, err := initializeServo(conf, logger, bcom, newConf)
	if err != nil {
		return nil, err
	}

	// Set the initial position of the servo
	if err := setInitialPosition(theServo, newConf); err != nil {
		return nil, err
	}

	// Handle the hold position configuration
	handleHoldPosition(theServo, newConf)

	return theServo, nil
}

// parseConfig parses the provided configuration into a ServoConfig.
func parseConfig(conf resource.Config) (*ServoConfig, error) {
	newConf, err := resource.NativeConfig[*ServoConfig](conf)
	if err != nil {
		return nil, err
	}
	return newConf, nil
}

// validateConfig validates the provided ServoConfig.
func validateConfig(newConf *ServoConfig) error {
	if newConf.Pin == "" {
		return errors.New("need pin for pi servo")
	}
	return nil
}

// getBroadcomPin retrieves the Broadcom pin number from the hardware label.
func getBroadcomPin(pin string) (uint, error) {
	bcom, have := rpiutils.BroadcomPinFromHardwareLabel(pin)
	if !have {
		return 0, errors.Errorf("no hw mapping for %s", pin)
	}
	return bcom, nil
}

// initializeServo creates and initializes the piPigpioServo with the provided configuration and logger.
func initializeServo(conf resource.Config, logger logging.Logger, bcom uint, newConf *ServoConfig) (*piPigpioServo, error) {
	theServo := &piPigpioServo{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
		pin:    C.uint(bcom),
		opMgr:  operation.NewSingleOperationManager(),
	}

	// Validate and set the servo configuration
	if err := theServo.validateAndSetConfiguration(newConf); err != nil {
		return nil, err
	}

	// Start a separate connection to the pigpio daemon
	piID := C.custom_pigpio_start()
	theServo.piID = piID

	return theServo, nil
}

// setInitialPosition sets the initial position of the servo based on the provided configuration.
func setInitialPosition(theServo *piPigpioServo, newConf *ServoConfig) error {
	var setPos C.int
	if newConf.StartPos == nil {
		// Set the servo to the default 90 degrees position
		setPos = C.set_servo_pulsewidth(theServo.piID, theServo.pin, C.uint(1500))
	} else {
		// Set the servo to the specified start position
		setPos = C.set_servo_pulsewidth(
			theServo.piID, theServo.pin,
			C.uint(angleToPulseWidth(int(*newConf.StartPos), int(theServo.maxRotation))),
		)
	}
	errorCode := int(setPos)
	if errorCode != 0 {
		return rpiutils.ConvertErrorCodeToMessage(errorCode, "gpioServo failed with")
	}
	return nil
}

// handleHoldPosition configures the hold position setting for the servo.
func handleHoldPosition(theServo *piPigpioServo, newConf *ServoConfig) {
	if newConf.HoldPos == nil || *newConf.HoldPos {
		// Hold the servo position
		theServo.holdPos = true
	} else {
		// Release the servo position and disable the servo
		theServo.res = C.get_servo_pulsewidth(theServo.piID, theServo.pin)
		theServo.holdPos = false
		C.set_servo_pulsewidth(theServo.piID, theServo.pin, C.uint(0)) // disables servo
	}
}

// piPigpioServo implements a servo.Servo using pigpio.
type piPigpioServo struct {
	resource.Named
	resource.AlwaysRebuild
	logger      logging.Logger
	pin         C.uint
	pinname     string
	res         C.int
	min, max    uint32
	opMgr       *operation.SingleOperationManager
	pulseWidth  int // pulsewidth value, 500-2500us is 0-180 degrees, 0 is off
	holdPos     bool
	maxRotation uint32
	piID        C.int
}

// Move moves the servo to the given angle (0-180 degrees)
// This will block until done or a new operation cancels this one
func (s *piPigpioServo) Move(ctx context.Context, angle uint32, extra map[string]interface{}) error {
	ctx, done := s.opMgr.New(ctx)
	defer done()

	if s.min > 0 && angle < s.min {
		angle = s.min
	}
	if s.max > 0 && angle > s.max {
		angle = s.max
	}
	pulseWidth := angleToPulseWidth(int(angle), int(s.maxRotation))
	res := C.set_servo_pulsewidth(s.piID, s.pin, C.uint(pulseWidth))

	s.pulseWidth = pulseWidth

	if res != 0 {
		err := s.pigpioErrors(int(res))
		return err
	}

	utils.SelectContextOrWait(ctx, time.Duration(pulseWidth)*time.Microsecond) // duration of pulswidth send on pin and servo moves

	if !s.holdPos { // the following logic disables a servo once it has reached a position or after a certain amount of time has been reached
		time.Sleep(time.Duration(holdTime)) // time before a stop is sent
		setPos := C.set_servo_pulsewidth(s.piID, s.pin, C.uint(0))
		if setPos < 0 {
			return errors.Errorf("servo on pin %s failed with code %d", s.pinname, setPos)
		}
	}
	return nil
}

// pigpioErrors returns piGPIO specific errors to user
func (s *piPigpioServo) pigpioErrors(res int) error {
	switch {
	case res == C.PI_NOT_SERVO_GPIO:
		return errors.Errorf("gpioservo pin %s is not set up to send and receive pulsewidths", s.pinname)
	case res == C.PI_BAD_PULSEWIDTH:
		return errors.Errorf("gpioservo on pin %s trying to reach out of range position", s.pinname)
	case res == 0:
		return nil
	case res < 0 && res != C.PI_BAD_PULSEWIDTH && res != C.PI_NOT_SERVO_GPIO:
		errMsg := fmt.Sprintf("gpioServo on pin %s failed", s.pinname)
		return rpiutils.ConvertErrorCodeToMessage(res, errMsg)
	default:
		return nil
	}
}

// Position returns the current set angle (degrees) of the servo.
func (s *piPigpioServo) Position(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	res := C.get_servo_pulsewidth(s.piID, s.pin)
	err := s.pigpioErrors(int(res))
	if int(res) != 0 {
		s.res = res
	}
	if err != nil {
		return 0, err
	}
	return uint32(pulseWidthToAngle(int(s.res), int(s.maxRotation))), nil
}

// angleToPulseWidth changes the input angle in degrees
// into the corresponding pulsewidth value in microsecond
func angleToPulseWidth(angle, maxRotation int) int {
	pulseWidth := 500 + (2000 * angle / maxRotation)
	return pulseWidth
}

// pulseWidthToAngle changes the pulsewidth value in microsecond
// to the corresponding angle in degrees
func pulseWidthToAngle(pulseWidth, maxRotation int) int {
	angle := maxRotation * (pulseWidth + 1 - 500) / 2000
	return angle
}

// Stop stops the servo. It is assumed the servo stops immediately.
func (s *piPigpioServo) Stop(ctx context.Context, extra map[string]interface{}) error {
	_, done := s.opMgr.New(ctx)
	defer done()
	getPos := C.set_servo_pulsewidth(s.piID, s.pin, C.uint(0))
	errorCode := int(getPos)
	if errorCode != 0 {
		return rpiutils.ConvertErrorCodeToMessage(errorCode, "gpioServo failed with")
	}
	return nil
}

// IsMoving returns whether the servo is actively moving (or attempting to move) under its own power.
func (s *piPigpioServo) IsMoving(ctx context.Context) (bool, error) {
	err := s.pigpioErrors(int(s.res))
	if err != nil {
		return false, err
	}
	if int(s.res) == 0 {
		return false, nil
	}
	return s.opMgr.OpRunning(), nil
}

// Close function to stop socket connection to pigpio daemon
func (s *piPigpioServo) Close(_ context.Context) error {
	C.pigpio_stop(s.piID)

	return nil
}
