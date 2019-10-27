package main

import (
	"errors"
	"flag"
	"fmt"
	"net"

	rpio "github.com/stianeikeland/go-rpio"
	"github.com/yath/controlifx"
	"golang.org/x/net/ipv4"
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

// listenAndProcess is the main loop that listens on the specified UDP port and
// invokes each addressed bulb’s process() receiver.
func listenAndProcess(addr string, port int, bulbs map[string]*bulb) error {
	hp := net.JoinHostPort(addr, fmt.Sprintf("%d", port))
	conn, err := net.ListenPacket("udp", hp)
	if err != nil {
		return fmt.Errorf("can't listen: %v", err)
	}

	pc := ipv4.NewPacketConn(conn)
	pc.SetControlMessage(ipv4.FlagDst, true)

	log.Infof("listening on %s", hp)

	for {
		buf := make([]byte, controlifx.MaxReadSize)
		n, cm, saddr, err := pc.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("can't read: %v", err)
		}
		buf = buf[:n]

		found := false
		for id, b := range bulbs {
			if net.IPv4bcast.Equal(cm.Dst) || b.addr.Equal(cm.Dst) {
				found = true
				if err := b.process(pc, saddr, buf); err != nil {
					log.Errorf("error processing packet for bulb %q: %v", id, err)
				}
			}
		}

		if !found {
			log.Warningf("%d bytes from %v received for %v, but is neither broadcast nor a bulb", n,
				saddr, cm.Dst)
		}
	}
}

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

// main parses flags, reads the config, initializes GPIOs and then runs the listenAndProcess main loop.
func main() {
	flag.Parse()

	bulbs, err := readConf(*configFile)
	if err != nil {
		log.Fatalf("Can't read config %q: %v", *configFile, err)
	}

	gpios := make([]*gpio, 0, len(bulbs))
	for _, b := range bulbs {
		gpios = append(gpios, b.g)
	}
	cleanup, err := initGPIOs(gpios)
	if err != nil {
		log.Fatalf("can't initialize GPIOs: %v", err)
	}
	defer cleanup()

	if err := listenAndProcess(*listenAddr, *port, bulbs); err != nil {
		log.Fatalf("Error while processing: %v", err)
	}
}

// readConf reads the config specified by filename and returns a map of the bulb's id
// to *bulb. Overrides flags specified in [flags] as a side-effect.
func readConf(filename string) (map[string]*bulb, error) {
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
	}
	if err := gcfg.ReadFileInto(&c, filename); err != nil {
		return nil, err
	}

	if len(c.Bulb) < 1 {
		return nil, errors.New("no bulbs defined")
	}

	ret := make(map[string]*bulb, len(c.Bulb))
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

		ret[id] = &bulb{
			id:     id,
			name:   info.Name,
			addr:   addr,
			hwaddr: hwaddrInt,
			g:      &gpio{port: info.GPIO, activeLow: info.ActiveLow, isPWM: info.IsPWM},
			s:      newBulbState(info.Name),
		}
	}

	if c.Flags.Port != 0 {
		*port = c.Flags.Port
	}

	if c.Flags.ListenAddr != "" {
		*listenAddr = c.Flags.ListenAddr
	}

	return ret, nil
}
