//go:build rp2040 || rp2350

package machine

import (
	"device/rp"
	"errors"
	"unsafe"
)

// SPI on the RP2040
var (
	SPI0 = &SPI{
		Bus: rp.SPI0,
	}
	SPI1 = &SPI{
		Bus: rp.SPI1,
	}
)

// SPIConfig is used to store config info for SPI.
type SPIConfig struct {
	Frequency uint32
	// LSB not supported on rp2040.
	LSBFirst bool
	// Mode's two most LSB are CPOL and CPHA. i.e. Mode==2 (0b10) is CPOL=1, CPHA=0
	Mode uint8
	// Serial clock pin
	SCK Pin
	// TX or Serial Data Out (MOSI if rp2040 is master)
	SDO Pin
	// RX or Serial Data In (MISO if rp2040 is master)
	SDI Pin
}

var (
	ErrLSBNotSupported = errors.New("SPI LSB unsupported on PL022")
	ErrSPITimeout      = errors.New("SPI timeout")
	ErrSPIBaud         = errors.New("SPI baud too low or above 66.5Mhz")
	errSPIInvalidSDI   = errors.New("invalid SPI SDI pin")
	errSPIInvalidSDO   = errors.New("invalid SPI SDO pin")
	errSPIInvalidSCK   = errors.New("invalid SPI SCK pin")
)

