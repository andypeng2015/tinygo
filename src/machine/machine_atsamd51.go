//go:build (sam && atsamd51) || (sam && atsame5x)

// Peripheral abstraction layer for the atsamd51.
//
// Datasheet:
// http://ww1.microchip.com/downloads/en/DeviceDoc/60001507C.pdf
package machine

import (
	"device/arm"
	"device/sam"
	"errors"
	"internal/binary"
	"runtime/interrupt"
	"unsafe"
)

const deviceName = sam.Device

// DS60001507, Section 9.6: Serial Number
var deviceIDAddr = []uintptr{0x008061FC, 0x00806010, 0x00806014, 0x00806018}

func CPUFrequency() uint32 {
	return 120000000
}

const (
	PinAnalog        PinMode = 1
	PinSERCOM        PinMode = 2
	PinSERCOMAlt     PinMode = 3
	PinTimer         PinMode = 4
	PinTimerAlt      PinMode = 5
	PinTCCPDEC       PinMode = 6
	PinCom           PinMode = 7
	PinSDHC          PinMode = 8
	PinI2S           PinMode = 9
	PinPCC           PinMode = 10
	PinGMAC          PinMode = 11
	PinACCLK         PinMode = 12
	PinCCL           PinMode = 13
	PinDigital       PinMode = 14
	PinInput         PinMode = 15
	PinInputPullup   PinMode = 16
	PinOutput        PinMode = 17
	PinTCCE          PinMode = PinTimer
	PinTCCF          PinMode = PinTimerAlt
	PinTCCG          PinMode = PinTCCPDEC
	PinInputPulldown PinMode = 18
	PinCAN           PinMode = 19
	PinCAN0          PinMode = PinSDHC
	PinCAN1          PinMode = PinCom
)

type PinChange uint8

// Pin change interrupt constants for SetInterrupt.
const (
	PinRising  PinChange = sam.EIC_CONFIG_SENSE0_RISE
	PinFalling PinChange = sam.EIC_CONFIG_SENSE0_FALL
	PinToggle  PinChange = sam.EIC_CONFIG_SENSE0_BOTH
)

// Callbacks to be called for pins configured with SetInterrupt. Unfortunately,
// we also need to keep track of which interrupt channel is used by which pin,
// as the only alternative would be iterating through all pins.
//
// We're using the magic constant 16 here because the SAM D21 has 16 interrupt
// channels configurable for pins.
var (
	interruptPins [16]Pin // warning: the value is invalid when pinCallbacks[i] is not set!
	pinCallbacks  [16]func(Pin)
)

// Hardware pins
const (
	PA00 Pin = 0
	PA01 Pin = 1
	PA02 Pin = 2
	PA03 Pin = 3
	PA04 Pin = 4
	PA05 Pin = 5
	PA06 Pin = 6
	PA07 Pin = 7
	PA08 Pin = 8  // peripherals: TCC0 channel 0, TCC1 channel 4
	PA09 Pin = 9  // peripherals: TCC0 channel 1, TCC1 channel 5
	PA10 Pin = 10 // peripherals: TCC0 channel 2, TCC1 channel 6
	PA11 Pin = 11 // peripherals: TCC0 channel 3, TCC1 channel 7
	PA12 Pin = 12 // peripherals: TCC0 channel 6, TCC1 channel 2
	PA13 Pin = 13 // peripherals: TCC0 channel 7, TCC1 channel 3
	PA14 Pin = 14 // peripherals: TCC2 channel 0, TCC1 channel 2
	PA15 Pin = 15 // peripherals: TCC2 channel 1, TCC1 channel 3
	PA16 Pin = 16 // peripherals: TCC1 channel 0, TCC0 channel 4
	PA17 Pin = 17 // peripherals: TCC1 channel 1, TCC0 channel 5
	PA18 Pin = 18 // peripherals: TCC1 channel 2, TCC0 channel 6
	PA19 Pin = 19 // peripherals: TCC1 channel 3, TCC0 channel 7
	PA20 Pin = 20 // peripherals: TCC1 channel 4, TCC0 channel 0
	PA21 Pin = 21 // peripherals: TCC1 channel 5, TCC0 channel 1
	PA22 Pin = 22 // peripherals: TCC1 channel 6, TCC0 channel 2
	PA23 Pin = 23 // peripherals: TCC1 channel 7, TCC0 channel 3
	PA24 Pin = 24 // peripherals: TCC2 channel 2
	PA25 Pin = 25 // peripherals: TCC2 channel 3
	PA26 Pin = 26
	PA27 Pin = 27
	PA28 Pin = 28
	PA29 Pin = 29
	PA30 Pin = 30 // peripherals: TCC2 channel 0
	PA31 Pin = 31 // peripherals: TCC2 channel 1
	PB00 Pin = 32
	PB01 Pin = 33
	PB02 Pin = 34 // peripherals: TCC2 channel 2
	PB03 Pin = 35 // peripherals: TCC2 channel 3
	PB04 Pin = 36
	PB05 Pin = 37
	PB06 Pin = 38
	PB07 Pin = 39
	PB08 Pin = 40
	PB09 Pin = 41
	PB10 Pin = 42 // peripherals: TCC0 channel 4, TCC1 channel 0
	PB11 Pin = 43 // peripherals: TCC0 channel 5, TCC1 channel 1
	PB12 Pin = 44 // peripherals: TCC3 channel 0, TCC0 channel 0
	PB13 Pin = 45 // peripherals: TCC3 channel 1, TCC0 channel 1
	PB14 Pin = 46 // peripherals: TCC4 channel 0, TCC0 channel 2
	PB15 Pin = 47 // peripherals: TCC4 channel 1, TCC0 channel 3
	PB16 Pin = 48 // peripherals: TCC3 channel 0, TCC0 channel 4
	PB17 Pin = 49 // peripherals: TCC3 channel 1, TCC0 channel 5
	PB18 Pin = 50 // peripherals: TCC1 channel 0
	PB19 Pin = 51 // peripherals: TCC1 channel 1
	PB20 Pin = 52 // peripherals: TCC1 channel 2
	PB21 Pin = 53 // peripherals: TCC1 channel 3
	PB22 Pin = 54
	PB23 Pin = 55
	PB24 Pin = 56
	PB25 Pin = 57
	PB26 Pin = 58 // peripherals: TCC1 channel 2
	PB27 Pin = 59 // peripherals: TCC1 channel 3
	PB28 Pin = 60 // peripherals: TCC1 channel 4
	PB29 Pin = 61 // peripherals: TCC1 channel 5
	PB30 Pin = 62 // peripherals: TCC4 channel 0, TCC0 channel 6
	PB31 Pin = 63 // peripherals: TCC4 channel 1, TCC0 channel 7
	PC00 Pin = 64
	PC01 Pin = 65
	PC02 Pin = 66
	PC03 Pin = 67
	PC04 Pin = 68 // peripherals: TCC0 channel 0
	PC05 Pin = 69 // peripherals: TCC0 channel 1
	PC06 Pin = 70
	PC07 Pin = 71
	PC08 Pin = 72
	PC09 Pin = 73
	PC10 Pin = 74 // peripherals: TCC0 channel 0, TCC1 channel 4
	PC11 Pin = 75 // peripherals: TCC0 channel 1, TCC1 channel 5
	PC12 Pin = 76 // peripherals: TCC0 channel 2, TCC1 channel 6
	PC13 Pin = 77 // peripherals: TCC0 channel 3, TCC1 channel 7
	PC14 Pin = 78 // peripherals: TCC0 channel 4, TCC1 channel 0
	PC15 Pin = 79 // peripherals: TCC0 channel 5, TCC1 channel 1
	PC16 Pin = 80 // peripherals: TCC0 channel 0
	PC17 Pin = 81 // peripherals: TCC0 channel 1
	PC18 Pin = 82 // peripherals: TCC0 channel 2
	PC19 Pin = 83 // peripherals: TCC0 channel 3
	PC20 Pin = 84 // peripherals: TCC0 channel 4
	PC21 Pin = 85 // peripherals: TCC0 channel 5
	PC22 Pin = 86 // peripherals: TCC0 channel 6
	PC23 Pin = 87 // peripherals: TCC0 channel 7
	PC24 Pin = 88
	PC25 Pin = 89
	PC26 Pin = 90
	PC27 Pin = 91
	PC28 Pin = 92
	PC29 Pin = 93
	PC30 Pin = 94
	PC31 Pin = 95
	PD00 Pin = 96
	PD01 Pin = 97
	PD02 Pin = 98
	PD03 Pin = 99
	PD04 Pin = 100
	PD05 Pin = 101
	PD06 Pin = 102
	PD07 Pin = 103
	PD08 Pin = 104 // peripherals: TCC0 channel 1
	PD09 Pin = 105 // peripherals: TCC0 channel 2
	PD10 Pin = 106 // peripherals: TCC0 channel 3
	PD11 Pin = 107 // peripherals: TCC0 channel 4
	PD12 Pin = 108 // peripherals: TCC0 channel 5
	PD13 Pin = 109 // peripherals: TCC0 channel 6
	PD14 Pin = 110
	PD15 Pin = 111
	PD16 Pin = 112
	PD17 Pin = 113
	PD18 Pin = 114
	PD19 Pin = 115
	PD20 Pin = 116 // peripherals: TCC1 channel 0
	PD21 Pin = 117 // peripherals: TCC1 channel 1
	PD22 Pin = 118
	PD23 Pin = 119
	PD24 Pin = 120
	PD25 Pin = 121
	PD26 Pin = 122
	PD27 Pin = 123
	PD28 Pin = 124
	PD29 Pin = 125
	PD30 Pin = 126
	PD31 Pin = 127
)

const (
	pinPadMapSERCOM0Pad0 uint16 = 0x1000
	pinPadMapSERCOM1Pad0 uint16 = 0x2000
	pinPadMapSERCOM2Pad0 uint16 = 0x3000
	pinPadMapSERCOM3Pad0 uint16 = 0x4000
	pinPadMapSERCOM4Pad0 uint16 = 0x5000
	pinPadMapSERCOM5Pad0 uint16 = 0x6000
	pinPadMapSERCOM6Pad0 uint16 = 0x7000
	pinPadMapSERCOM7Pad0 uint16 = 0x8000
	pinPadMapSERCOM0Pad2 uint16 = 0x1200
	pinPadMapSERCOM1Pad2 uint16 = 0x2200
	pinPadMapSERCOM2Pad2 uint16 = 0x3200
	pinPadMapSERCOM3Pad2 uint16 = 0x4200
	pinPadMapSERCOM4Pad2 uint16 = 0x5200
	pinPadMapSERCOM5Pad2 uint16 = 0x6200
	pinPadMapSERCOM6Pad2 uint16 = 0x7200
	pinPadMapSERCOM7Pad2 uint16 = 0x8200

	pinPadMapSERCOM0AltPad0 uint16 = 0x0010
	pinPadMapSERCOM1AltPad0 uint16 = 0x0020
	pinPadMapSERCOM2AltPad0 uint16 = 0x0030
	pinPadMapSERCOM3AltPad0 uint16 = 0x0040
	pinPadMapSERCOM4AltPad0 uint16 = 0x0050
	pinPadMapSERCOM5AltPad0 uint16 = 0x0060
	pinPadMapSERCOM6AltPad0 uint16 = 0x0070
	pinPadMapSERCOM7AltPad0 uint16 = 0x0080
	pinPadMapSERCOM0AltPad1 uint16 = 0x0011
	pinPadMapSERCOM1AltPad1 uint16 = 0x0021
	pinPadMapSERCOM2AltPad1 uint16 = 0x0031
	pinPadMapSERCOM3AltPad1 uint16 = 0x0041
	pinPadMapSERCOM4AltPad1 uint16 = 0x0051
	pinPadMapSERCOM5AltPad1 uint16 = 0x0061
	pinPadMapSERCOM6AltPad1 uint16 = 0x0071
	pinPadMapSERCOM7AltPad1 uint16 = 0x0081
	pinPadMapSERCOM0AltPad2 uint16 = 0x0012
	pinPadMapSERCOM1AltPad2 uint16 = 0x0022
	pinPadMapSERCOM2AltPad2 uint16 = 0x0032
	pinPadMapSERCOM3AltPad2 uint16 = 0x0042
	pinPadMapSERCOM4AltPad2 uint16 = 0x0052
	pinPadMapSERCOM5AltPad2 uint16 = 0x0062
	pinPadMapSERCOM6AltPad2 uint16 = 0x0072
	pinPadMapSERCOM7AltPad2 uint16 = 0x0082
)

