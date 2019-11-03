package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"sort"

	rpio "github.com/stianeikeland/go-rpio"
	"golang.org/x/sync/errgroup"
	gcfg "gopkg.in/gcfg.v1"
	logging "gopkg.in/op/go-logging.v1"
)

// Flags. Values in the configuration file take precedence (TODO: fix this).
var (
	configFile = flag.String("config_file", "lc.conf", "Path to the configuration file")
	port       = flag.Int("port", 56700, "UDP port to listen on")
	listenAddr = flag.String("listen_addr", "0.0.0.0", "IPv4 address to listen on for broadcasts")
	isPi       = flag.Bool("is_pi", true, "Assume running on a Raspberry Pi. If false, GPIOs will not be used.")
)

// Logger.
var log = logging.MustGetLogger("lect")

// gpioMaxVal is the maximum value a GPIO can be set to and for non-PWM GPIOs is the only “on”
// value.
const gpioMaxVal = ^uint16(0)

// gpio is a generic GPIO port that can be set to a 16-bit value.
type gpio interface {
	init() error
	set(value uint16)
	isPWM() bool
}

// gpio is a Raspberry Pi hardware GPIO port that may optionally be active low x-or a PWM port.
type piGPIO struct {
	port      int // BCM notation, i.e. “gpio -g”.
	activeLow bool
	pwm       bool
	pin       *rpio.Pin
}

// String implements fmt.Stringer.
func (g *piGPIO) String() string {
	return fmt.Sprintf("%T%#v", g, g)
}

// init sets up a GPIO pin for use and sets its value to 0.
func (g *piGPIO) init() error {
	if !*isPi {
		return nil
	}

	if g.pin != nil {
		return fmt.Errorf("gpio %d already initialized", g.port)
	}
	if g.activeLow && g.pwm {
		return fmt.Errorf("gpio %d can't be both activeLow and PWM", g.port)
	}

	p := rpio.Pin(g.port)
	if g.pwm {
		p.Mode(rpio.Pwm)
		p.Freq(19200000 / 2) // to get divi=2, from bcm2835-1.58/src/bcm2835.h BCM2835_PWM_CLOCK_DIVIDER_2
	} else {
		p.Mode(rpio.Output)
	}
	g.pin = &p

	g.set(0)

	return nil
}

// isPWM returns whether g supports pulse-width modulation.
func (g *piGPIO) isPWM() bool {
	return g.pwm
}

// set sets g to value.
func (g *piGPIO) set(value uint16) {
	if g.pin == nil {
		log.Warningf("gpio %v pin not initialized, not setting 0x%04x", g, value)
		return
	}

	if g.pwm {
		log.Infof("%v: setting dutiness %d/%d", g, value, gpioMaxVal)
		g.pin.DutyCycle(uint32(value), uint32(gpioMaxVal))
	} else {
		on := (value == gpioMaxVal)
		if g.activeLow {
			on = !on
		}
		if on {
			g.pin.High()
		} else {
			g.pin.Low()
		}
	}
}

// config represents the configurations with all bulbs and serialPorts attached to a gpio.
type config struct {
	bulbs       map[string]*bulb
	serialPorts map[string]*serialPort
	allGPIOs    []gpio
}

