package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// randomPort returns a random UDP port in [1024,65535].
func randomPort() int {
	const base = 1024
	const max = 65535
	return base + rand.Intn(max-base+1)
}

const localhostAddr = "127.0.0.1" // Address to listen on and connect to in tests.

// listenOnRandomPort tries listening on a randomly allocated UDP port on localhostAddr and returns
// the host:port pair along with a function that cancels the listening goroutine.
func listenOnRandomPort(t *testing.T, bulbs map[string]*bulb) (string, func(), error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	listenTries := 0
	errc := make(chan error)

retry:
	port := randomPort()
	go func() { errc <- listenAndProcessBulbs(ctx, localhostAddr, port, bulbs) }()
	select {
	case err := <-errc:
		var operr *net.OpError
		if errors.As(err, &operr) && operr.Op == "listen" {
			if listenTries < 5 {
				listenTries++
				log.Infof("Can't listen: %v. Retrying on a different port (attempt #%d).", err, listenTries)
				goto retry
			}
		}
		cancel()
		return "", nil, fmt.Errorf("can't listen (tried %d times): %w", listenTries, err)

	case <-time.After(500 * time.Millisecond):
               // listenAndProcessBulbs does not signal when it’s ready, so assume that after 500ms without
               // any error it is actually serving.
	}

	return net.JoinHostPort(localhostAddr, fmt.Sprintf("%d", port)), cancel, nil
}

// testGPIO is a GPIO for testing that records the values set.
type testGPIO struct {
	values []uint16
}

// init does nothing.
func (g *testGPIO) init() error {
	return nil
}

// set records the set value to g.values.
func (g *testGPIO) set(value uint16) {
	g.values = append(g.values, value)
}

// isPWM returns false.
func (g *testGPIO) isPWM() bool {
	return false
}