// pinPadMapping lists which pins have which SERCOMs attached to them.
// The encoding is rather dense, with each uint16 encoding two pins and both
// SERCOM and SERCOM-ALT.
//
// Observations:
//   - There are eight SERCOMs. Those SERCOM numbers can be encoded in 4 bits.
//   - Even pad numbers are usually on even pins, and odd pad numbers are usually
//     on odd pins. The exception is SERCOM-ALT, which sometimes swaps pad 0 and 1.
//     With that, there is still an invariant that the pad number for an odd pin is
//     the pad number for the corresponding even pin with the low bit toggled.
//   - Pin pads come in pairs. If PA00 has pad 0, then PA01 has pad 1.
//
// With this information, we can encode SERCOM pin/pad numbers much more
// efficiently. Due to pads coming in pairs, we can ignore half the pins: the
// information for an odd pin can be calculated easily from the preceding even
// pin.
//
// Each word below is split in two bytes. The 8 high bytes are for SERCOM and
// the 8 low bits are for SERCOM-ALT. Of each byte, the 4 high bits encode the
// SERCOM + 1 while the two low bits encodes the pad number (the pad number for
// the odd pin can be trivially calculated by toggling the low bit of the pad
// number). It encodes SERCOM + 1 instead of just the SERCOM number, to make it
// easy to check whether a nibble is set at all.
//
// Datasheet: http://ww1.microchip.com/downloads/en/DeviceDoc/60001507E.pdf
var pinPadMapping = [64]uint16{
	// page 32
	PA00 / 2: 0 | pinPadMapSERCOM1AltPad0,

	// page 33
	PB08 / 2: 0 | pinPadMapSERCOM4AltPad0,
	PA04 / 2: 0 | pinPadMapSERCOM0AltPad0,
	PA06 / 2: 0 | pinPadMapSERCOM0AltPad2,
	PC04 / 2: pinPadMapSERCOM6Pad0 | 0,
	PC06 / 2: pinPadMapSERCOM6Pad2 | 0,
	PA08 / 2: pinPadMapSERCOM0Pad0 | pinPadMapSERCOM2AltPad1,
	PA10 / 2: pinPadMapSERCOM0Pad2 | pinPadMapSERCOM2AltPad2,
	PB10 / 2: 0 | pinPadMapSERCOM4AltPad2,
	PB12 / 2: pinPadMapSERCOM4Pad0 | 0,
	PB14 / 2: pinPadMapSERCOM4Pad2 | 0,
	PD08 / 2: pinPadMapSERCOM7Pad0 | pinPadMapSERCOM6AltPad1,
	PD10 / 2: pinPadMapSERCOM7Pad2 | pinPadMapSERCOM6AltPad2,
	PC10 / 2: pinPadMapSERCOM6Pad2 | pinPadMapSERCOM7AltPad2,

	// page 34
	PC12 / 2: pinPadMapSERCOM7Pad0 | pinPadMapSERCOM6AltPad1,
	PC14 / 2: pinPadMapSERCOM7Pad2 | pinPadMapSERCOM6AltPad2,
	PA12 / 2: pinPadMapSERCOM2Pad0 | pinPadMapSERCOM4AltPad1,
	PA14 / 2: pinPadMapSERCOM2Pad2 | pinPadMapSERCOM4AltPad2,
	PA16 / 2: pinPadMapSERCOM1Pad0 | pinPadMapSERCOM3AltPad1,
	PA18 / 2: pinPadMapSERCOM1Pad2 | pinPadMapSERCOM3AltPad2,
	PC16 / 2: pinPadMapSERCOM6Pad0 | pinPadMapSERCOM0AltPad1,
	PC18 / 2: pinPadMapSERCOM6Pad2 | pinPadMapSERCOM0AltPad2,
	PC22 / 2: pinPadMapSERCOM1Pad0 | pinPadMapSERCOM3AltPad1,
	PD20 / 2: pinPadMapSERCOM1Pad2 | pinPadMapSERCOM3AltPad2,
	PB16 / 2: pinPadMapSERCOM5Pad0 | 0,
	PB18 / 2: pinPadMapSERCOM5Pad2 | pinPadMapSERCOM7AltPad2,

	// page 35
	PB20 / 2: pinPadMapSERCOM3Pad0 | pinPadMapSERCOM7AltPad1,
	PA20 / 2: pinPadMapSERCOM5Pad2 | pinPadMapSERCOM3AltPad2,
	PA22 / 2: pinPadMapSERCOM3Pad0 | pinPadMapSERCOM5AltPad1,
	PA24 / 2: pinPadMapSERCOM3Pad2 | pinPadMapSERCOM5AltPad2,
	PB22 / 2: pinPadMapSERCOM1Pad2 | pinPadMapSERCOM5AltPad2,
	PB24 / 2: pinPadMapSERCOM0Pad0 | pinPadMapSERCOM2AltPad1,
	PB26 / 2: pinPadMapSERCOM2Pad0 | pinPadMapSERCOM4AltPad1,
	PB28 / 2: pinPadMapSERCOM2Pad2 | pinPadMapSERCOM4AltPad2,
	PC24 / 2: pinPadMapSERCOM0Pad2 | pinPadMapSERCOM2AltPad2,
	//PC26 / 2: pinPadMapSERCOM1Pad1 | 0, // note: PC26 doesn't support SERCOM, but PC27 does
	//PC28 / 2: pinPadMapSERCOM1Pad1 | 0, // note: PC29 doesn't exist in the datasheet?
	PA30 / 2: 0 | pinPadMapSERCOM1AltPad2,

	// page 36
	PB30 / 2: 0 | pinPadMapSERCOM5AltPad1,
	PB00 / 2: 0 | pinPadMapSERCOM5AltPad2,
	PB02 / 2: 0 | pinPadMapSERCOM5AltPad0,
}

// findPinPadMapping looks up the pad number and the pinmode for a given pin and
// SERCOM number. The result can either be SERCOM, SERCOM-ALT, or "not found"
// (indicated by returning ok=false). The pad number is returned to calculate
// the DOPO/DIPO bitfields of the various serial peripherals.
func findPinPadMapping(sercom uint8, pin Pin) (pinMode PinMode, pad uint32, ok bool) {
	if int(pin)/2 >= len(pinPadMapping) {
		// This is probably NoPin, for which no mapping is available.
		return
	}

	bytes := pinPadMapping[pin/2]
	upper := byte(bytes >> 8)
	lower := byte(bytes & 0xff)

	if upper != 0 {
		// SERCOM
		if (upper>>4)-1 == sercom {
			pinMode = PinSERCOM
			pad |= uint32(upper % 4)
			ok = true
		}
	}
	if lower != 0 {
		// SERCOM-ALT
		if (lower>>4)-1 == sercom {
			pinMode = PinSERCOMAlt
			pad |= uint32(lower % 4)
			ok = true
		}
	}

	if ok {
		// If the pin is uneven, toggle the lowest bit of the pad number.
		if pin&1 != 0 {
			pad ^= 1
		}
	}
	return
}

// SetInterrupt sets an interrupt to be executed when a particular pin changes
// state. The pin should already be configured as an input, including a pull up
// or down if no external pull is provided.
//
// This call will replace a previously set callback on this pin. You can pass a
// nil func to unset the pin change interrupt. If you do so, the change
// parameter is ignored and can be set to any value (such as 0).
func (p Pin) SetInterrupt(change PinChange, callback func(Pin)) error {
	// Most pins follow a common pattern where the EXTINT value is the pin
	// number modulo 16. However, there are a few exceptions, as you can see
	// below.
	extint := uint8(0)

	switch p {
	case PA08:
		// Connected to NMI. This is not currently supported.
		return ErrInvalidInputPin
	case PB26:
		extint = 12
	case PB27:
		extint = 13
	case PB28:
		extint = 14
	case PB29:
		extint = 15
	case PC07:
		extint = 9
	case PD08:
		extint = 3
	case PD09:
		extint = 4
	case PD10:
		extint = 5
	case PD11:
		extint = 6
	case PD12:
		extint = 7
	case PD20:
		extint = 10
	case PD21:
		extint = 11
	default:
		// All other pins follow a normal pattern.
		extint = uint8(p) % 16
	}

	if callback == nil {
		// Disable this pin interrupt (if it was enabled).
		sam.EIC.INTENCLR.Set(1 << extint)
		if pinCallbacks[extint] != nil {
			pinCallbacks[extint] = nil
		}
		return nil
	}

	if pinCallbacks[extint] != nil {
		// The pin was already configured.
		// To properly re-configure a pin, unset it first and set a new
		// configuration.
		return ErrNoPinChangeChannel
	}
	pinCallbacks[extint] = callback
	interruptPins[extint] = p

	if !sam.EIC.CTRLA.HasBits(sam.EIC_CTRLA_ENABLE) {
		// EIC peripheral has not yet been initialized. Initialize it now.

		// The EIC needs two clocks: CLK_EIC_APB and GCLK_EIC. CLK_EIC_APB is
		// enabled by default, so doesn't have to be re-enabled. The other is
		// required for detecting edges and must be enabled manually.
		sam.GCLK.PCHCTRL[4].Set((sam.GCLK_PCHCTRL_GEN_GCLK0 << sam.GCLK_PCHCTRL_GEN_Pos) | sam.GCLK_PCHCTRL_CHEN)

		// should not be necessary (CLKCTRL is not synchronized)
		for sam.GCLK.SYNCBUSY.HasBits(sam.GCLK_SYNCBUSY_GENCTRL_GCLK0 << sam.GCLK_SYNCBUSY_GENCTRL_Pos) {
		}
	}

	// CONFIG register is enable-protected, so disable EIC.
	sam.EIC.CTRLA.ClearBits(sam.EIC_CTRLA_ENABLE)

	// Configure this pin. Set the 4 bits of the EIC.CONFIGx register to the
	// sense value (filter bit set to 0, sense bits set to the change value).
	addr := &sam.EIC.CONFIG[0]
	if extint >= 8 {
		addr = &sam.EIC.CONFIG[1]
	}
	pos := (extint % 8) * 4 // bit position in register
	addr.ReplaceBits(uint32(change), 0xf, pos)

	// Enable external interrupt for this pin.
	sam.EIC.INTENSET.Set(1 << extint)

	sam.EIC.CTRLA.Set(sam.EIC_CTRLA_ENABLE)
	for sam.EIC.SYNCBUSY.HasBits(sam.EIC_SYNCBUSY_ENABLE) {
	}

	// Set the PMUXEN flag, while keeping the INEN and PULLEN flags (if they
	// were set before). This avoids clearing the pin pull mode while
	// configuring the pin interrupt.
	p.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN | (p.getPinCfg() & (sam.PORT_GROUP_PINCFG_INEN | sam.PORT_GROUP_PINCFG_PULLEN)))
	if p&1 > 0 {
		// odd pin, so save the even pins
		val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
		p.setPMux(val | (0 << sam.PORT_GROUP_PMUX_PMUXO_Pos))
	} else {
		// even pin, so save the odd pins
		val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
		p.setPMux(val | (0 << sam.PORT_GROUP_PMUX_PMUXE_Pos))
	}

	handleEICInterrupt := func(interrupt.Interrupt) {
		flags := sam.EIC.INTFLAG.Get()
		sam.EIC.INTFLAG.Set(flags)      // clear interrupt
		for i := uint(0); i < 16; i++ { // there are 16 channels
			if flags&(1<<i) != 0 {
				pinCallbacks[i](interruptPins[i])
			}
		}
	}
	switch extint {
	case 0:
		interrupt.New(sam.IRQ_EIC_EXTINT_0, handleEICInterrupt).Enable()
	case 1:
		interrupt.New(sam.IRQ_EIC_EXTINT_1, handleEICInterrupt).Enable()
	case 2:
		interrupt.New(sam.IRQ_EIC_EXTINT_2, handleEICInterrupt).Enable()
	case 3:
		interrupt.New(sam.IRQ_EIC_EXTINT_3, handleEICInterrupt).Enable()
	case 4:
		interrupt.New(sam.IRQ_EIC_EXTINT_4, handleEICInterrupt).Enable()
	case 5:
		interrupt.New(sam.IRQ_EIC_EXTINT_5, handleEICInterrupt).Enable()
	case 6:
		interrupt.New(sam.IRQ_EIC_EXTINT_6, handleEICInterrupt).Enable()
	case 7:
		interrupt.New(sam.IRQ_EIC_EXTINT_7, handleEICInterrupt).Enable()
	case 8:
		interrupt.New(sam.IRQ_EIC_EXTINT_8, handleEICInterrupt).Enable()
	case 9:
		interrupt.New(sam.IRQ_EIC_EXTINT_9, handleEICInterrupt).Enable()
	case 10:
		interrupt.New(sam.IRQ_EIC_EXTINT_10, handleEICInterrupt).Enable()
	case 11:
		interrupt.New(sam.IRQ_EIC_EXTINT_11, handleEICInterrupt).Enable()
	case 12:
		interrupt.New(sam.IRQ_EIC_EXTINT_12, handleEICInterrupt).Enable()
	case 13:
		interrupt.New(sam.IRQ_EIC_EXTINT_13, handleEICInterrupt).Enable()
	case 14:
		interrupt.New(sam.IRQ_EIC_EXTINT_14, handleEICInterrupt).Enable()
	case 15:
		interrupt.New(sam.IRQ_EIC_EXTINT_15, handleEICInterrupt).Enable()
	}

	return nil
}

