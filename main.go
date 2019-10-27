package main

import (
	"errors"
	"flag"
	"fmt"
	"net"

	rpio "github.com/stianeikeland/go-rpio"
	gcfg "gopkg.in/gcfg.v1"
	logging "gopkg.in/op/go-logging.v1"
)

// Flags. Values in the configuration file take precedence (TODO: fix this).
var (
	configFile = flag.String("config_file", "lc.conf", "Path to the configuration file")
	port       = flag.Int("port", 56700, "UDP port to listen on")
	listenAddr = flag.String("listen_addr", "0.0.0.0", "IPv4 address to listen on for broadcasts")
)

// Logger.
var log = logging.MustGetLogger("lect")

// initGPIOs sets up the given GPIO pins. The returned cleanup function must be called at the end of
// the program.
func initGPIOs(gpios []*gpio) (func(), error) {
	if err := rpio.Open(); err != nil {
		return nil, fmt.Errorf("can't open raspberry GPIO: %v", err)
	}
	for _, g := range gpios {
		if err := g.init(); err != nil {
			return nil, fmt.Errorf("can't initialize GPIO: %v", err)
		}
	}
	return func() { rpio.Close() }, nil
}

// gpio is a gpio port that may optionally be active low x-or a PWM port.
type gpio struct {
	port      int // BCM notation, i.e. “gpio -g”.
	activeLow bool
	isPWM     bool
	pin       *rpio.Pin
}

// String implements fmt.Stringer
func (g *gpio) String() string {
	return fmt.Sprintf("%T%#v", g, g)
}

// init sets up a GPIO pin for use and sets its value to 0.
func (g *gpio) init() error {
	if g.pin != nil {
		return fmt.Errorf("gpio %d already initialized", g.port)
	}
	if g.activeLow && g.isPWM {
		return fmt.Errorf("gpio %d can't be both activeLow and PWM", g.port)
	}

	p := rpio.Pin(g.port)
	if g.isPWM {
		p.Mode(rpio.Pwm)
		p.Freq(19200000 / 2) // to get divi=2, from bcm2835-1.58/src/bcm2835.h BCM2835_PWM_CLOCK_DIVIDER_2
	} else {
		p.Mode(rpio.Output)
	}
	g.pin = &p

	g.set(0)

	return nil
}

// set sets g to value. For non-PWM GPIOs, only max(uint16) is considered “on”.
func (g *gpio) set(value uint16) {
	const max = ^uint16(0)

	logPfx := fmt.Sprintf("set %v to value %d", g, value)
	if g.isPWM {
		log.Infof("%s: setting dutiness %d/%d", logPfx, value, max)
		g.pin.DutyCycle(uint32(value), uint32(max))
	} else {
		on := (value == max)
		if g.activeLow {
			on = !on
		}
		if on {
			log.Infof("%s: setting high", logPfx)
			g.pin.High()
		} else {
			log.Infof("%s: setting low", logPfx)
			g.pin.Low()
		}
	}
}

// main parses flags, reads the config, initializes GPIOs and then runs the listenAndProcessBulbs
// main loop.
func main() {
	flag.Parse()

	conf, err := readConf(*configFile)
	if err != nil {
		log.Fatalf("Can't read config %q: %v", *configFile, err)
	}

	cleanup, err := initGPIOs(conf.allGPIOs)
	if err != nil {
		log.Fatalf("can't initialize GPIOs: %v", err)
	}
	defer cleanup()

	if err := listenAndProcessBulbs(*listenAddr, *port, conf.bulbs); err != nil {
		log.Fatalf("Error while processing: %v", err)
	}
}

// config represents the configurations with all bulbs and serialPorts attached to a gpio.
type config struct {
	bulbs       map[string]*bulb
	serialPorts map[string]*serialPort
	allGPIOs    []*gpio
}

// readConf reads the config specified by filename and returns a map of the bulb's id
// to *bulb. Overrides flags specified in [flags] as a side-effect.
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
		allGPIOs:    make([]*gpio, 0, len(c.Bulb)+len(c.Serial)),
	}

	// Keep track of used GPIOs and store them in allGPIOs. They are all initialized at once in
	// initGPIOs().
	usedGPIOs := map[int]string{}
	useGPIO := func(g *gpio, me string) error {
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
			return nil, fmt.Errorf("can't parse %q for bulb %q as a MAC address: %v", info.MACAddress, id, err)
		}
		if len(hwaddr) != 6 {
			return nil, fmt.Errorf("MAC address %q for bulb %q has not exactly 48 bits", hwaddr, id)
		}
		var hwaddrInt uint64
		for i := 0; i < 6; i++ {
			// The endianess is probably wrong, but it’s not used on the MAC layer anyway, so who cares.
			hwaddrInt = (hwaddrInt << 8) | uint64(hwaddr[i])
		}

		g := &gpio{port: info.GPIO, activeLow: info.ActiveLow, isPWM: info.IsPWM}
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
		g := &gpio{port: info.GPIO, activeLow: true /* NRZ */, isPWM: false}
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

	// Global flags.
	if c.Flags.Port != 0 {
		*port = c.Flags.Port
	}

	if c.Flags.ListenAddr != "" {
		*listenAddr = c.Flags.ListenAddr
	}

	return ret, nil
}