// readConf reads the config specified by filename and returns a new *config. Overrides flags
// specified in [flags] as a side-effect.
func readConf(filename string) (*config, error) {
	var c struct {
		Flags struct {
			Port       int
			ListenAddr string
		}
		Bulb map[string]*struct {
			Name       string
			IPAddress  string
			MACAddress string
			GPIO       int
			ActiveLow  bool
			IsPWM      bool
		}
		Serial map[string]*struct {
			InputFIFO string
			BaudRate  int
			GPIO      int
		}
	}
	if err := gcfg.ReadFileInto(&c, filename); err != nil {
		return nil, err
	}

	if len(c.Bulb)+len(c.Serial) == 0 {
		return nil, errors.New("no bulb or serial port defined")
	}

	ret := &config{
		bulbs:       make(map[string]*bulb, len(c.Bulb)),
		serialPorts: make(map[string]*serialPort, len(c.Serial)),
		allGPIOs:    make([]gpio, 0, len(c.Bulb)+len(c.Serial)),
	}

	// Keep track of used GPIOs and store them in allGPIOs. They are all initialized at once in
	// initGPIOs().
	usedGPIOs := map[int]string{}
	useGPIO := func(g *piGPIO, me string) error {
		old, ok := usedGPIOs[g.port]
		if !ok {
			usedGPIOs[g.port] = me
			ret.allGPIOs = append(ret.allGPIOs, g)
			return nil
		}
		return fmt.Errorf("GPIO %d for %s already used by %s", g.port, me, old)
	}

	// Parse bulbs.
	for id, info := range c.Bulb {
		if info.Name == "" || info.IPAddress == "" || info.MACAddress == "" || info.GPIO == 0 {
			return nil, fmt.Errorf("all of name, ipaddress, macaddress and gpio are required for bulb %q", id)
		}
		addr := net.ParseIP(info.IPAddress)
		if addr == nil {
			return nil, fmt.Errorf("invalid IP address %q for bulb %q", info.IPAddress, id)
		}
		hwaddr, err := net.ParseMAC(info.MACAddress)
		if err != nil {
			return nil, fmt.Errorf("can't parse %q for bulb %q as a MAC address: %w", info.MACAddress, id, err)
		}
		if len(hwaddr) != 6 {
			return nil, fmt.Errorf("MAC address %q for bulb %q has not exactly 48 bits", hwaddr, id)
		}
		var hwaddrInt uint64
		for i := 0; i < 6; i++ {
			// The endianess is probably wrong, but it’s not used on the MAC layer anyway, so who cares.
			hwaddrInt = (hwaddrInt << 8) | uint64(hwaddr[i])
		}

		g := &piGPIO{port: info.GPIO, activeLow: info.ActiveLow, pwm: info.IsPWM}
		if err := useGPIO(g, fmt.Sprintf("bulb %q", id)); err != nil {
			return nil, err
		}

		ret.bulbs[id] = &bulb{
			id:     id,
			name:   info.Name,
			addr:   addr,
			hwaddr: hwaddrInt,
			g:      g,
			s:      newBulbState(info.Name),
		}
	}

	// Parse serial ports.
	for id, info := range c.Serial {
		g := &piGPIO{port: info.GPIO, pwm: false}
		if err := useGPIO(g, fmt.Sprintf("serial %q", id)); err != nil {
			return nil, err
		}

		ret.serialPorts[id] = &serialPort{
			id:        id,
			inputFIFO: info.InputFIFO,
			baudRate:  info.BaudRate,
			g:         g,
		}

	}

	// For stable comparison in test.
	sort.Slice(ret.allGPIOs, func(i, j int) bool {
		return ret.allGPIOs[i].(*piGPIO).port < (ret.allGPIOs[j]).(*piGPIO).port
	})

	// Global flags.
	if c.Flags.Port != 0 {
		*port = c.Flags.Port
	}

	if c.Flags.ListenAddr != "" {
		*listenAddr = c.Flags.ListenAddr
	}

	return ret, nil
}

// initGPIOs sets up the given GPIO pins. The returned cleanup function must be called at the end of
// the program.
func initGPIOs(gpios []gpio) (func(), error) {
	if err := rpio.Open(); err != nil {
		err = fmt.Errorf("can't open raspberry GPIO: %w", err)
		if *isPi {
			return nil, err
		}
		log.Warningf("%v (ignored)", err)
	}
	for _, g := range gpios {
		if err := g.init(); err != nil {
			return nil, fmt.Errorf("can't initialize GPIO: %w", err)
		}
	}
	return func() { rpio.Close() }, nil
}

// main parses flags, reads the config, initializes GPIOs and then runs the listenAndProcessBulbs
// and serialPorts main loops.
func main() {
	ctx := context.Background()
	flag.Parse()

	conf, err := readConf(*configFile)
	if err != nil {
		log.Fatalf("Can't read config %q: %v", *configFile, err)
	}

	cleanup, err := initGPIOs(conf.allGPIOs)
	if err != nil {
		log.Fatalf("can't initialize GPIOs: %v", err)
	}
	// cleanup() is called unconditionally after eg.Wait(), because the log.Fatalf would not trigger
	// a deferred call.

	ignoreCanceled := func(e error) error {
		if errors.Is(e, context.Canceled) {
			return nil
		}
		return e
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		if err := ignoreCanceled(listenAndProcessBulbs(ctx, *listenAddr, *port, conf.bulbs)); err != nil {
			return fmt.Errorf("can't process bulb messages: %w", err)
		}
		return nil
	})

	for id, sp := range conf.serialPorts {
		eg.Go(func() error {
			if err := ignoreCanceled(sp.readAndProcess(ctx)); err != nil {
				return fmt.Errorf("can't process serial port messages for %q: %w", id, err)
			}
			return nil
		})
	}

	err = eg.Wait()
	cleanup()
	if err != nil {
		log.Fatalf(err.Error())
	}
}