// Return the register and mask to enable a given GPIO pin. This can be used to
// implement bit-banged drivers.
func (p Pin) PortMaskSet() (*uint32, uint32) {
	group, pin_in_group := p.getPinGrouping()
	return &sam.PORT.GROUP[group].OUTSET.Reg, 1 << pin_in_group
}

// Return the register and mask to disable a given port. This can be used to
// implement bit-banged drivers.
func (p Pin) PortMaskClear() (*uint32, uint32) {
	group, pin_in_group := p.getPinGrouping()
	return &sam.PORT.GROUP[group].OUTCLR.Reg, 1 << pin_in_group
}

// Set the pin to high or low.
// Warning: only use this on an output pin!
func (p Pin) Set(high bool) {
	group, pin_in_group := p.getPinGrouping()
	if high {
		sam.PORT.GROUP[group].OUTSET.Set(1 << pin_in_group)
	} else {
		sam.PORT.GROUP[group].OUTCLR.Set(1 << pin_in_group)
	}
}

// Get returns the current value of a GPIO pin when configured as an input or as
// an output.
func (p Pin) Get() bool {
	group, pin_in_group := p.getPinGrouping()
	return (sam.PORT.GROUP[group].IN.Get()>>pin_in_group)&1 > 0
}

// Toggle switches an output pin from low to high or from high to low.
// Warning: only use this on an output pin!
func (p Pin) Toggle() {
	group, pin_in_group := p.getPinGrouping()
	sam.PORT.GROUP[group].OUTTGL.Set(1 << pin_in_group)
}

// Configure this pin with the given configuration.
func (p Pin) Configure(config PinConfig) {
	group, pin_in_group := p.getPinGrouping()
	switch config.Mode {
	case PinOutput:
		sam.PORT.GROUP[group].DIRSET.Set(1 << pin_in_group)
		// output is also set to input enable so pin can read back its own value
		p.setPinCfg(sam.PORT_GROUP_PINCFG_INEN)

	case PinInput:
		sam.PORT.GROUP[group].DIRCLR.Set(1 << pin_in_group)
		p.setPinCfg(sam.PORT_GROUP_PINCFG_INEN)

	case PinInputPulldown:
		sam.PORT.GROUP[group].DIRCLR.Set(1 << pin_in_group)
		sam.PORT.GROUP[group].OUTCLR.Set(1 << pin_in_group)
		p.setPinCfg(sam.PORT_GROUP_PINCFG_INEN | sam.PORT_GROUP_PINCFG_PULLEN)

	case PinInputPullup:
		sam.PORT.GROUP[group].DIRCLR.Set(1 << pin_in_group)
		sam.PORT.GROUP[group].OUTSET.Set(1 << pin_in_group)
		p.setPinCfg(sam.PORT_GROUP_PINCFG_INEN | sam.PORT_GROUP_PINCFG_PULLEN)

	case PinSERCOM:
		if p&1 > 0 {
			// odd pin, so save the even pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
			p.setPMux(val | (uint8(PinSERCOM) << sam.PORT_GROUP_PMUX_PMUXO_Pos))
		} else {
			// even pin, so save the odd pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
			p.setPMux(val | (uint8(PinSERCOM) << sam.PORT_GROUP_PMUX_PMUXE_Pos))
		}
		// enable port config
		p.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN | sam.PORT_GROUP_PINCFG_DRVSTR | sam.PORT_GROUP_PINCFG_INEN)

	case PinSERCOMAlt:
		if p&1 > 0 {
			// odd pin, so save the even pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
			p.setPMux(val | (uint8(PinSERCOMAlt) << sam.PORT_GROUP_PMUX_PMUXO_Pos))
		} else {
			// even pin, so save the odd pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
			p.setPMux(val | (uint8(PinSERCOMAlt) << sam.PORT_GROUP_PMUX_PMUXE_Pos))
		}
		// enable port config
		p.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN | sam.PORT_GROUP_PINCFG_DRVSTR)

	case PinCom:
		if p&1 > 0 {
			// odd pin, so save the even pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
			p.setPMux(val | (uint8(PinCom) << sam.PORT_GROUP_PMUX_PMUXO_Pos))
		} else {
			// even pin, so save the odd pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
			p.setPMux(val | (uint8(PinCom) << sam.PORT_GROUP_PMUX_PMUXE_Pos))
		}
		// enable port config
		p.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN)
	case PinAnalog:
		if p&1 > 0 {
			// odd pin, so save the even pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
			p.setPMux(val | (uint8(PinAnalog) << sam.PORT_GROUP_PMUX_PMUXO_Pos))
		} else {
			// even pin, so save the odd pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
			p.setPMux(val | (uint8(PinAnalog) << sam.PORT_GROUP_PMUX_PMUXE_Pos))
		}
		// enable port config
		p.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN | sam.PORT_GROUP_PINCFG_DRVSTR)
	case PinSDHC:
		if p&1 > 0 {
			// odd pin, so save the even pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
			p.setPMux(val | (uint8(PinSDHC) << sam.PORT_GROUP_PMUX_PMUXO_Pos))
		} else {
			// even pin, so save the odd pins
			val := p.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
			p.setPMux(val | (uint8(PinSDHC) << sam.PORT_GROUP_PMUX_PMUXE_Pos))
		}
		// enable port config
		p.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN)
	}
}

// getPMux returns the value for the correct PMUX register for this pin.
func (p Pin) getPMux() uint8 {
	group, pin_in_group := p.getPinGrouping()
	return sam.PORT.GROUP[group].PMUX[pin_in_group>>1].Get()
}

// setPMux sets the value for the correct PMUX register for this pin.
func (p Pin) setPMux(val uint8) {
	group, pin_in_group := p.getPinGrouping()
	sam.PORT.GROUP[group].PMUX[pin_in_group>>1].Set(val)
}

// getPinCfg returns the value for the correct PINCFG register for this pin.
func (p Pin) getPinCfg() uint8 {
	group, pin_in_group := p.getPinGrouping()
	return sam.PORT.GROUP[group].PINCFG[pin_in_group].Get()
}

// setPinCfg sets the value for the correct PINCFG register for this pin.
func (p Pin) setPinCfg(val uint8) {
	group, pin_in_group := p.getPinGrouping()
	sam.PORT.GROUP[group].PINCFG[pin_in_group].Set(val)
}

// getPinGrouping calculates the gpio group and pin id from the pin number.
// Pins are split into groups of 32, and each group has its own set of
// control registers.
func (p Pin) getPinGrouping() (uint8, uint8) {
	group := uint8(p) >> 5
	pin_in_group := uint8(p) & 0x1f
	return group, pin_in_group
}

// InitADC initializes the ADC.
func InitADC() {
	// ADC Bias Calibration
	// NVMCTRL_SW0 0x00800080
	// #define ADC0_FUSES_BIASCOMP_ADDR    NVMCTRL_SW0
	// #define ADC0_FUSES_BIASCOMP_Pos     2            /**< \brief (NVMCTRL_SW0) ADC Comparator Scaling */
	// #define ADC0_FUSES_BIASCOMP_Msk     (_Ul(0x7) << ADC0_FUSES_BIASCOMP_Pos)
	// #define ADC0_FUSES_BIASCOMP(value)  (ADC0_FUSES_BIASCOMP_Msk & ((value) << ADC0_FUSES_BIASCOMP_Pos))

	// #define ADC0_FUSES_BIASR2R_ADDR     NVMCTRL_SW0
	// #define ADC0_FUSES_BIASR2R_Pos      8            /**< \brief (NVMCTRL_SW0) ADC Bias R2R ampli scaling */
	// #define ADC0_FUSES_BIASR2R_Msk      (_Ul(0x7) << ADC0_FUSES_BIASR2R_Pos)
	// #define ADC0_FUSES_BIASR2R(value)   (ADC0_FUSES_BIASR2R_Msk & ((value) << ADC0_FUSES_BIASR2R_Pos))

	// #define ADC0_FUSES_BIASREFBUF_ADDR  NVMCTRL_SW0
	// #define ADC0_FUSES_BIASREFBUF_Pos   5            /**< \brief (NVMCTRL_SW0) ADC Bias Reference Buffer Scaling */
	// #define ADC0_FUSES_BIASREFBUF_Msk   (_Ul(0x7) << ADC0_FUSES_BIASREFBUF_Pos)
	// #define ADC0_FUSES_BIASREFBUF(value) (ADC0_FUSES_BIASREFBUF_Msk & ((value) << ADC0_FUSES_BIASREFBUF_Pos))

	// #define ADC1_FUSES_BIASCOMP_ADDR    NVMCTRL_SW0
	// #define ADC1_FUSES_BIASCOMP_Pos     16           /**< \brief (NVMCTRL_SW0) ADC Comparator Scaling */
	// #define ADC1_FUSES_BIASCOMP_Msk     (_Ul(0x7) << ADC1_FUSES_BIASCOMP_Pos)
	// #define ADC1_FUSES_BIASCOMP(value)  (ADC1_FUSES_BIASCOMP_Msk & ((value) << ADC1_FUSES_BIASCOMP_Pos))

	// #define ADC1_FUSES_BIASR2R_ADDR     NVMCTRL_SW0
	// #define ADC1_FUSES_BIASR2R_Pos      22           /**< \brief (NVMCTRL_SW0) ADC Bias R2R ampli scaling */
	// #define ADC1_FUSES_BIASR2R_Msk      (_Ul(0x7) << ADC1_FUSES_BIASR2R_Pos)
	// #define ADC1_FUSES_BIASR2R(value)   (ADC1_FUSES_BIASR2R_Msk & ((value) << ADC1_FUSES_BIASR2R_Pos))

	// #define ADC1_FUSES_BIASREFBUF_ADDR  NVMCTRL_SW0
	// #define ADC1_FUSES_BIASREFBUF_Pos   19           /**< \brief (NVMCTRL_SW0) ADC Bias Reference Buffer Scaling */
	// #define ADC1_FUSES_BIASREFBUF_Msk   (_Ul(0x7) << ADC1_FUSES_BIASREFBUF_Pos)
	// #define ADC1_FUSES_BIASREFBUF(value) (ADC1_FUSES_BIASREFBUF_Msk & ((value) << ADC1_FUSES_BIASREFBUF_Pos))

	adcFuse := *(*uint32)(unsafe.Pointer(uintptr(0x00800080)))

	// uint32_t biascomp = (*((uint32_t *)ADC0_FUSES_BIASCOMP_ADDR) & ADC0_FUSES_BIASCOMP_Msk) >> ADC0_FUSES_BIASCOMP_Pos;
	biascomp := (adcFuse & uint32(0x7<<2)) //>> 2

	// uint32_t biasr2r = (*((uint32_t *)ADC0_FUSES_BIASR2R_ADDR) & ADC0_FUSES_BIASR2R_Msk) >> ADC0_FUSES_BIASR2R_Pos;
	biasr2r := (adcFuse & uint32(0x7<<8)) //>> 8

	// uint32_t biasref = (*((uint32_t *)ADC0_FUSES_BIASREFBUF_ADDR) & ADC0_FUSES_BIASREFBUF_Msk) >> ADC0_FUSES_BIASREFBUF_Pos;
	biasref := (adcFuse & uint32(0x7<<5)) //>> 5

	// calibrate ADC0
	sam.ADC0.CALIB.Set(uint16(biascomp | biasr2r | biasref))

	// biascomp = (*((uint32_t *)ADC1_FUSES_BIASCOMP_ADDR) & ADC1_FUSES_BIASCOMP_Msk) >> ADC1_FUSES_BIASCOMP_Pos;
	biascomp = (adcFuse & uint32(0x7<<16)) //>> 16

	// biasr2r = (*((uint32_t *)ADC1_FUSES_BIASR2R_ADDR) & ADC1_FUSES_BIASR2R_Msk) >> ADC1_FUSES_BIASR2R_Pos;
	biasr2r = (adcFuse & uint32(0x7<<22)) //>> 22

	// biasref = (*((uint32_t *)ADC1_FUSES_BIASREFBUF_ADDR) & ADC1_FUSES_BIASREFBUF_Msk) >> ADC1_FUSES_BIASREFBUF_Pos;
	biasref = (adcFuse & uint32(0x7<<19)) //>> 19

	// calibrate ADC1
	sam.ADC1.CALIB.Set(uint16((biascomp | biasr2r | biasref) >> 16))
}

