package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// timerGPIO is a GPIO for testing that records the timestamps when set() was called.
type timerGPIO struct {
	n          int
	timestamps []unix.Timespec
	values     []uint16
}

// newTimerGPIO returns a pre-allocated *timerGPIO in order to avoid allocations during set().
func newTimerGPIO(n int) *timerGPIO {
	nbits := 10 * n
	return &timerGPIO{
		n:          0,
		timestamps: make([]unix.Timespec, nbits),
		values:     make([]uint16, nbits),
	}
}

// init does nothing.
func (g *timerGPIO) init() error {
	return nil
}

// set records the set value and timestamp into g.
func (g *timerGPIO) set(value uint16) {
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &g.timestamps[g.n]); err != nil {
		log.Panicf("Can't get current monotonic clock timestamp: %v", err)
	}
	g.values[g.n] = value
	g.n++
}

// isPWM returns false.
func (g *timerGPIO) isPWM() bool {
	return false
}

// mkvals returns data split into individual bits, each byte surrounded with start and stop bit and
// mapped to 0 and gpioMaxVal.
func mkvals(data []byte) []uint16 {
	ret := make([]uint16, 0, 10*len(data))
	for _, b := range data {
		ret = append(ret, 0) // start bit
		for i := 0; i < 8; i++ {
			if b&1 == 1 {
				ret = append(ret, 0xffff)
			} else {
				ret = append(ret, 0)
			}
			b >>= 1
		}
		ret = append(ret, 0xffff) // stop bit
	}
	return ret
}

// TestSend tests send.
func TestSend(t *testing.T) {
	const tolerancePercent = 50 // ugh

	data := []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	for _, baud := range []int{4800} {
		t.Run(fmt.Sprintf("%d baud", baud), func(t *testing.T) {
			g := newTimerGPIO(len(data))
			s := &serialPort{baudRate: baud, g: g}
			if err := s.send(context.Background(), data); err != nil {
				t.Fatalf("can't send %d bytes of data: %v", len(data), err)
			}

			want := mkvals(data)
			if diff := cmp.Diff(want, g.values[:g.n]); diff != "" {
				t.Errorf("gpio.set() called with wrong values (-want +got):\n%s", diff)
			}

			periodNs := int64(time.Second/time.Nanosecond) / int64(baud)
			toleranceNs := periodNs / int64(100/tolerancePercent)
			tolLowNs := periodNs - toleranceNs
			tolHighNs := periodNs + toleranceNs
			var prev int64
			for i := 0; i < g.n; i++ {
				this := unix.TimespecToNsec(g.timestamps[i])
				if prev != 0 {
					delta := this - prev
					if delta < tolLowNs || delta > tolHighNs {
						t.Errorf("gpio.set() for bit #%d called %dns after previous, want after [%d, %d] ns", i, delta, tolLowNs, tolHighNs)
					}
				}
				prev = this
			}
		})
	}
}

// TestReadAndProcess tests readAndProcess.
func TestReadAndProcess(t *testing.T) {
	dir, err := ioutil.TempDir("", "testing")
	if err != nil {
		t.Fatalf("Can't create temporary directory: %v", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fifo := filepath.Join(dir, "testfifo")

	g := newTimerGPIO(10)
	s := &serialPort{
		id:        "testing",
		inputFIFO: fifo,
		baudRate:  9600,
		g:         g,
	}
	errc := make(chan error)
	go func() {
		errc <- s.readAndProcess(ctx)
	}()

	select {
	case err := <-errc:
		t.Fatalf("readAndProcess failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := ioutil.WriteFile(fifo, []byte("foobar\n"), 0666); err != nil {
		t.Fatalf("Can't write to fifo %q: %v", fifo, err)
	}

	// Symbol period for 9600 Baud is ~1ms, so 10 symbols * 10 bytes = 100ms. 500ms should be
	// sufficient.
	time.Sleep(500 * time.Millisecond)

	want := mkvals([]byte("foobar\r\n"))
	if diff := cmp.Diff(want, g.values[:g.n]); diff != "" {
		t.Errorf("gpio.set() called with wrong values (-want +got):\n%s", diff)
	}
}
