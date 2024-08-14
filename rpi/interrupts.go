package rpi

/*
	This file implements digital interrupt functionality for the Raspberry Pi.
*/

// #include <stdlib.h>
// #include <pigpiod_if2.h>
// #include "pi.h"
// #cgo LDFLAGS: -lpigpiod_if2
import "C"

import (
	"context"
	"fmt"
	"math"

	rpiutils "viamrpi/utils"

	"github.com/pkg/errors"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/logging"
)

type RpiInterrupt struct {
	interrupt  rpiutils.ReconfigurableDigitalInterrupt
	callbackID C.uint // callback ID to close pi callback connection
}

// Function finds an interrupt by its name.
func findInterruptByName(
	name string,
	interrupts map[uint]*RpiInterrupt,
) (rpiutils.ReconfigurableDigitalInterrupt, bool) {
	for _, rpiInterrupt := range interrupts {
		if rpiInterrupt.interrupt.Name() == name {
			return rpiInterrupt.interrupt, true
		}
	}
	return nil, false
}

// reconfigureContext contains the context and state required for reconfiguring interrupts.
type reconfigureContext struct {
	pi  *piPigpio
	ctx context.Context

	// We reuse the old interrupts when possible.
	oldInterrupts map[uint]*RpiInterrupt

	// Like oldInterrupts and oldInterruptsHW, these two will have identical values, mapped to
	// using different keys.
	newInterrupts map[uint]*RpiInterrupt

	interruptsToClose map[rpiutils.ReconfigurableDigitalInterrupt]struct{}
}

// reconfigureInterrupts reconfigures the digital interrupts based on the new configuration provided.
// It reuses existing interrupts when possible and creates new ones if necessary.
func (pi *piPigpio) reconfigureInterrupts(ctx context.Context, cfg *Config) error {
	reconfigCtx := &reconfigureContext{
		pi:            pi,
		ctx:           ctx,
		oldInterrupts: pi.interrupts,
		newInterrupts: make(map[uint]*RpiInterrupt),
	}

	// teardown old interrupts
	for _, interrupt := range reconfigCtx.oldInterrupts {
		if result := C.teardownInterrupt(interrupt.callbackID); result != 0 {
			return rpiutils.ConvertErrorCodeToMessage(int(result), "error")
		}
	}

	// Set new interrupts based on config
	for _, newConfig := range cfg.DigitalInterrupts {
		// check if pin is valid
		bcom, ok := rpiutils.BroadcomPinFromHardwareLabel(newConfig.Pin)
		if !ok {
			return errors.Errorf("no hw mapping for %s", newConfig.Pin)
		}

		// create new interrupt
		if err := reconfigCtx.createNewInterrupt(newConfig, bcom); err != nil {
			return err
		}
	}

	pi.interrupts = reconfigCtx.newInterrupts
	// pi.interruptsHW = reconfigCtx.newInterruptsHW
	return nil
}

// type aliases for initializeInterruptsToClose function
type InterruptMap map[string]rpiutils.ReconfigurableDigitalInterrupt
type InterruptSet map[rpiutils.ReconfigurableDigitalInterrupt]struct{}

// createNewInterrupt creates a new digital interrupt and sets it up with the specified configuration.
func (ctx *reconfigureContext) createNewInterrupt(newConfig rpiutils.DigitalInterruptConfig, bcom uint) error {
	di, err := rpiutils.CreateDigitalInterrupt(newConfig)
	if err != nil {
		return err
	}

	newInterrupt := &RpiInterrupt{
		interrupt: di,
	}

	ctx.newInterrupts[bcom] = newInterrupt

	// returns callback ID on success >= 0
	callbackID := C.setupInterrupt(ctx.pi.piID, C.int(bcom))
	if int(callbackID) < 0 {
		return rpiutils.ConvertErrorCodeToMessage(int(callbackID), "error")
	}

	newInterrupt.callbackID = C.uint(callbackID)

	return nil
}

// DigitalInterruptNames returns the names of all known digital interrupts.
func (pi *piPigpio) DigitalInterruptNames() []string {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	names := []string{}
	for _, rpiInterrupt := range pi.interrupts {
		names = append(names, rpiInterrupt.interrupt.Name())
	}
	return names
}

// DigitalInterruptByName returns a digital interrupt by name.
// NOTE: During board setup, if a digital interrupt has not been created
// for a pin, then this function will attempt to create one with the pin
// number as the name.
func (pi *piPigpio) DigitalInterruptByName(name string) (board.DigitalInterrupt, error) {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	d, ok := findInterruptByName(name, pi.interrupts)
	if !ok {
		var err error
		if bcom, have := rpiutils.BroadcomPinFromHardwareLabel(name); have {
			if d, ok := pi.interrupts[bcom]; ok {
				return d.interrupt, nil
			}
			d, err = rpiutils.CreateDigitalInterrupt(
				rpiutils.DigitalInterruptConfig{
					Name: name,
					Pin:  name,
					Type: "basic",
				})
			if err != nil {
				return nil, err
			}
			callbackID := C.setupInterrupt(pi.piID, C.int(bcom))
			if callbackID < 0 {
				err := rpiutils.ConvertErrorCodeToMessage(int(callbackID), "error")
				return nil, errors.Errorf("Unable to set up interrupt on pin %s: %s", name, err)
			}

			pi.interrupts[bcom] = &RpiInterrupt{
				interrupt:  d,
				callbackID: C.uint(callbackID),
			}
			return d, nil
		}
		return d, fmt.Errorf("interrupt %s does not exist", name)
	}
	return d, nil
}

var (
	lastTick      = uint32(0)
	tickRollevers = 0
)

//export pigpioInterruptCallback
func pigpioInterruptCallback(gpio, level int, rawTick uint32) {
	if rawTick < lastTick {
		tickRollevers++
	}
	lastTick = rawTick

	tick := (uint64(tickRollevers) * uint64(math.MaxUint32)) + uint64(rawTick)

	instanceMu.RLock()
	defer instanceMu.RUnlock()

	// instance has to be initialized before callback can be called
	if instance == nil {
		return
	}
	i := instance.interrupts[uint(gpio)]
	if i == nil {
		logging.Global().Infof("no DigitalInterrupt configured for gpio %d", gpio)
		return
	}
	high := true
	if level == 0 {
		high = false
	}
	switch di := i.interrupt.(type) {
	case *rpiutils.BasicDigitalInterrupt:
		err := rpiutils.Tick(instance.cancelCtx, di, high, tick*1000)
		if err != nil {
			instance.logger.Error(err)
		}
	case *rpiutils.ServoDigitalInterrupt:
		err := rpiutils.ServoTick(instance.cancelCtx, di, high, tick*1000)
		if err != nil {
			instance.logger.Error(err)
		}
	default:
		instance.logger.Error("unknown digital interrupt type")
	}
}