// Configure configures a ADCPin to be able to be used to read data.
func (a ADC) Configure(config ADCConfig) {

	for _, adc := range []*sam.ADC_Type{sam.ADC0, sam.ADC1} {

		for adc.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_CTRLB) {
		} // wait for sync

		// Averaging (see datasheet table in AVGCTRL register description)
		var resolution uint32 = sam.ADC_CTRLB_RESSEL_16BIT
		var samples uint32
		switch config.Samples {
		case 2:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_2
		case 4:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_4
		case 8:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_8
		case 16:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_16
		case 32:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_32
		case 64:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_64
		case 128:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_128
		case 256:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_256
		case 512:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_512
		case 1024:
			samples = sam.ADC_AVGCTRL_SAMPLENUM_1024
		default: // 1 sample only (no oversampling nor averaging), adjusting result by 0
			// Resolutions less than 16 bits only make sense when sampling only
			// once. Resulting ADC values become erratic when using both
			// multi-sampling and less than 16 bits of resolution.
			samples = sam.ADC_AVGCTRL_SAMPLENUM_1
			switch config.Resolution {
			case 8:
				resolution = sam.ADC_CTRLB_RESSEL_8BIT
			case 10:
				resolution = sam.ADC_CTRLB_RESSEL_10BIT
			case 12:
				resolution = sam.ADC_CTRLB_RESSEL_12BIT
			case 16:
				resolution = sam.ADC_CTRLB_RESSEL_16BIT
			default:
				resolution = sam.ADC_CTRLB_RESSEL_12BIT
			}
		}

		adc.AVGCTRL.Set(uint8(samples<<sam.ADC_AVGCTRL_SAMPLENUM_Pos) |
			(0 << sam.ADC_AVGCTRL_ADJRES_Pos))

		adc.CTRLA.SetBits(sam.ADC_CTRLA_PRESCALER_DIV32 << sam.ADC_CTRLA_PRESCALER_Pos)
		adc.CTRLB.SetBits(uint16(resolution << sam.ADC_CTRLB_RESSEL_Pos))
		adc.SAMPCTRL.Set(5) // sampling Time Length

		for adc.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_SAMPCTRL) {
		} // wait for sync

		// No Negative input (Internal Ground)
		adc.INPUTCTRL.Set(sam.ADC_INPUTCTRL_MUXNEG_GND << sam.ADC_INPUTCTRL_MUXNEG_Pos)
		for adc.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_INPUTCTRL) {
		} // wait for sync

		for adc.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_AVGCTRL) {
		} // wait for sync
		for adc.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_REFCTRL) {
		} // wait for sync

		// TODO: use config.Reference to set AREF level

		// default is 3V3 reference voltage
		adc.REFCTRL.SetBits(sam.ADC_REFCTRL_REFSEL_INTVCC1)
	}

	a.Pin.Configure(PinConfig{Mode: PinAnalog})
}

// Get returns the current value of a ADC pin, in the range 0..0xffff.
func (a ADC) Get() uint16 {
	bus := a.getADCBus()
	ch := a.getADCChannel()

	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_INPUTCTRL) {
	}

	// Selection for the positive ADC input channel
	bus.INPUTCTRL.ClearBits(sam.ADC_INPUTCTRL_MUXPOS_Msk)
	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_ENABLE) {
	}
	bus.INPUTCTRL.SetBits((uint16(ch) & sam.ADC_INPUTCTRL_MUXPOS_Msk) << sam.ADC_INPUTCTRL_MUXPOS_Pos)
	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_ENABLE) {
	}

	// Enable ADC
	bus.CTRLA.SetBits(sam.ADC_CTRLA_ENABLE)
	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_ENABLE) {
	}

	// Start conversion
	bus.SWTRIG.SetBits(sam.ADC_SWTRIG_START)
	for !bus.INTFLAG.HasBits(sam.ADC_INTFLAG_RESRDY) {
	}

	// Clear the Data Ready flag
	bus.INTFLAG.ClearBits(sam.ADC_INTFLAG_RESRDY)
	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_ENABLE) {
	}

	// Start conversion again, since first conversion after reference voltage changed is invalid.
	bus.SWTRIG.SetBits(sam.ADC_SWTRIG_START)

	// Waiting for conversion to complete
	for !bus.INTFLAG.HasBits(sam.ADC_INTFLAG_RESRDY) {
	}
	val := bus.RESULT.Get()

	// Disable ADC
	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_ENABLE) {
	}
	bus.CTRLA.ClearBits(sam.ADC_CTRLA_ENABLE)
	for bus.SYNCBUSY.HasBits(sam.ADC_SYNCBUSY_ENABLE) {
	}

	// scales to 16-bit result
	switch (bus.CTRLB.Get() & sam.ADC_CTRLB_RESSEL_Msk) >> sam.ADC_CTRLB_RESSEL_Pos {
	case sam.ADC_CTRLB_RESSEL_8BIT:
		val = val << 8
	case sam.ADC_CTRLB_RESSEL_10BIT:
		val = val << 6
	case sam.ADC_CTRLB_RESSEL_12BIT:
		val = val << 4
	case sam.ADC_CTRLB_RESSEL_16BIT:
		// Adjust for multiple samples. This is only configured when the
		// resolution is 16 bits.
		switch (bus.AVGCTRL.Get() & sam.ADC_AVGCTRL_SAMPLENUM_Msk) >> sam.ADC_AVGCTRL_SAMPLENUM_Pos {
		case sam.ADC_AVGCTRL_SAMPLENUM_1:
			val <<= 4
		case sam.ADC_AVGCTRL_SAMPLENUM_2:
			val <<= 3
		case sam.ADC_AVGCTRL_SAMPLENUM_4:
			val <<= 2
		case sam.ADC_AVGCTRL_SAMPLENUM_8:
			val <<= 1
		default:
			// These values are all shifted by the hardware so they fit exactly
			// in a 16-bit integer, so they don't need to be shifted here.
		}
	}
	return val
}

func (a ADC) getADCBus() *sam.ADC_Type {
	if (a.Pin >= PB04 && a.Pin <= PB07) || (a.Pin >= PC00) {
		return sam.ADC1
	}
	return sam.ADC0
}

func (a ADC) getADCChannel() uint8 {
	switch a.Pin {
	case PA02:
		return 0
	case PB08:
		return 2
	case PB09:
		return 3
	case PA04:
		return 4
	case PA05:
		return 5
	case PA06:
		return 6
	case PA07:
		return 7
	case PB00:
		return 12
	case PB01:
		return 13
	case PB02:
		return 14
	case PB03:
		return 15
	case PA09:
		return 17
	case PA11:
		return 19

	case PB04:
		return 6
	case PB05:
		return 7
	case PB06:
		return 8
	case PB07:
		return 9

	case PC00:
		return 10
	case PC01:
		return 11
	case PC02:
		return 4
	case PC03:
		return 5
	case PC30:
		return 12
	case PC31:
		return 13

	case PD00:
		return 14
	case PD01:
		return 15
	default:
		panic("Invalid ADC pin")
	}
}

// UART on the SAMD51.
type UART struct {
	Buffer    *RingBuffer
	Bus       *sam.SERCOM_USART_INT_Type
	SERCOM    uint8
	Interrupt interrupt.Interrupt // RXC interrupt
}

var (
	sercomUSART0 = UART{Buffer: NewRingBuffer(), Bus: sam.SERCOM0_USART_INT, SERCOM: 0}
	sercomUSART1 = UART{Buffer: NewRingBuffer(), Bus: sam.SERCOM1_USART_INT, SERCOM: 1}
	sercomUSART2 = UART{Buffer: NewRingBuffer(), Bus: sam.SERCOM2_USART_INT, SERCOM: 2}
	sercomUSART3 = UART{Buffer: NewRingBuffer(), Bus: sam.SERCOM3_USART_INT, SERCOM: 3}
	sercomUSART4 = UART{Buffer: NewRingBuffer(), Bus: sam.SERCOM4_USART_INT, SERCOM: 4}
	sercomUSART5 = UART{Buffer: NewRingBuffer(), Bus: sam.SERCOM5_USART_INT, SERCOM: 5}
)

func init() {
	sercomUSART0.Interrupt = interrupt.New(sam.IRQ_SERCOM0_2, sercomUSART0.handleInterrupt)
	sercomUSART1.Interrupt = interrupt.New(sam.IRQ_SERCOM1_2, sercomUSART1.handleInterrupt)
	sercomUSART2.Interrupt = interrupt.New(sam.IRQ_SERCOM2_2, sercomUSART2.handleInterrupt)
	sercomUSART3.Interrupt = interrupt.New(sam.IRQ_SERCOM3_2, sercomUSART3.handleInterrupt)
	sercomUSART4.Interrupt = interrupt.New(sam.IRQ_SERCOM4_2, sercomUSART4.handleInterrupt)
	sercomUSART5.Interrupt = interrupt.New(sam.IRQ_SERCOM5_2, sercomUSART5.handleInterrupt)
}

const (
	sampleRate16X = 16
	lsbFirst      = 1
)

