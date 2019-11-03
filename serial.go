package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// serialPort represents an emulated serial port that transmits data via a GPIO pin as it read from
// inputFifo.
type serialPort struct {
	id        string        // Subsection in the config, used as identifier in log messages
	inputFIFO string        // Named pipe to read from. Will be created by readAndProcess().
	baudRate  int           // Baud rate
	g         gpio          // Transmit GPIO.
	b         <-chan uint16 // Connected bulb, if any.
	mute      bool          // Whether connected bulb is turned off.
}

// createFIFO creates a FIFO special file if it does not already exist and returns it opened for reading.
func createFIFO(path string) (*os.File, error) {
	fi, err := os.Stat(path)
	if err != nil {
		log.Infof("FIFO %q does not exist yet, creating.", path)
		if err := syscall.Mkfifo(path, 0666); err != nil {
			return nil, fmt.Errorf("can't create FIFO %q: %w", path, err)
		}
	} else if fi.Mode()&os.ModeNamedPipe == 0 {
		return nil, fmt.Errorf("path %q exists but is not a FIFO", path)
	}

	return os.Open(path)
}

// send sends data with s.baudRate Baud, 8 data bits, no parity and one stop bit to s.g.
func (s *serialPort) send(ctx context.Context, data []byte) error {
	// Delay between two bits.
	periodNs := int64(time.Second/time.Nanosecond) / int64(s.baudRate)

	// Timestamp for sending next bit.
	var next unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &next); err != nil {
		return fmt.Errorf("can't get timestamp from monotonic clock: %w", err)
	}

	for _, b := range data {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Bits to be sent, LSB first.
		bits := (1 << 9 /* stop bit */) | (uint16(b) << 1 /* data bits */) | 0 /* start bit */
		// Current bit being sent.
		mask := uint16(1)

		for mask != uint16(1)<<10 {
			bit := ((bits & mask) == mask)
			if bit {
				s.g.set(gpioMaxVal)
			} else {
				s.g.set(0)
			}

			next = unix.NsecToTimespec(unix.TimespecToNsec(next) + periodNs)
			if err := unix.ClockNanosleep(unix.CLOCK_MONOTONIC, unix.TIMER_ABSTIME, &next, nil); err != nil {
				return fmt.Errorf("can't sleep to absolute timestamp %#v: %w", next, err)
			}

			mask <<= 1
		}
	}

	return nil
}

// readAndProcess creates the input FIFO, reads from it and sends the data to the output GPIO until
// the context is canceled.
func (s *serialPort) readAndProcess(ctx context.Context) error {
reopen:
	for {
		f, err := createFIFO(s.inputFIFO)
		if err != nil {
			return fmt.Errorf("can't open input FIFO: %w", err)
		}

		for {
			buf := make([]byte, 1024)
			n, err := f.Read(buf)
			if err != nil {
				f.Close()
				if err == io.EOF {
					continue reopen
				}
				return fmt.Errorf("while reading from %q: %w", s.inputFIFO, err)
			}
			if len(s.b) > 0 {
				s.mute = (<-s.b != gpioMaxVal)
				log.Infof("set mute = %v", s.mute)
				if s.mute {
					s.g.set(0)
				}
			}

			if s.mute {
				log.Infof("%d bytes of input for serial %q ignored because bulb is off.", n, s.id)
				continue
			}

			buf = bytes.ReplaceAll(buf[:n], []byte("\n"), []byte("\r\n"))
			log.Infof("Read %d bytes: %q", n, buf)
			if err := s.send(ctx, buf); err != nil {
				return fmt.Errorf("while sending %q: %w", buf, err)
			}
		}
	}
}