type SPI struct {
	Bus *rp.SPI0_Type
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
//
// Remark: This implementation (RP2040) allows reading into buffer with a custom repeated
// value on tx.
//
//	spi.Tx([]byte{0xff}, rx) // may cause unwanted heap allocations.
//
// This form sends 0xff and puts the result into rx buffer. Useful for reading from SD cards
// which require 0xff input on SI.
func (spi *SPI) Tx(w, r []byte) (err error) {
	switch {
	case w == nil:
		// read only, so write zero and read a result.
		err = spi.rx(r, 0)
	case r == nil:
		// write only
		err = spi.tx(w)
	case len(w) == 1 && len(r) > 1:
		// Read with custom repeated value.
		err = spi.rx(r, w[0])
	default:
		// write/read
		err = spi.txrx(w, r)
	}
	return err
}

// Write a single byte and read a single byte from TX/RX FIFO.
func (spi *SPI) Transfer(w byte) (byte, error) {
	for !spi.isWritable() {
	}

	spi.Bus.SSPDR.Set(uint32(w))

	for !spi.isReadable() {
	}
	return uint8(spi.Bus.SSPDR.Get()), nil
}

func (spi *SPI) SetBaudRate(br uint32) error {
	const maxBaud uint32 = 66.5 * MHz // max output frequency is 66.5MHz on rp2040. see Note page 527.
	// Find smallest prescale value which puts output frequency in range of
	// post-divide. Prescale is an even number from 2 to 254 inclusive.
	var prescale, postdiv uint32
	freq := CPUFrequency()
	for prescale = 2; prescale < 255; prescale += 2 {
		if freq < (prescale+2)*256*br {
			break
		}
	}
	if prescale > 254 || br > maxBaud {
		return ErrSPIBaud
	}
	// Find largest post-divide which makes output <= baudrate. Post-divide is
	// an integer in the range 1 to 256 inclusive.
	for postdiv = 256; postdiv > 1; postdiv-- {
		if freq/(prescale*(postdiv-1)) > br {
			break
		}
	}
	spi.Bus.SSPCPSR.Set(prescale)
	spi.Bus.SSPCR0.ReplaceBits((postdiv-1)<<rp.SPI0_SSPCR0_SCR_Pos, rp.SPI0_SSPCR0_SCR_Msk, 0)
	return nil
}

func (spi *SPI) GetBaudRate() uint32 {
	freqin := CPUFrequency()
	prescale := spi.Bus.SSPCPSR.Get()
	postdiv := ((spi.Bus.SSPCR0.Get() & rp.SPI0_SSPCR0_SCR_Msk) >> rp.SPI0_SSPCR0_SCR_Pos) + 1
	return freqin / (prescale * postdiv)
}

// Configure is intended to setup/initialize the SPI interface.
// Default baudrate of 4MHz is used if Frequency == 0. Default
// word length (data bits) is 8.
// Below is a list of GPIO pins corresponding to SPI0 bus on the rp2040:
//
//	SI : 0, 4, 17  a.k.a RX and MISO (if rp2040 is master)
//	SO : 3, 7, 19  a.k.a TX and MOSI (if rp2040 is master)
//	SCK: 2, 6, 18
//
// SPI1 bus GPIO pins:
//
//	SI : 8, 12
//	SO : 11, 15
//	SCK: 10, 14
//
// No pin configuration is needed of SCK, SDO and SDI needed after calling Configure.
func (spi *SPI) Configure(config SPIConfig) error {
	const defaultBaud uint32 = 4 * MHz
	if config.SCK == 0 && config.SDO == 0 && config.SDI == 0 {
		// set default pins if config zero valued or invalid clock pin supplied.
		switch spi.Bus {
		case rp.SPI0:
			config.SCK = SPI0_SCK_PIN
			config.SDO = SPI0_SDO_PIN
			config.SDI = SPI0_SDI_PIN
		case rp.SPI1:
			config.SCK = SPI1_SCK_PIN
			config.SDO = SPI1_SDO_PIN
			config.SDI = SPI1_SDI_PIN
		}
	}
	var okSDI, okSDO, okSCK bool
	switch spi.Bus {
	case rp.SPI0:
		okSDI = config.SDI == 0 || config.SDI == 4 || config.SDI == 16 || config.SDI == 20
		okSDO = config.SDO == 3 || config.SDO == 7 || config.SDO == 19 || config.SDO == 23
		okSCK = config.SCK == 2 || config.SCK == 6 || config.SCK == 18 || config.SCK == 22
	case rp.SPI1:
		okSDI = config.SDI == 8 || config.SDI == 12 || config.SDI == 24 || config.SDI == 28
		okSDO = config.SDO == 11 || config.SDO == 15 || config.SDO == 27
		okSCK = config.SCK == 10 || config.SCK == 14 || config.SCK == 26
	}

	switch {
	case !okSDI:
		return errSPIInvalidSDI
	case !okSDO:
		return errSPIInvalidSDO
	case !okSCK:
		return errSPIInvalidSCK
	}

	if config.Frequency == 0 {
		config.Frequency = defaultBaud
	}
	// SPI pin configuration
	config.SCK.setFunc(fnSPI)
	config.SDO.setFunc(fnSPI)
	config.SDI.setFunc(fnSPI)

	return spi.initSPI(config)
}

func (spi *SPI) initSPI(config SPIConfig) (err error) {
	spi.reset()
	// LSB-first not supported on PL022:
	if config.LSBFirst {
		return ErrLSBNotSupported
	}
	err = spi.SetBaudRate(config.Frequency)
	// Set SPI Format (CPHA and CPOL) and frame format (default is Motorola)
	spi.setFormat(config.Mode)

	// Always enable DREQ signals -- harmless if DMA is not listening
	spi.Bus.SSPDMACR.SetBits(rp.SPI0_SSPDMACR_TXDMAE | rp.SPI0_SSPDMACR_RXDMAE)
	// Finally enable the SPI
	spi.Bus.SSPCR1.SetBits(rp.SPI0_SSPCR1_SSE)
	return err
}

//go:inline
func (spi *SPI) setFormat(mode uint8) {
	cpha := uint32(mode) & 1
	cpol := uint32(mode>>1) & 1
	spi.Bus.SSPCR0.ReplaceBits(
		(cpha<<rp.SPI0_SSPCR0_SPH_Pos)|
			(cpol<<rp.SPI0_SSPCR0_SPO_Pos)|
			(uint32(7)<<rp.SPI0_SSPCR0_DSS_Pos), // Set databits (SPI word length) to 8 bits.
		rp.SPI0_SSPCR0_SPH_Msk|rp.SPI0_SSPCR0_SPO_Msk|rp.SPI0_SSPCR0_DSS_Msk|rp.SPI0_SSPCR0_FRF_Msk, 0)
}

// reset resets SPI and waits until reset is done.
//
//go:inline
func (spi *SPI) reset() {
	resetVal := spi.deinit()
	rp.RESETS.RESET.ClearBits(resetVal)
	// Wait until reset is done.
	for !rp.RESETS.RESET_DONE.HasBits(resetVal) {
	}
}

//go:inline
func (spi *SPI) deinit() (resetVal uint32) {
	switch spi.Bus {
	case rp.SPI0:
		resetVal = rp.RESETS_RESET_SPI0
	case rp.SPI1:
		resetVal = rp.RESETS_RESET_SPI1
	}
	// Perform SPI reset.
	rp.RESETS.RESET.SetBits(resetVal)
	return resetVal
}

// isWritable returns false if no space is available to write. True if a write is possible
//
//go:inline
func (spi *SPI) isWritable() bool {
	return spi.Bus.SSPSR.HasBits(rp.SPI0_SSPSR_TNF)
}

// isReadable returns true if a read is possible i.e. data is present
//
//go:inline
func (spi *SPI) isReadable() bool {
	return spi.Bus.SSPSR.HasBits(rp.SPI0_SSPSR_RNE)
}

// PrintRegs prints SPI's peripheral common registries current values
func (spi *SPI) PrintRegs() {
	cr0 := spi.Bus.SSPCR0.Get()
	cr1 := spi.Bus.SSPCR1.Get()
	dmacr := spi.Bus.SSPDMACR.Get()
	cpsr := spi.Bus.SSPCPSR.Get()
	dr := spi.Bus.SSPDR.Get()
	ris := spi.Bus.SSPRIS.Get()
	println("CR0:", cr0)
	println("CR1:", cr1)
	println("DMACR:", dmacr)
	println("CPSR:", cpsr)
	println("DR:", dr)
	println("RIS:", ris)
}

//go:inline
func (spi *SPI) isBusy() bool {
	return spi.Bus.SSPSR.HasBits(rp.SPI0_SSPSR_BSY)
}

// tx writes buffer to SPI ignoring Rx.
func (spi *SPI) tx(tx []byte) error {
	if len(tx) == 0 {
		// We don't have to do anything.
		// This avoids a panic in &tx[0] when len(tx) == 0.
		return nil
	}

	// Pick the DMA channel reserved for this SPI peripheral.
	var ch *dmaChannel
	var dreq uint32
	if spi.Bus == rp.SPI0 {
		ch = &dmaChannels[spi0DMAChannel]
		dreq = rp.DREQ_SPI0_TX
	} else { // SPI1
		ch = &dmaChannels[spi1DMAChannel]
		dreq = rp.DREQ_SPI1_TX
	}

	// Configure the DMA peripheral as follows:
	//   - set read address, write address, and number of transfer units (bytes)
	//   - increment read address (in memory), don't increment write address (SSPDR)
	//   - set data size to single bytes
	//   - set the DREQ so that the DMA will fill the SPI FIFO as needed
	//   - start the transfer
	ch.READ_ADDR.Set(uint32(uintptr(unsafe.Pointer(&tx[0]))))
	ch.WRITE_ADDR.Set(uint32(uintptr(unsafe.Pointer(&spi.Bus.SSPDR))))
	ch.TRANS_COUNT.Set(uint32(len(tx)))
	ch.CTRL_TRIG.Set(rp.DMA_CH0_CTRL_TRIG_INCR_READ |
		rp.DMA_CH0_CTRL_TRIG_DATA_SIZE_SIZE_BYTE<<rp.DMA_CH0_CTRL_TRIG_DATA_SIZE_Pos |
		dreq<<rp.DMA_CH0_CTRL_TRIG_TREQ_SEL_Pos |
		rp.DMA_CH0_CTRL_TRIG_EN)

	// Wait until the transfer is complete.
	// TODO: do this more efficiently:
	//   - Add a new API to start the transfer, without waiting for it to
	//     complete. This way, the CPU can do something useful while the
	//     transfer is in progress.
	//   - If we have to wait, do so by waiting for an interrupt and blocking
	//     this goroutine until finished (so that other goroutines can run or
	//     the CPU can go to sleep).
	for ch.CTRL_TRIG.Get()&rp.DMA_CH0_CTRL_TRIG_BUSY != 0 {
	}

	// We didn't read any result values, which means the RX FIFO has likely
	// overflown. We have to clean up this mess now.

	// Drain RX FIFO, then wait for shifting to finish (which may be *after*
	// TX FIFO drains), then drain RX FIFO again
	for spi.isReadable() {
		spi.Bus.SSPDR.Get()
	}
	for spi.isBusy() {
	}
	for spi.isReadable() {
		spi.Bus.SSPDR.Get()
	}
	// Don't leave overrun flag set
	spi.Bus.SSPICR.Set(rp.SPI0_SSPICR_RORIC)
	return nil
}

// rx reads buffer to SPI ignoring x.
// txrepeat is output repeatedly on SO as data is read in from SI.
// Generally this can be 0, but some devices require a specific value here,
// e.g. SD cards expect 0xff
func (spi *SPI) rx(rx []byte, txrepeat byte) error {
	plen := len(rx)
	const fifoDepth = 8 // see txrx
	var rxleft, txleft = plen, plen
	for txleft != 0 || rxleft != 0 {
		if txleft != 0 && spi.isWritable() && rxleft < txleft+fifoDepth {
			spi.Bus.SSPDR.Set(uint32(txrepeat))
			txleft--
		}
		if rxleft != 0 && spi.isReadable() {
			rx[plen-rxleft] = uint8(spi.Bus.SSPDR.Get())
			rxleft--
			continue
		}
	}
	for spi.isBusy() {
		gosched()
	}
	return nil
}

// Write len bytes from src to SPI. Simultaneously read len bytes from SPI to dst.
// Note this function is guaranteed to exit in a known amount of time (bits sent * time per bit)
func (spi *SPI) txrx(tx, rx []byte) error {
	plen := len(tx)
	if plen != len(rx) {
		return ErrTxInvalidSliceSize
	}
	// Never have more transfers in flight than will fit into the RX FIFO,
	// else FIFO will overflow if this code is heavily interrupted.
	const fifoDepth = 8
	var rxleft, txleft = plen, plen
	for txleft != 0 || rxleft != 0 {
		if txleft != 0 && spi.isWritable() && rxleft < txleft+fifoDepth {
			spi.Bus.SSPDR.Set(uint32(tx[plen-txleft]))
			txleft--
		}
		if rxleft != 0 && spi.isReadable() {
			rx[plen-rxleft] = uint8(spi.Bus.SSPDR.Get())
			rxleft--
		}
	}

	if txleft != 0 || rxleft != 0 {
		// Transaction ended early due to timeout
		return ErrSPITimeout
	}
	for spi.isBusy() {
		gosched()
	}
	return nil
}