// Configure the UART.
func (uart *UART) Configure(config UARTConfig) error {
	// Default baud rate to 115200.
	if config.BaudRate == 0 {
		config.BaudRate = 115200
	}

	// determine pins
	if config.TX == 0 && config.RX == 0 {
		// use default pins
		config.TX = UART_TX_PIN
		config.RX = UART_RX_PIN
	}

	// Determine transmit pinout.
	txPinMode, txPad, ok := findPinPadMapping(uart.SERCOM, config.TX)
	if !ok {
		return ErrInvalidOutputPin
	}
	var txPadOut uint32
	// See CTRLA.RXPO bits of the SERCOM USART peripheral (page 945-946) for how
	// pads are mapped to pinout values.
	switch txPad {
	case 0:
		txPadOut = 0
	default:
		// should be flow control (RTS/CTS) pin
		return ErrInvalidOutputPin
	}

	// Determine receive pinout.
	rxPinMode, rxPad, ok := findPinPadMapping(uart.SERCOM, config.RX)
	if !ok {
		return ErrInvalidInputPin
	}
	// As you can see in the CTRLA.RXPO bits of the SERCOM USART peripheral
	// (page 945), input pins are mapped directly.
	rxPadOut := rxPad

	// configure pins
	config.TX.Configure(PinConfig{Mode: txPinMode})
	config.RX.Configure(PinConfig{Mode: rxPinMode})

	// configure RTS/CTS pins if provided
	if config.RTS != 0 && config.CTS != 0 {
		rtsPinMode, _, ok := findPinPadMapping(uart.SERCOM, config.RTS)
		if !ok {
			return ErrInvalidOutputPin
		}

		ctsPinMode, _, ok := findPinPadMapping(uart.SERCOM, config.CTS)
		if !ok {
			return ErrInvalidInputPin
		}

		// See CTRLA.RXPO bits of the SERCOM USART peripheral (page 945-946) for how
		// pads are mapped to pinout values.
		txPadOut = 2

		config.RTS.Configure(PinConfig{Mode: rtsPinMode})
		config.CTS.Configure(PinConfig{Mode: ctsPinMode})
	}

	// reset SERCOM
	uart.Bus.CTRLA.SetBits(sam.SERCOM_USART_INT_CTRLA_SWRST)
	for uart.Bus.CTRLA.HasBits(sam.SERCOM_USART_INT_CTRLA_SWRST) ||
		uart.Bus.SYNCBUSY.HasBits(sam.SERCOM_USART_INT_SYNCBUSY_SWRST) {
	}

	// set UART mode/sample rate
	// SERCOM_USART_CTRLA_MODE(mode) |
	// SERCOM_USART_CTRLA_SAMPR(sampleRate);
	// sam.SERCOM_USART_CTRLA_MODE_USART_INT_CLK = 1?
	uart.Bus.CTRLA.Set((1 << sam.SERCOM_USART_INT_CTRLA_MODE_Pos) |
		(1 << sam.SERCOM_USART_INT_CTRLA_SAMPR_Pos)) // sample rate of 16x

	// set clock
	setSERCOMClockGenerator(uart.SERCOM, sam.GCLK_PCHCTRL_GEN_GCLK1)

	// Set baud rate
	uart.SetBaudRate(config.BaudRate)

	// setup UART frame
	// SERCOM_USART_CTRLA_FORM( (parityMode == SERCOM_NO_PARITY ? 0 : 1) ) |
	// dataOrder << SERCOM_USART_CTRLA_DORD_Pos;
	uart.Bus.CTRLA.SetBits((0 << sam.SERCOM_USART_INT_CTRLA_FORM_Pos) | // no parity
		(lsbFirst << sam.SERCOM_USART_INT_CTRLA_DORD_Pos)) // data order

	// set UART stop bits/parity
	// SERCOM_USART_CTRLB_CHSIZE(charSize) |
	// 	nbStopBits << SERCOM_USART_CTRLB_SBMODE_Pos |
	// 	(parityMode == SERCOM_NO_PARITY ? 0 : parityMode) << SERCOM_USART_CTRLB_PMODE_Pos; //If no parity use default value
	uart.Bus.CTRLB.SetBits((0 << sam.SERCOM_USART_INT_CTRLB_CHSIZE_Pos) | // 8 bits is 0
		(0 << sam.SERCOM_USART_INT_CTRLB_SBMODE_Pos) | // 1 stop bit is zero
		(0 << sam.SERCOM_USART_INT_CTRLB_PMODE_Pos)) // no parity

	// set UART pads. This is not same as pins...
	//  SERCOM_USART_CTRLA_TXPO(txPad) |
	//   SERCOM_USART_CTRLA_RXPO(rxPad);
	uart.Bus.CTRLA.SetBits((txPadOut << sam.SERCOM_USART_INT_CTRLA_TXPO_Pos) |
		(rxPadOut << sam.SERCOM_USART_INT_CTRLA_RXPO_Pos))

	// Enable Transceiver and Receiver
	//sercom->USART.CTRLB.reg |= SERCOM_USART_CTRLB_TXEN | SERCOM_USART_CTRLB_RXEN ;
	uart.Bus.CTRLB.SetBits(sam.SERCOM_USART_INT_CTRLB_TXEN | sam.SERCOM_USART_INT_CTRLB_RXEN)

	// Enable USART1 port.
	// sercom->USART.CTRLA.bit.ENABLE = 0x1u;
	uart.Bus.CTRLA.SetBits(sam.SERCOM_USART_INT_CTRLA_ENABLE)
	for uart.Bus.SYNCBUSY.HasBits(sam.SERCOM_USART_INT_SYNCBUSY_ENABLE) {
	}

	// setup interrupt on receive
	uart.Bus.INTENSET.Set(sam.SERCOM_USART_INT_INTENSET_RXC)

	// Enable RX IRQ.
	// This is a small note at the bottom of the NVIC section of the datasheet:
	// > The integer number specified in the source refers to the respective bit
	// > position in the INTFLAG register of respective peripheral.
	// Therefore, if we only need to listen to the RXC interrupt source (in bit
	// position 2), we only need interrupt source 2 for this SERCOM device.
	uart.Interrupt.Enable()

	return nil
}

// SetBaudRate sets the communication speed for the UART.
func (uart *UART) SetBaudRate(br uint32) {
	// Asynchronous fractional mode (Table 24-2 in datasheet)
	//   BAUD = fref / (sampleRateValue * fbaud)
	// (multiply by 8, to calculate fractional piece)
	// uint32_t baudTimes8 = (SystemCoreClock * 8) / (16 * baudrate);
	baud := (SERCOM_FREQ_REF * 8) / (sampleRate16X * br)

	// sercom->USART.BAUD.FRAC.FP   = (baudTimes8 % 8);
	// sercom->USART.BAUD.FRAC.BAUD = (baudTimes8 / 8);
	uart.Bus.BAUD.Set(uint16(((baud % 8) << sam.SERCOM_USART_INT_BAUD_FRAC_MODE_FP_Pos) |
		((baud / 8) << sam.SERCOM_USART_INT_BAUD_FRAC_MODE_BAUD_Pos)))
}

// WriteByte writes a byte of data to the UART.
func (uart *UART) writeByte(c byte) error {
	// wait until ready to receive
	for !uart.Bus.INTFLAG.HasBits(sam.SERCOM_USART_INT_INTFLAG_DRE) {
	}
	uart.Bus.DATA.Set(uint32(c))
	return nil
}

func (uart *UART) flush() {}

func (uart *UART) handleInterrupt(interrupt.Interrupt) {
	// should reset IRQ
	uart.Receive(byte((uart.Bus.DATA.Get() & 0xFF)))
	uart.Bus.INTFLAG.SetBits(sam.SERCOM_USART_INT_INTFLAG_RXC)
}

// I2C on the SAMD51.
type I2C struct {
	Bus    *sam.SERCOM_I2CM_Type
	SERCOM uint8
}

// I2CConfig is used to store config info for I2C.
type I2CConfig struct {
	Frequency uint32
	SCL       Pin
	SDA       Pin
}

const (
	// SERCOM_FREQ_REF is always reference frequency on SAMD51 regardless of CPU speed.
	SERCOM_FREQ_REF       = 48000000
	SERCOM_FREQ_REF_GCLK0 = 120000000

	// Default rise time in nanoseconds, based on 4.7K ohm pull up resistors
	riseTimeNanoseconds = 125

	// wire bus states
	wireUnknownState = 0
	wireIdleState    = 1
	wireOwnerState   = 2
	wireBusyState    = 3

	// wire commands
	wireCmdNoAction    = 0
	wireCmdRepeatStart = 1
	wireCmdRead        = 2
	wireCmdStop        = 3
)

const i2cTimeout = 28000 // about 210us

// Configure is intended to setup the I2C interface.
func (i2c *I2C) Configure(config I2CConfig) error {
	// Default I2C bus speed is 100 kHz.
	if config.Frequency == 0 {
		config.Frequency = 100 * KHz
	}

	// Use default I2C pins if not set.
	if config.SDA == 0 && config.SCL == 0 {
		config.SDA = SDA_PIN
		config.SCL = SCL_PIN
	}

	sclPinMode, sclPad, ok := findPinPadMapping(i2c.SERCOM, config.SCL)
	if !ok || sclPad != 1 {
		// SCL must be on pad 1, according to section 36.4 of the datasheet.
		// Note: this is not an exhaustive test for I2C support on the pin: not
		// all pins support I2C.
		return ErrInvalidClockPin
	}
	sdaPinMode, sdaPad, ok := findPinPadMapping(i2c.SERCOM, config.SDA)
	if !ok || sdaPad != 0 {
		// SDA must be on pad 0, according to section 36.4 of the datasheet.
		// Note: this is not an exhaustive test for I2C support on the pin: not
		// all pins support I2C.
		return ErrInvalidDataPin
	}

	// reset SERCOM
	i2c.Bus.CTRLA.SetBits(sam.SERCOM_I2CM_CTRLA_SWRST)
	for i2c.Bus.CTRLA.HasBits(sam.SERCOM_I2CM_CTRLA_SWRST) ||
		i2c.Bus.SYNCBUSY.HasBits(sam.SERCOM_I2CM_SYNCBUSY_SWRST) {
	}

	// set clock
	setSERCOMClockGenerator(i2c.SERCOM, sam.GCLK_PCHCTRL_GEN_GCLK1)

	// Set i2c controller mode
	//SERCOM_I2CM_CTRLA_MODE( I2C_MASTER_OPERATION )
	// sam.SERCOM_I2CM_CTRLA_MODE_I2C_MASTER = 5?
	i2c.Bus.CTRLA.Set(5 << sam.SERCOM_I2CM_CTRLA_MODE_Pos) // |

	i2c.SetBaudRate(config.Frequency)

	// Enable I2CM port.
	// sercom->USART.CTRLA.bit.ENABLE = 0x1u;
	i2c.Bus.CTRLA.SetBits(sam.SERCOM_I2CM_CTRLA_ENABLE)
	for i2c.Bus.SYNCBUSY.HasBits(sam.SERCOM_I2CM_SYNCBUSY_ENABLE) {
	}

	// set bus idle mode
	i2c.Bus.STATUS.SetBits(wireIdleState << sam.SERCOM_I2CM_STATUS_BUSSTATE_Pos)
	for i2c.Bus.SYNCBUSY.HasBits(sam.SERCOM_I2CM_SYNCBUSY_SYSOP) {
	}

	// enable pins
	config.SDA.Configure(PinConfig{Mode: sdaPinMode})
	config.SCL.Configure(PinConfig{Mode: sclPinMode})

	return nil
}

// SetBaudRate sets the communication speed for I2C.
func (i2c *I2C) SetBaudRate(br uint32) error {
	// Synchronous arithmetic baudrate, via Adafruit SAMD51 implementation:
	// sercom->I2CM.BAUD.bit.BAUD = SERCOM_FREQ_REF / ( 2 * baudrate) - 1 ;
	baud := SERCOM_FREQ_REF/(2*br) - 1
	i2c.Bus.BAUD.Set(baud)
	return nil
}

// Tx does a single I2C transaction at the specified address.
// It clocks out the given address, writes the bytes in w, reads back len(r)
// bytes and stores them in r, and generates a stop condition on the bus.
func (i2c *I2C) Tx(addr uint16, w, r []byte) error {
	var err error
	if len(w) != 0 {
		// send start/address for write
		i2c.sendAddress(addr, true)

		// wait until transmission complete
		timeout := i2cTimeout
		for !i2c.Bus.INTFLAG.HasBits(sam.SERCOM_I2CM_INTFLAG_MB) {
			timeout--
			if timeout == 0 {
				return errI2CWriteTimeout
			}
		}

		// ACK received (0: ACK, 1: NACK)
		if i2c.Bus.STATUS.HasBits(sam.SERCOM_I2CM_STATUS_RXNACK) {
			return errI2CAckExpected
		}

		// write data
		for _, b := range w {
			err = i2c.WriteByte(b)
			if err != nil {
				return err
			}
		}

		err = i2c.signalStop()
		if err != nil {
			return err
		}
	}
	if len(r) != 0 {
		// send start/address for read
		i2c.sendAddress(addr, false)

		// wait transmission complete
		for !i2c.Bus.INTFLAG.HasBits(sam.SERCOM_I2CM_INTFLAG_SB) {
			// If the peripheral NACKS the address, the MB bit will be set.
			// In that case, send a stop condition and return error.
			if i2c.Bus.INTFLAG.HasBits(sam.SERCOM_I2CM_INTFLAG_MB) {
				i2c.Bus.CTRLB.SetBits(wireCmdStop << sam.SERCOM_I2CM_CTRLB_CMD_Pos) // Stop condition
				return errI2CAckExpected
			}
		}

		// ACK received (0: ACK, 1: NACK)
		if i2c.Bus.STATUS.HasBits(sam.SERCOM_I2CM_STATUS_RXNACK) {
			return errI2CAckExpected
		}

		// read first byte
		r[0] = i2c.readByte()
		for i := 1; i < len(r); i++ {
			// Send an ACK
			i2c.Bus.CTRLB.ClearBits(sam.SERCOM_I2CM_CTRLB_ACKACT)

			i2c.signalRead()

			// Read data and send the ACK
			r[i] = i2c.readByte()
		}

		// Send NACK to end transmission
		i2c.Bus.CTRLB.SetBits(sam.SERCOM_I2CM_CTRLB_ACKACT)

		err = i2c.signalStop()
		if err != nil {
			return err
		}
	}

	return nil
}

// WriteByte writes a single byte to the I2C bus.
func (i2c *I2C) WriteByte(data byte) error {
	// Send data byte
	i2c.Bus.DATA.Set(data)

	// wait until transmission successful
	timeout := i2cTimeout
	for !i2c.Bus.INTFLAG.HasBits(sam.SERCOM_I2CM_INTFLAG_MB) {
		// check for bus error
		if i2c.Bus.STATUS.HasBits(sam.SERCOM_I2CM_STATUS_BUSERR) {
			return errI2CBusError
		}
		timeout--
		if timeout == 0 {
			return errI2CWriteTimeout
		}
	}

	if i2c.Bus.STATUS.HasBits(sam.SERCOM_I2CM_STATUS_RXNACK) {
		return errI2CAckExpected
	}

	return nil
}

