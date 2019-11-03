package main

import (
	"io/ioutil"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestReadConf(t *testing.T) {
	tests := []struct {
		name             string
		conf             string
		want             *config
		wantErrSubstring string
	}{
		{
			name: "regular config",
			conf: `[bulb "foo"]
					name = Foo
					ipaddress = 1.2.3.4
					macaddress = 12:34:56:78:90:01
					gpio = 8
					activelow = true

					[bulb "bar"]
					name = Bar
					ipaddress = 2.3.4.5
					macaddress = 12:34:56:78:90:02
					gpio = 25
					ispwm = true

					[serial "foo"]
					inputfifo = /tmp/serial.fifo
					baudrate = 9600
					gpio = 17`,
			want: &config{
				bulbs: map[string]*bulb{
					"foo": &bulb{
						id:     "foo",
						name:   "Foo",
						addr:   net.ParseIP("1.2.3.4"),
						hwaddr: 0x123456789001,
						g:      &piGPIO{port: 8, activeLow: true},
						s:      &bulbState{}, // ignored by cmp
					},
					"bar": &bulb{
						id:     "bar",
						name:   "Bar",
						addr:   net.ParseIP("2.3.4.5"),
						hwaddr: 0x123456789002,
						g:      &piGPIO{port: 25, pwm: true},
						s:      &bulbState{}, // ignored by cmp
					},
				},
				serialPorts: map[string]*serialPort{
					"foo": &serialPort{
						id:        "foo",
						inputFIFO: "/tmp/serial.fifo",
						baudRate:  9600,
						g:         &piGPIO{port: 17},
					},
				},
				allGPIOs: []gpio{
					&piGPIO{port: 8, activeLow: true},
					&piGPIO{port: 17},
					&piGPIO{port: 25, pwm: true},
				},
			},
		},
		{
			name: "duplicate gpio",
			conf: `[bulb "foo"]
					name = Foo
					ipaddress = 1.2.3.4
					macaddress = 12:34:56:78:90:01
					gpio = 2

					[bulb "bar"]
					name = Bar
					ipaddress = 2.3.4.5
					macaddress = 12:34:56:78:90:02
					gpio = 2`,
			want:             nil,
			wantErrSubstring: "already used by bulb",
		},
		{
			name: "missing macaddress",
			conf: `[bulb "foo"]
					name = Foo
					ipaddress = 1.2.3.4
					gpio = 2`,
			want:             nil,
			wantErrSubstring: `are required for bulb "foo"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpf, err := ioutil.TempFile("", "*.conf")
			if err != nil {
				t.Fatalf("Can't create temporary config file: %v", err)
			}
			tmpfn := tmpf.Name()
			defer os.Remove(tmpfn)

			if _, err := tmpf.WriteString(tc.conf); err != nil {
				t.Fatalf("Can't write %q to temporary file %q: %v", tc.conf, tmpfn, err)
			}

			conf, err := readConf(tmpfn)
			cmpOpts := []cmp.Option{
				cmp.AllowUnexported(config{}, piGPIO{}, bulb{}, serialPort{}),
				cmpopts.IgnoreUnexported(bulbState{}),
			}
			if diff := cmp.Diff(tc.want, conf, cmpOpts...); diff != "" {
				t.Errorf("readConf() mismatch (-want +got):\n%s", diff)
			}

			if tc.wantErrSubstring != "" && !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("readConf() returned err = %+v, want substring %q", err, tc.wantErrSubstring)
			}
		})
	}
}