// TestListenAndProcessBulbs tests listenAndProcessBulbs with some sample binary messages captured
// from a Logitech Harmony.
func TestListenAndProcessBulbs(t *testing.T) {
       // Fake bulb (non-PWM) with a recording testGPIO. We can only have one bulb, because we’re only
       // listening on one address and that’s the unique identifier for a bulb. (The MAC address is
       // ignored.)
	testg := &testGPIO{}
	bulbs := map[string]*bulb{
		"onoff": &bulb{
			id:     "onoff",
			name:   "On-Off Bulb",
			addr:   net.ParseIP(localhostAddr),
			hwaddr: 0x123456789abc,
			g:      testg,
			s:      newBulbState("On-Off Bulb"),
		},
	}

       // Start server on a random UDP port in another goroutine.
	server, cancel, err := listenOnRandomPort(t, bulbs)
	if err != nil {
		t.Fatalf("Can't listen on a random port: %v", err)
	}
	defer cancel()

       // Connect back.
	conn, err := net.Dial("udp", server)
	if err != nil {
		t.Fatalf("Can't connect back to %q: %v", server, err)
	}
       defer conn.Close()

       // The tests to be run against the server.
	tests := []struct {
		name       string
		req        []byte
		wantResp   [][]byte
		wantValues []uint16
	}{
		{
			name: "broadcast get",
			req: []byte(
				"\x24\x00" + // length
					"\x00\x34" + // protocol 1024, tagged = 1, addressable = 1, origin = 0
					"\x6c\x6f\x67\x69" + // source identifier
					"\x00\x00\x00\x00\x00\x00\x00\x00" + // target address (broadcast) + 0-padding
					"\x00\x00\x00\x00\x00\x00" + // reserved1
					"\x02" + // response required = 0, ack required = 1
					"\x6c" + // sequence number
					"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
					"\x65" + // command 101, get
					"\x00\x00\x00"), // reserved3
			wantResp: [][]byte{
				[]byte( // ACK packet
					"\x24\x00" + // length
						"\x00\x14" + // protocol 1024, tagged = 0, addressable = 1
						"\x6c\x6f\x67\x69" + // source identifier
						"\xbc\x9a\x78\x56\x34\x12\x00\x00" + // target (bulb) address + 0-padding
						"\x00\x00\x00\x00\x00\x00" + // reserved1
						"\x00" + // no response required, no ack required
						"\x6c" + // sequence number
						"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
						"\x2d" + // command 45, acknowledgement
						"\x00\x00\x00"), // reserved3
				[]byte( // State packet
					"\x58\x00" + // length
                                               "\x00\x14" + // protocol 1024, tagged = 0, addressable = 1,
						"\x6c\x6f\x67\x69" + // source identifier
						"\xbc\x9a\x78\x56\x34\x12\x00\x00" + // target (bulb) address + 0-padding
						"\x00\x00\x00\x00\x00\x00" + // reserved1
						"\x00" + // no response required, no ack required
						"\x6c" + // sequence number
						"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
						"\x6b" + // command 107, state
						"\x00\x00\x00" + // reserved3
						"\x00\x00\x00\x00\x00\x00\xac\x0d\x00\x00" + // HSBK state
						"\x00\x00" + // Power level
						"On-Off Bulb\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" + // Label + padding to 32 bytes
						"\x00\x00\x00\x00\x00\x00\x00\x00"), // reserved4
			},
		}, {
			name: "broadcast getversion",
			req: []byte(
				"\x24\x00" + // length
					"\x00\x34" + // protocol 1024, tagged = 1, addressable = 1,
					"\x6c\x6f\x67\x69" + // source idenfier
					"\x00\x00\x00\x00\x00\x00\x00\x00" + // target (broadcast) address + 0-padding
					"\x00\x00\x00\x00\x00\x00" + // reserved1
					"\x02" + // response required = 0, ack required = 1
					"\x6d" + // sequence number
					"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
					"\x20" + // command 32, get version
					"\x00\x00\x00"), // reserved3
			wantResp: [][]byte{
				[]byte( // ACK packet
					"\x24\x00" + // length
						"\x00\x14" + // protocol 1024, tagged = 0, addressable = 1
						"\x6c\x6f\x67\x69" + // source identifier
						"\xbc\x9a\x78\x56\x34\x12\x00\x00" + // target (bulb) address + 0-padding
						"\x00\x00\x00\x00\x00\x00" + // reserved1
						"\x00" + // no response required, no ack required
						"\x6d" + // sequence number
						"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
						"\x2d" + // command 45, acknowledgement
						"\x00\x00\x00"), // reserved3
				[]byte( // StateVersion response
					"\x30\x00" + // length
						"\x00\x14" + // protocol 1024, tagged = 0, addressable = 1
						"\x6c\x6f\x67\x69" + // source identifier
						"\xbc\x9a\x78\x56\x34\x12\x00\x00" + // target (bulb) address + 0-padding
						"\x00\x00\x00\x00\x00\x00" + // reserved1
						"\x00" + // no response required, no ack required
						"\x6d" + // sequence number
						"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
						"\x21" + // command 33, state version
						"\x00\x00\x00" + // reserved
						"\x01\x00\x00\x00" + // vendor ID 1
						"\x0b\x00\x00\x00" + // product ID 11 (“White 800”)
						"\x00\x00\x00\x00"), // hardware version 0
			},
		}, {
			name: "turn on bulb",
			req: []byte("\x2a\x00" + // length
                               "\x00\x14" + // protocol 1024, tagged = 0, addressable = 1
				"\x6c\x6f\x67\x69" + // source identifier
				"\x02\x90\x78\x45\x34\x12\x00\x00" + // target (bulb) address + 0-padding
				"\x00\x00\x00\x00\x00\x00" + //reserved1
				"\x02" + // response required = 0, ack required = 1
				"\x73" + // sequence number
				"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
				"\x75" + // command 117, set power
				"\x00\x00\x00" + // reserved
				"\xff\xff" + // power level
				"\xe8\x03\x00\x00"), // transition time
			wantResp: [][]byte{[]byte(
				"\x24\x00" + //length
                                       "\x00\x14" + //protocol 1024, tagged = 0, addressable = 1
					"\x6c\x6f\x67\x69" + // source identifier
					"\xbc\x9a\x78\x56\x34\x12\x00\x00" + // target (bulb) address + 0-padding
					"\x00\x00\x00\x00\x00\x00" + // reserved1
					"\x00" + // no response, no ack
					"\x73" + // sequence number
					"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
					"\x2d" + // command 45, acknowledgement
					"\x00\x00\x00"), // reserved2
			},
			wantValues: []uint16{0xffff},
		}, {
			name: "dim bulb",
			req: []byte("\x2a\x00" + // length
                               "\x00\x14" + // protocol 1024, tagged = 0, addressable = 1
				"\x6c\x6f\x67\x69" + // source identifier
				"\x02\x90\x78\x45\x34\x12\x00\x00" + // target (bulb) address + 0-padding
				"\x00\x00\x00\x00\x00\x00" + //reserved1
				"\x02" + // response required = 0, ack required = 1
				"\x74" + // sequence number
				"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
				"\x75" + // command 117, set power
				"\x00\x00\x00" + // reserved
				"\xcd\xab" + // power level
				"\xe8\x03\x00\x00"), // transition time
			wantResp: [][]byte{[]byte(
				"\x24\x00" + //length
                                       "\x00\x14" + //protocol 1024, tagged = 0, addressable = 1
					"\x6c\x6f\x67\x69" + // source identifier
					"\xbc\x9a\x78\x56\x34\x12\x00\x00" + // target (bulb) address + 0-padding
					"\x00\x00\x00\x00\x00\x00" + // reserved1
					"\x00" + // no response, no ack
					"\x74" + // sequence number
					"\x00\x00\x00\x00\x00\x00\x00\x00" + // reserved2
					"\x2d" + // command 45, acknowledgement
					"\x00\x00\x00"), // reserved2
			},
			wantValues: []uint16{0xffff, 0xabcd},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
                       // Send request.
			n, err := conn.Write(tc.req)
			if err != nil {
				t.Fatalf("Can't write request to server: %v", err)
			}
			if n != len(tc.req) {
				t.Fatalf("Write only wrote %d bytes, want %d", n, len(tc.req))
			}

                       // Check expected responses in order.
			for i, want := range tc.wantResp {
				buf := make([]byte, 1024)
				n, err := conn.Read(buf)
				if err != nil {
					t.Fatalf("Can't read response from server: %v", err)
				}
				buf = buf[:n]

				if diff := cmp.Diff(want, buf); diff != "" {
					t.Errorf("Response #%d from server differs (-want +got):\n%s", i, diff)
				}
			}

                       // Check recorded GPIO values.
			if diff := cmp.Diff(testg.values, tc.wantValues); diff != "" {
				t.Errorf("gpio.set() called with invalid values (-want +got):\n%s", diff)
			}
		})
	}
}