// sendAddress sends the address and start signal
func (i2c *I2C) sendAddress(address uint16, write bool) error {
	data := (address << 1)
	if !write {
		data |= 1 // set read flag
	}

	// wait until bus ready
	timeout := i2cTimeout
	for !i2c.Bus.STATUS.HasBits(wireIdleState<<sam.SERCOM_I2CM_STATUS_BUSSTATE_Pos) &&
		!i2c.Bus.STATUS.HasBits(wireOwnerState<<sam.SERCOM_I2CM_STATUS_BUSSTATE_Pos) {
		timeout--
		if timeout == 0 {
			return errI2CBusReadyTimeout
		}
	}
	i2c.Bus.ADDR.Set(uint32(data))

	return nil
}

func (i2c *I2C) signalStop() error {
	i2c.Bus.CTRLB.SetBits(wireCmdStop << sam.SERCOM_I2CM_CTRLB_CMD_Pos) // Stop command
	timeout := i2cTimeout
	for i2c.Bus.SYNCBUSY.HasBits(sam.SERCOM_I2CM_SYNCBUSY_SYSOP) {
		timeout--
		if timeout == 0 {
			return errI2CSignalStopTimeout
		}
	}
	return nil
}

func (i2c *I2C) signalRead() error {
	i2c.Bus.CTRLB.SetBits(wireCmdRead << sam.SERCOM_I2CM_CTRLB_CMD_Pos) // Read command
	timeout := i2cTimeout
	for i2c.Bus.SYNCBUSY.HasBits(sam.SERCOM_I2CM_SYNCBUSY_SYSOP) {
		timeout--
		if timeout == 0 {
			return errI2CSignalReadTimeout
		}
	}
	return nil
}

func (i2c *I2C) readByte() byte {
	for !i2c.Bus.INTFLAG.HasBits(sam.SERCOM_I2CM_INTFLAG_SB) {
	}
	return byte(i2c.Bus.DATA.Get())
}

// SPI
type SPI struct {
	Bus    *sam.SERCOM_SPIM_Type
	SERCOM uint8
}

// SPIConfig is used to store config info for SPI.
type SPIConfig struct {
	Frequency uint32
	SCK       Pin
	SDO       Pin
	SDI       Pin
	LSBFirst  bool
	Mode      uint8
}

// Configure is intended to setup the SPI interface.
func (spi *SPI) Configure(config SPIConfig) error {
	// Use default pins if not set.
	if config.SCK == 0 && config.SDO == 0 && config.SDI == 0 {
		config.SCK = SPI0_SCK_PIN
		config.SDO = SPI0_SDO_PIN
		config.SDI = SPI0_SDI_PIN
	}

	// set default frequency
	if config.Frequency == 0 {
		config.Frequency = 4000000
	}

	// Determine the input pinout (for SDI).
	var dataInPinout uint32
	var SDIPinMode PinMode
	if config.SDI != NoPin {
		var ok bool
		SDIPinMode, dataInPinout, ok = findPinPadMapping(spi.SERCOM, config.SDI)
		if !ok {
			return ErrInvalidInputPin
		}
	}

	// Determine the output pinout (for SDO/SCK).
	// See DOPO field in the CTRLA register on page 986 of the datasheet.
	var dataOutPinout uint32
	sckPinMode, sckPad, ok := findPinPadMapping(spi.SERCOM, config.SCK)
	if !ok || sckPad != 1 {
		// SCK pad must always be 1
		return ErrInvalidOutputPin
	}
	SDOPinMode, SDOPad, ok := findPinPadMapping(spi.SERCOM, config.SDO)
	if !ok {
		return ErrInvalidOutputPin
	}
	switch SDOPad {
	case 0:
		dataOutPinout = 0x0
	case 3:
		dataOutPinout = 0x2
	default:
		return ErrInvalidOutputPin
	}

	// Disable SPI port.
	spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_ENABLE)
	for spi.Bus.SYNCBUSY.HasBits(sam.SERCOM_SPIM_SYNCBUSY_ENABLE) {
	}

	// enable pins
	config.SCK.Configure(PinConfig{Mode: sckPinMode})
	config.SDO.Configure(PinConfig{Mode: SDOPinMode})
	if config.SDI != NoPin {
		config.SDI.Configure(PinConfig{Mode: SDIPinMode})
	}

	// reset SERCOM
	spi.Bus.CTRLA.SetBits(sam.SERCOM_SPIM_CTRLA_SWRST)
	for spi.Bus.CTRLA.HasBits(sam.SERCOM_SPIM_CTRLA_SWRST) ||
		spi.Bus.SYNCBUSY.HasBits(sam.SERCOM_SPIM_SYNCBUSY_SWRST) {
	}

	// set bit transfer order
	dataOrder := uint32(0)
	if config.LSBFirst {
		dataOrder = 1
	}

	// Set SPI controller
	// SERCOM_SPIM_CTRLA_MODE_SPI_MASTER = 3
	spi.Bus.CTRLA.Set((3 << sam.SERCOM_SPIM_CTRLA_MODE_Pos) |
		(dataOutPinout << sam.SERCOM_SPIM_CTRLA_DOPO_Pos) |
		(dataInPinout << sam.SERCOM_SPIM_CTRLA_DIPO_Pos) |
		(dataOrder << sam.SERCOM_SPIM_CTRLA_DORD_Pos))

	spi.Bus.CTRLB.SetBits((0 << sam.SERCOM_SPIM_CTRLB_CHSIZE_Pos) | // 8bit char size
		sam.SERCOM_SPIM_CTRLB_RXEN) // receive enable

	for spi.Bus.SYNCBUSY.HasBits(sam.SERCOM_SPIM_SYNCBUSY_CTRLB) {
	}

	// set mode
	switch config.Mode {
	case 0:
		spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_CPHA)
		spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_CPOL)
	case 1:
		spi.Bus.CTRLA.SetBits(sam.SERCOM_SPIM_CTRLA_CPHA)
		spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_CPOL)
	case 2:
		spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_CPHA)
		spi.Bus.CTRLA.SetBits(sam.SERCOM_SPIM_CTRLA_CPOL)
	case 3:
		spi.Bus.CTRLA.SetBits(sam.SERCOM_SPIM_CTRLA_CPHA | sam.SERCOM_SPIM_CTRLA_CPOL)
	default: // to mode 0
		spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_CPHA)
		spi.Bus.CTRLA.ClearBits(sam.SERCOM_SPIM_CTRLA_CPOL)
	}

	// Set the clock frequency.
	// There are two clocks we can use GCLK0 (120MHz) and GCLK1 (48MHz).
	// We can use any even divisor for these clock, which means:
	//   - for GCLK0 we can make 60MHz, 30MHz, 20MHz, 15MHz, 12MHz, 10MHz, etc
	//   - for GCLK1 we can make 24MHz, 12MHz, 8MHz, 6MHz, 4.8MHz, 4MHz, etc
	// This means that by trying both clocks, we can have a wider selection of
	// available SPI clock frequencies.

	// Calculate the baudrate if we would use GCLK1 (48MHz), and the resulting
	// frequency. The baud rate is rounded up, so that the resulting frequency
	// is rounded down from the maximum value (meaning it will always be smaller
	// than or equal to config.Frequency).
	baudRateGCLK1 := (SERCOM_FREQ_REF/2 + config.Frequency - 1) / config.Frequency
	freqGCLK1 := SERCOM_FREQ_REF / 2 / baudRateGCLK1

	// Same for GCLK0 (120MHz).
	baudRateGCLK0 := (SERCOM_FREQ_REF_GCLK0/2 + config.Frequency - 1) / config.Frequency
	freqGCLK0 := SERCOM_FREQ_REF_GCLK0 / 2 / baudRateGCLK0

	// Pick the clock source that is the closest to the maximum baud rate.
	// Note: there may be reasons to prefer the lower frequency clock (like
	// power consumption). If that's the case, we might want to always use the
	// 48MHz clock at low frequencies (below 4MHz or so).
	if freqGCLK0 > freqGCLK1 && uint32(uint8(baudRateGCLK0-1))+1 == baudRateGCLK0 {
		// Pick this 120MHz clock if it results in a better frequency after
		// division, and the baudRate value fits in the BAUD register.
		setSERCOMClockGenerator(spi.SERCOM, sam.GCLK_PCHCTRL_GEN_GCLK0)
		spi.Bus.BAUD.Set(uint8(baudRateGCLK0 - 1))
	} else {
		// Use the 48MHz clock in other cases.
		setSERCOMClockGenerator(spi.SERCOM, sam.GCLK_PCHCTRL_GEN_GCLK1)
		spi.Bus.BAUD.Set(uint8(baudRateGCLK1 - 1))
	}

	// Enable SPI port.
	spi.Bus.CTRLA.SetBits(sam.SERCOM_SPIM_CTRLA_ENABLE)
	for spi.Bus.SYNCBUSY.HasBits(sam.SERCOM_SPIM_SYNCBUSY_ENABLE) {
	}

	return nil
}

// Transfer writes/reads a single byte using the SPI interface.
func (spi *SPI) Transfer(w byte) (byte, error) {
	// write data
	spi.Bus.DATA.Set(uint32(w))

	// wait for receive
	for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_RXC) {
	}

	// return data
	return byte(spi.Bus.DATA.Get()), nil
}

// Tx handles read/write operation for SPI interface. Since SPI is a synchronous write/read
// interface, there must always be the same number of bytes written as bytes read.
// The Tx method knows about this, and offers a few different ways of calling it.
//
// This form sends the bytes in tx buffer, putting the resulting bytes read into the rx buffer.
// Note that the tx and rx buffers must be the same size:
//
//	spi.Tx(tx, rx)
//
// This form sends the tx buffer, ignoring the result. Useful for sending "commands" that return zeros
// until all the bytes in the command packet have been received:
//
//	spi.Tx(tx, nil)
//
// This form sends zeros, putting the result into the rx buffer. Good for reading a "result packet":
//
//	spi.Tx(nil, rx)
func (spi *SPI) Tx(w, r []byte) error {
	switch {
	case w == nil:
		// read only, so write zero and read a result.
		spi.rx(r)
	case r == nil:
		// write only
		spi.tx(w)

	default:
		// write/read
		if len(w) != len(r) {
			return ErrTxInvalidSliceSize
		}

		spi.txrx(w, r)
	}

	return nil
}

func (spi *SPI) tx(tx []byte) {
	for i := 0; i < len(tx); i++ {
		for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_DRE) {
		}
		spi.Bus.DATA.Set(uint32(tx[i]))
	}
	for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_TXC) {
	}

	// read to clear RXC register
	for spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_RXC) {
		spi.Bus.DATA.Get()
	}
}

func (spi *SPI) rx(rx []byte) {
	spi.Bus.DATA.Set(0)
	for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_DRE) {
	}

	for i := 1; i < len(rx); i++ {
		spi.Bus.DATA.Set(0)
		for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_RXC) {
		}
		rx[i-1] = byte(spi.Bus.DATA.Get())
	}
	for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_RXC) {
	}
	rx[len(rx)-1] = byte(spi.Bus.DATA.Get())
}

func (spi *SPI) txrx(tx, rx []byte) {
	spi.Bus.DATA.Set(uint32(tx[0]))
	for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_DRE) {
	}

	for i := 1; i < len(rx); i++ {
		spi.Bus.DATA.Set(uint32(tx[i]))
		for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_RXC) {
		}
		rx[i-1] = byte(spi.Bus.DATA.Get())
	}
	for !spi.Bus.INTFLAG.HasBits(sam.SERCOM_SPIM_INTFLAG_RXC) {
	}
	rx[len(rx)-1] = byte(spi.Bus.DATA.Get())
}

// The QSPI peripheral on ATSAMD51 is only available on the following pins
const (
	QSPI_SCK   = PB10
	QSPI_CS    = PB11
	QSPI_DATA0 = PA08
	QSPI_DATA1 = PA09
	QSPI_DATA2 = PA10
	QSPI_DATA3 = PA11
)

// TCC is one timer peripheral, which consists of a counter and multiple output
// channels (that can be connected to actual pins). You can set the frequency
// using SetPeriod, but only for all the channels in this timer peripheral at
// once.
type TCC sam.TCC_Type

//go:inline
func (tcc *TCC) timer() *sam.TCC_Type {
	return (*sam.TCC_Type)(tcc)
}

// Configure enables and configures this TCC.
func (tcc *TCC) Configure(config PWMConfig) error {
	// Enable the TCC clock to be able to use the TCC.
	tcc.configureClock()

	// Disable timer (if it was enabled). This is necessary because
	// tcc.setPeriod may want to change the prescaler bits in CTRLA, which is
	// only allowed when the TCC is disabled.
	tcc.timer().CTRLA.ClearBits(sam.TCC_CTRLA_ENABLE)

	// Use "Normal PWM" (single-slope PWM)
	tcc.timer().WAVE.Set(sam.TCC_WAVE_WAVEGEN_NPWM)

	// Wait for synchronization of all changed registers.
	for tcc.timer().SYNCBUSY.Get() != 0 {
	}

	// Set the period and prescaler.
	err := tcc.setPeriod(config.Period, true)

	// Enable the timer.
	tcc.timer().CTRLA.SetBits(sam.TCC_CTRLA_ENABLE)

	// Wait for synchronization of all changed registers.
	for tcc.timer().SYNCBUSY.Get() != 0 {
	}

	// Return any error that might have occurred in the tcc.setPeriod call.
	return err
}

// SetPeriod updates the period of this TCC peripheral.
// To set a particular frequency, use the following formula:
//
//	period = 1e9 / frequency
//
// If you use a period of 0, a period that works well for LEDs will be picked.
//
// SetPeriod will not change the prescaler, but also won't change the current
// value in any of the channels. This means that you may need to update the
// value for the particular channel.
//
// Note that you cannot pick any arbitrary period after the TCC peripheral has
// been configured. If you want to switch between frequencies, pick the lowest
// frequency (longest period) once when calling Configure and adjust the
// frequency here as needed.
func (tcc *TCC) SetPeriod(period uint64) error {
	return tcc.setPeriod(period, false)
}

// setPeriod sets the period of this TCC, possibly updating the prescaler as
// well. The prescaler can only modified when the TCC is disabled, that is, in
// the Configure function.
func (tcc *TCC) setPeriod(period uint64, updatePrescaler bool) error {
	var top uint64
	if period == 0 {
		// Make sure the TOP value is at 0xffff (enough for a 16-bit timer).
		top = 0xffff
	} else {
		// The formula below calculates the following formula, optimized:
		//     period * (120e6 / 1e9)
		// This assumes that the chip is running from generic clock generator 0
		// at 120MHz.
		top = period * 3 / 25
	}

	maxTop := uint64(0xffff)
	if tcc.timer() == sam.TCC0 || tcc.timer() == sam.TCC1 {
		// Only TCC0 and TCC1 are 24-bit timers, the rest are 16-bit.
		maxTop = 0xffffff
	}

	if updatePrescaler {
		// This function was called during Configure(), with the timer disabled.
		// Note that updating the prescaler can only happen while the peripheral
		// is disabled.
		var prescaler uint32
		switch {
		case top <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV1
		case top/2 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV2
			top = top / 2
		case top/4 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV4
			top = top / 4
		case top/8 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV8
			top = top / 8
		case top/16 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV16
			top = top / 16
		case top/64 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV64
			top = top / 64
		case top/256 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV256
			top = top / 256
		case top/1024 <= maxTop:
			prescaler = sam.TCC_CTRLA_PRESCALER_DIV1024
			top = top / 1024
		default:
			return ErrPWMPeriodTooLong
		}
		tcc.timer().CTRLA.Set((tcc.timer().CTRLA.Get() &^ sam.TCC_CTRLA_PRESCALER_Msk) | (prescaler << sam.TCC_CTRLA_PRESCALER_Pos))
	} else {
		// Do not update the prescaler, but use the already-configured
		// prescaler. This is the normal SetPeriod case, where the prescaler
		// must not be changed.
		prescaler := (tcc.timer().CTRLA.Get() & sam.TCC_CTRLA_PRESCALER_Msk) >> sam.TCC_CTRLA_PRESCALER_Pos
		switch prescaler {
		case sam.TCC_CTRLA_PRESCALER_DIV1:
			top /= 1 // no-op
		case sam.TCC_CTRLA_PRESCALER_DIV2:
			top /= 2
		case sam.TCC_CTRLA_PRESCALER_DIV4:
			top /= 4
		case sam.TCC_CTRLA_PRESCALER_DIV8:
			top /= 8
		case sam.TCC_CTRLA_PRESCALER_DIV16:
			top /= 16
		case sam.TCC_CTRLA_PRESCALER_DIV64:
			top /= 64
		case sam.TCC_CTRLA_PRESCALER_DIV256:
			top /= 256
		case sam.TCC_CTRLA_PRESCALER_DIV1024:
			top /= 1024
		default:
			// unreachable
		}
		if top > maxTop {
			return ErrPWMPeriodTooLong
		}
	}

	// Set the period (the counter top).
	tcc.timer().PER.Set(uint32(top) - 1)

	// Wait for synchronization of CTRLA.PRESCALER and PER registers.
	for tcc.timer().SYNCBUSY.Get() != 0 {
	}

	return nil
}

// Top returns the current counter top, for use in duty cycle calculation. It
// will only change with a call to Configure or SetPeriod, otherwise it is
// constant.
//
// The value returned here is hardware dependent. In general, it's best to treat
// it as an opaque value that can be divided by some number and passed to
// tcc.Set (see tcc.Set for more information).
func (tcc *TCC) Top() uint32 {
	return tcc.timer().PER.Get() + 1
}

// Counter returns the current counter value of the timer in this TCC
// peripheral. It may be useful for debugging.
func (tcc *TCC) Counter() uint32 {
	tcc.timer().CTRLBSET.Set(sam.TCC_CTRLBSET_CMD_READSYNC << sam.TCC_CTRLBSET_CMD_Pos)
	for tcc.timer().SYNCBUSY.Get() != 0 {
	}
	return tcc.timer().COUNT.Get()
}

// Constants that encode a TCC number and WO number together in a single byte.
const (
	pinTCC0   = 1 << 4 // keep the value 0 usable as "no value"
	pinTCC1   = 2 << 4
	pinTCC2   = 3 << 4
	pinTCC3   = 4 << 4
	pinTCC4   = 5 << 4
	pinTCC0_0 = pinTCC0 | 0
	pinTCC0_1 = pinTCC0 | 1
	pinTCC0_2 = pinTCC0 | 2
	pinTCC0_3 = pinTCC0 | 3
	pinTCC0_4 = pinTCC0 | 4
	pinTCC0_5 = pinTCC0 | 5
	pinTCC0_6 = pinTCC0 | 6
	pinTCC1_0 = pinTCC1 | 0
	pinTCC1_2 = pinTCC1 | 2
	pinTCC1_4 = pinTCC1 | 4
	pinTCC1_6 = pinTCC1 | 6
	pinTCC2_0 = pinTCC2 | 0
	pinTCC2_2 = pinTCC2 | 2
	pinTCC3_0 = pinTCC3 | 0
	pinTCC4_0 = pinTCC4 | 0
)

// This is a copy of columns F and G (the TCC columns) of table 6-1 in the
// datasheet:
// http://ww1.microchip.com/downloads/en/DeviceDoc/60001507E.pdf
// For example, "TCC0/WO[2]" is converted to pinTCC0_2.
// Only the even pin numbers are stored here. The odd pin numbers are left out,
// because their PWM output can be determined from the even number: just add one
// to the wave output (WO) number.
var pinTimerMapping = [...]struct{ F, G uint8 }{
	// page 33
	PC04 / 2: {pinTCC0_0, 0},
	PA08 / 2: {pinTCC0_0, pinTCC1_4},
	PA10 / 2: {pinTCC0_2, pinTCC1_6},
	PB10 / 2: {pinTCC0_4, pinTCC1_0},
	PB12 / 2: {pinTCC3_0, pinTCC0_0},
	PB14 / 2: {pinTCC4_0, pinTCC0_2},
	PD08 / 2: {pinTCC0_1, 0},
	PD10 / 2: {pinTCC0_3, 0},
	PD12 / 2: {pinTCC0_5, 0},
	PC10 / 2: {pinTCC0_0, pinTCC1_4},
	// page 34
	PC12 / 2: {pinTCC0_2, pinTCC1_6},
	PC14 / 2: {pinTCC0_4, pinTCC1_0},
	PA12 / 2: {pinTCC0_6, pinTCC1_2},
	PA14 / 2: {pinTCC2_0, pinTCC1_2},
	PA16 / 2: {pinTCC1_0, pinTCC0_4},
	PA18 / 2: {pinTCC1_2, pinTCC0_6},
	PC16 / 2: {pinTCC0_0, 0},
	PC18 / 2: {pinTCC0_2, 0},
	PC20 / 2: {pinTCC0_4, 0},
	PC22 / 2: {pinTCC0_6, 0},
	PD20 / 2: {pinTCC1_0, 0},
	PB16 / 2: {pinTCC3_0, pinTCC0_4},
	PB18 / 2: {pinTCC1_0, 0},
	// page 35
	PB20 / 2: {pinTCC1_2, 0},
	PA20 / 2: {pinTCC1_4, pinTCC0_0},
	PA22 / 2: {pinTCC1_6, pinTCC0_2},
	PA24 / 2: {pinTCC2_2, 0},
	PB26 / 2: {pinTCC1_2, 0},
	PB28 / 2: {pinTCC1_4, 0},
	PA30 / 2: {pinTCC2_0, 0},
	// page 36
	PB30 / 2: {pinTCC4_0, pinTCC0_6},
	PB02 / 2: {pinTCC2_2, 0},
}

// findPinTimerMapping returns the pin mode (PinTCCF or PinTCCG) and the channel
// number for a given timer and pin. A zero PinMode is returned if no mapping
// could be found.
func findPinTimerMapping(timer uint8, pin Pin) (PinMode, uint8) {
	if int(pin/2) >= len(pinTimerMapping) {
		return 0, 0 // invalid pin number
	}

	mapping := pinTimerMapping[pin/2]

	// Check for column F in the datasheet.
	if mapping.F>>4-1 == timer {
		return PinTCCF, mapping.F&0x0f + uint8(pin)&1
	}

	// Check for column G in the datasheet.
	if mapping.G>>4-1 == timer {
		return PinTCCG, mapping.G&0x0f + uint8(pin)&1
	}

	// Nothing found.
	return 0, 0
}

// Channel returns a PWM channel for the given pin. Note that one channel may be
// shared between multiple pins, and so will have the same duty cycle. If this
// is not desirable, look for a different TCC or consider using a different pin.
func (tcc *TCC) Channel(pin Pin) (uint8, error) {
	pinMode, woOutput := findPinTimerMapping(tcc.timerNum(), pin)

	if pinMode == 0 {
		// No pin could be found.
		return 0, ErrInvalidOutputPin
	}

	// Convert from waveform output to channel, assuming WEXCTRL.OTMX equals 0.
	// See table 49-4 "Output Matrix Channel Pin Routing Configuration" on page
	// 1829 of the datasheet.
	// The number of channels varies by TCC instance, hence the need to switch
	// over them. For TCC2-4 the number of channels is equal to the number of
	// waveform outputs, so the WO number maps directly to the channel number.
	// For TCC0 and TCC1 this is not the case so they will need some special
	// handling.
	channel := woOutput
	switch tcc.timer() {
	case sam.TCC0:
		channel = woOutput % 6
	case sam.TCC1:
		channel = woOutput % 4
	}

	// Enable the port multiplexer for pin
	pin.setPinCfg(sam.PORT_GROUP_PINCFG_PMUXEN)

	// Connect timer/mux to pin.
	if pin&1 > 0 {
		// odd pin, so save the even pins
		val := pin.getPMux() & sam.PORT_GROUP_PMUX_PMUXE_Msk
		pin.setPMux(val | uint8(pinMode<<sam.PORT_GROUP_PMUX_PMUXO_Pos))
	} else {
		// even pin, so save the odd pins
		val := pin.getPMux() & sam.PORT_GROUP_PMUX_PMUXO_Msk
		pin.setPMux(val | uint8(pinMode<<sam.PORT_GROUP_PMUX_PMUXE_Pos))
	}

	return channel, nil
}

// SetInverting sets whether to invert the output of this channel.
// Without inverting, a 25% duty cycle would mean the output is high for 25% of
// the time and low for the rest. Inverting flips the output as if a NOT gate
// was placed at the output, meaning that the output would be 25% low and 75%
// high with a duty cycle of 25%.
func (tcc *TCC) SetInverting(channel uint8, inverting bool) {
	if inverting {
		tcc.timer().WAVE.SetBits(1 << (sam.TCC_WAVE_POL0_Pos + channel))
	} else {
		tcc.timer().WAVE.ClearBits(1 << (sam.TCC_WAVE_POL0_Pos + channel))
	}

	// Wait for synchronization of the WAVE register.
	for tcc.timer().SYNCBUSY.Get() != 0 {
	}
}

// Set updates the channel value. This is used to control the channel duty
// cycle, in other words the fraction of time the channel output is high (or low
// when inverted). For example, to set it to a 25% duty cycle, use:
//
//	tcc.Set(channel, tcc.Top() / 4)
//
// tcc.Set(channel, 0) will set the output to low and tcc.Set(channel,
// tcc.Top()) will set the output to high, assuming the output isn't inverted.
func (tcc *TCC) Set(channel uint8, value uint32) {
	// Update CCBUF, which provides double buffering. The update is applied on
	// the next cycle.
	tcc.timer().CCBUF[channel].Set(value)
	for tcc.timer().SYNCBUSY.Get() != 0 {
	}
}

// EnterBootloader should perform a system reset in preparation
// to switch to the bootloader to flash new firmware.
func EnterBootloader() {
	arm.DisableInterrupts()

	// Perform magic reset into bootloader, as mentioned in
	// https://github.com/arduino/ArduinoCore-samd/issues/197
	*(*uint32)(unsafe.Pointer(uintptr(0x20000000 + HSRAM_SIZE - 4))) = resetMagicValue

	arm.SystemReset()
}

// DAC on the SAMD51.
type DAC struct {
	Channel uint8
}

var (
	DAC0 = DAC{Channel: 0}
	DAC1 = DAC{Channel: 1}
)

// DACConfig placeholder for future expansion.
type DACConfig struct {
}

// Configure the DAC.
// output pin must already be configured.
func (dac DAC) Configure(config DACConfig) {
	// Turn on clock for DAC
	sam.MCLK.APBDMASK.SetBits(sam.MCLK_APBDMASK_DAC_)

	if !sam.GCLK.PCHCTRL[42].HasBits(sam.GCLK_PCHCTRL_CHEN) {
		// Use Generic Clock Generator 4 as source for DAC.
		sam.GCLK.PCHCTRL[42].Set((sam.GCLK_PCHCTRL_GEN_GCLK4 << sam.GCLK_PCHCTRL_GEN_Pos) | sam.GCLK_PCHCTRL_CHEN)
		for sam.GCLK.SYNCBUSY.HasBits(sam.GCLK_SYNCBUSY_GENCTRL_GCLK4 << sam.GCLK_SYNCBUSY_GENCTRL_Pos) {
		}

		// reset DAC
		sam.DAC.CTRLA.Set(sam.DAC_CTRLA_SWRST)

		// wait for reset complete
		for sam.DAC.CTRLA.HasBits(sam.DAC_CTRLA_SWRST) {
		}
		for sam.DAC.SYNCBUSY.HasBits(sam.DAC_SYNCBUSY_SWRST) {
		}
	}

	sam.DAC.CTRLA.ClearBits(sam.DAC_CTRLA_ENABLE)
	for sam.DAC.SYNCBUSY.HasBits(sam.DAC_SYNCBUSY_ENABLE) {
	}

	// enable
	sam.DAC.CTRLB.Set(sam.DAC_CTRLB_REFSEL_VREFPU << sam.DAC_CTRLB_REFSEL_Pos)
	sam.DAC.DACCTRL[dac.Channel].SetBits((sam.DAC_DACCTRL_CCTRL_CC12M << sam.DAC_DACCTRL_CCTRL_Pos) | sam.DAC_DACCTRL_ENABLE)
	sam.DAC.CTRLA.Set(sam.DAC_CTRLA_ENABLE)

	for sam.DAC.SYNCBUSY.HasBits(sam.DAC_SYNCBUSY_ENABLE) {
	}

	switch dac.Channel {
	case 0:
		for !sam.DAC.STATUS.HasBits(sam.DAC_STATUS_READY0) {
		}
	default:
		for !sam.DAC.STATUS.HasBits(sam.DAC_STATUS_READY1) {
		}
	}
}

// Set writes a single 16-bit value to the DAC.
// Since the ATSAMD51 only has a 12-bit DAC, the passed-in value will be scaled down.
func (dac DAC) Set(value uint16) error {
	sam.DAC.DATA[dac.Channel].Set(value >> 4)
	dac.syncDAC()
	return nil
}

func (dac DAC) syncDAC() {
	switch dac.Channel {
	case 0:
		for !sam.DAC.STATUS.HasBits(sam.DAC_STATUS_EOC0) {
		}
		for sam.DAC.SYNCBUSY.HasBits(sam.DAC_SYNCBUSY_DATA0) {
		}
	default:
		for !sam.DAC.STATUS.HasBits(sam.DAC_STATUS_EOC1) {
		}
		for sam.DAC.SYNCBUSY.HasBits(sam.DAC_SYNCBUSY_DATA1) {
		}
	}
}

// GetRNG returns 32 bits of cryptographically secure random data
func GetRNG() (uint32, error) {
	if !sam.MCLK.APBCMASK.HasBits(sam.MCLK_APBCMASK_TRNG_) {
		// Turn on clock for TRNG
		sam.MCLK.APBCMASK.SetBits(sam.MCLK_APBCMASK_TRNG_)

		// enable
		sam.TRNG.CTRLA.Set(sam.TRNG_CTRLA_ENABLE)
	}
	for !sam.TRNG.INTFLAG.HasBits(sam.TRNG_INTFLAG_DATARDY) {
	}
	ret := sam.TRNG.DATA.Get()
	return ret, nil
}

// Flash related code
const memoryStart = 0x0

// compile-time check for ensuring we fulfill BlockDevice interface
var _ BlockDevice = flashBlockDevice{}

var Flash flashBlockDevice

type flashBlockDevice struct {
	initComplete bool
}

// ReadAt reads the given number of bytes from the block device.
func (f flashBlockDevice) ReadAt(p []byte, off int64) (n int, err error) {
	if FlashDataStart()+uintptr(off)+uintptr(len(p)) > FlashDataEnd() {
		return 0, errFlashCannotReadPastEOF
	}

	waitWhileFlashBusy()

	data := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(FlashDataStart()), uintptr(off))), len(p))
	copy(p, data)

	return len(p), nil
}

// WriteAt writes the given number of bytes to the block device.
// Data is written to the page buffer in 4-byte chunks, then saved to flash memory.
// See SAM-D5x-E5x-Family-Data-Sheet-DS60001507.pdf page 591-592.
// If the length of p is not long enough it will be padded with 0xFF bytes.
// This method assumes that the destination is already erased.
func (f flashBlockDevice) WriteAt(p []byte, off int64) (n int, err error) {
	if FlashDataStart()+uintptr(off)+uintptr(len(p)) > FlashDataEnd() {
		return 0, errFlashCannotWritePastEOF
	}

	address := FlashDataStart() + uintptr(off)
	padded := flashPad(p, int(f.WriteBlockSize()))

	settings := disableFlashCache()
	defer restoreFlashCache(settings)

	waitWhileFlashBusy()

	sam.NVMCTRL.CTRLB.Set(sam.NVMCTRL_CTRLB_CMD_PBC | (sam.NVMCTRL_CTRLB_CMDEX_KEY << sam.NVMCTRL_CTRLB_CMDEX_Pos))

	waitWhileFlashBusy()

	for j := 0; j < len(padded); j += int(f.WriteBlockSize()) {
		// page buffer is 512 bytes long, but only 4 bytes can be written at once
		for k := 0; k < int(f.WriteBlockSize()); k += 4 {
			*(*uint32)(unsafe.Pointer(address + uintptr(k))) = binary.LittleEndian.Uint32(padded[j+k : j+k+4])
		}

		sam.NVMCTRL.SetADDR(uint32(address))
		sam.NVMCTRL.CTRLB.Set(sam.NVMCTRL_CTRLB_CMD_WP | (sam.NVMCTRL_CTRLB_CMDEX_KEY << sam.NVMCTRL_CTRLB_CMDEX_Pos))

		waitWhileFlashBusy()

		if err := checkFlashError(); err != nil {
			return j, err
		}

		address += uintptr(f.WriteBlockSize())
	}

	return len(padded), nil
}

// Size returns the number of bytes in this block device.
func (f flashBlockDevice) Size() int64 {
	return int64(FlashDataEnd() - FlashDataStart())
}

const writeBlockSize = 512

// WriteBlockSize returns the block size in which data can be written to
// memory. It can be used by a client to optimize writes, non-aligned writes
// should always work correctly.
func (f flashBlockDevice) WriteBlockSize() int64 {
	return writeBlockSize
}

const eraseBlockSizeValue = 8192

func eraseBlockSize() int64 {
	return eraseBlockSizeValue
}

// EraseBlockSize returns the smallest erasable area on this particular chip
// in bytes. This is used for the block size in EraseBlocks.
func (f flashBlockDevice) EraseBlockSize() int64 {
	return eraseBlockSize()
}

// EraseBlocks erases the given number of blocks. An implementation may
// transparently coalesce ranges of blocks into larger bundles if the chip
// supports this. The start and len parameters are in block numbers, use
// EraseBlockSize to map addresses to blocks.
func (f flashBlockDevice) EraseBlocks(start, len int64) error {
	address := FlashDataStart() + uintptr(start*f.EraseBlockSize())

	settings := disableFlashCache()
	defer restoreFlashCache(settings)

	waitWhileFlashBusy()

	for i := start; i < start+len; i++ {
		sam.NVMCTRL.SetADDR(uint32(address))
		sam.NVMCTRL.CTRLB.Set(sam.NVMCTRL_CTRLB_CMD_EB | (sam.NVMCTRL_CTRLB_CMDEX_KEY << sam.NVMCTRL_CTRLB_CMDEX_Pos))

		waitWhileFlashBusy()

		if err := checkFlashError(); err != nil {
			return err
		}

		address += uintptr(f.EraseBlockSize())
	}

	return nil
}

func disableFlashCache() uint16 {
	settings := sam.NVMCTRL.CTRLA.Get()

	// disable caches
	sam.NVMCTRL.SetCTRLA_CACHEDIS0(1)
	sam.NVMCTRL.SetCTRLA_CACHEDIS1(1)

	waitWhileFlashBusy()

	return settings
}

func restoreFlashCache(settings uint16) {
	sam.NVMCTRL.CTRLA.Set(settings)
	waitWhileFlashBusy()
}

func waitWhileFlashBusy() {
	for sam.NVMCTRL.GetSTATUS_READY() != sam.NVMCTRL_STATUS_READY {
	}
}

var (
	errFlashADDRE   = errors.New("errFlashADDRE")
	errFlashPROGE   = errors.New("errFlashPROGE")
	errFlashLOCKE   = errors.New("errFlashLOCKE")
	errFlashECCSE   = errors.New("errFlashECCSE")
	errFlashNVME    = errors.New("errFlashNVME")
	errFlashSEESOVF = errors.New("errFlashSEESOVF")
)

func checkFlashError() error {
	switch {
	case sam.NVMCTRL.GetINTENSET_ADDRE() != 0:
		return errFlashADDRE
	case sam.NVMCTRL.GetINTENSET_PROGE() != 0:
		return errFlashPROGE
	case sam.NVMCTRL.GetINTENSET_LOCKE() != 0:
		return errFlashLOCKE
	case sam.NVMCTRL.GetINTENSET_ECCSE() != 0:
		return errFlashECCSE
	case sam.NVMCTRL.GetINTENSET_NVME() != 0:
		return errFlashNVME
	case sam.NVMCTRL.GetINTENSET_SEESOVF() != 0:
		return errFlashSEESOVF
	}

	return nil
}

// Watchdog provides access to the hardware watchdog available
// in the SAMD51.
var Watchdog = &watchdogImpl{}

const (
	// WatchdogMaxTimeout in milliseconds (16s)
	WatchdogMaxTimeout = (16384 * 1000) / 1024 // CYC16384/1024kHz
)

type watchdogImpl struct{}

// Configure the watchdog.
//
// This method should not be called after the watchdog is started and on
// some platforms attempting to reconfigure after starting the watchdog
// is explicitly forbidden / will not work.
func (wd *watchdogImpl) Configure(config WatchdogConfig) error {
	// 1.024kHz clock
	cycles := int((int64(config.TimeoutMillis) * 1024) / 1000)

	// period is expressed as a power-of-two, starting at 8 / 1024ths of a second
	period := uint8(0)
	cfgCycles := 8
	for cfgCycles < cycles {
		period++
		cfgCycles <<= 1

		if period >= 0xB {
			break
		}
	}

	sam.WDT.CONFIG.Set(period << sam.WDT_CONFIG_PER_Pos)

	return nil
}

// Starts the watchdog.
func (wd *watchdogImpl) Start() error {
	sam.WDT.CTRLA.SetBits(sam.WDT_CTRLA_ENABLE)
	return nil
}

// Update the watchdog, indicating that `source` is healthy.
func (wd *watchdogImpl) Update() {
	sam.WDT.CLEAR.Set(sam.WDT_CLEAR_CLEAR_KEY)
}
