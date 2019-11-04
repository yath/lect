package main

import (
	"context"
	"encoding"
	"fmt"
	"net"
	"time"

	"github.com/yath/controlifx"
	"github.com/yath/implifx"
	"golang.org/x/net/ipv4"
)

// listenAndProcess is the main loop that listens on the specified address and UDP port and invokes
// each addressed bulb’s process() receiver.
func listenAndProcessBulbs(ctx context.Context, addr string, port int, bulbs map[string]*bulb) error {
	hp := net.JoinHostPort(addr, fmt.Sprintf("%d", port))
	conn, err := net.ListenPacket("udp", hp)
	if err != nil {
		return fmt.Errorf("can't listen: %w", err)
	}

	pc := ipv4.NewPacketConn(conn)
	pc.SetControlMessage(ipv4.FlagDst, true)
	defer pc.Close()

	log.Infof("listening on %s", hp)

	type readResult struct {
		data []byte
		cm   *ipv4.ControlMessage
		src  net.Addr
		err  error
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resc := make(chan readResult)
	go func() {
		for {
			buf := make([]byte, controlifx.MaxReadSize)
			n, cm, saddr, err := pc.ReadFrom(buf)
			select {
			case <-ctx.Done():
				return
			case resc <- readResult{buf[:n], cm, saddr, err}:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res := <-resc:
			found := false
			for id, b := range bulbs {
				if net.IPv4bcast.Equal(res.cm.Dst) || b.addr.Equal(res.cm.Dst) {
					found = true
					if err := b.process(pc, res.src, res.data); err != nil {
						log.Errorf("error processing packet for bulb %q: %w", id, err)
					}
				}
			}

			if !found {
				log.Warningf("%d bytes from %v received for %v, but is neither broadcast nor a bulb", len(res.data), res.src, res.cm.Dst)
			}
		}
	}
}

// bulb represents a single emulated bulb and its attributes. It listens on addr and responds to
// requests to set power state and brightness, calling g.set().
type bulb struct {
	id     string        // Subsection in the config, used as identifier in log messages
	name   string        // Name to announce
	addr   net.IP        // IP address
	hwaddr uint64        // 48-bit MAC address
	g      gpio          // GPIO to toggle
	s      *bulbState    // Bulb state
	serial chan<- uint16 // Serial port subscribed to power state changes for this bulb, or nil.
}

func (b *bulb) String() string {
	return fmt.Sprintf("bulb %q (%s)", b.id, b.addr)
}

// send builds a SendableLanMessage with the specified header “lh”, the message type “t” and the
// payload “payload” (may be nil) and sends it, with the source address specified in b.addr,
// to dst. lh is expected to be a pre-filled LanHeader, except for ProtocolHeader.Type and
// Frame.Size, which are calculated here.
func (b *bulb) send(pc *ipv4.PacketConn, lh controlifx.LanHeader, dst net.Addr, t uint16, payload encoding.BinaryMarshaler) error {
	msg := controlifx.SendableLanMessage{Header: lh, Payload: payload}
	// FIXME: check copy
	msg.Header.ProtocolHeader.Type = t
	if payload != nil {
		p, err := payload.MarshalBinary()
		if err != nil {
			return fmt.Errorf("can't marshal payload: %w", err)
		}
		msg.Header.Frame.Size += uint16(len(p))
	}
	data, err := msg.MarshalBinary()
	if err != nil {
		return fmt.Errorf("can't marshal message: %w", err)
	}

	cm := &ipv4.ControlMessage{Src: b.addr}
	n, err := pc.WriteTo(data, cm, dst)
	if err != nil {
		return fmt.Errorf("can't send %d bytes to %v (from %v): %w", len(data), dst, b.addr, err)
	}

	log.Debugf("[%v] sent %d bytes type %d (payload %T) back: %#v", b, n, t, payload, msg)
	return nil
}

// process unmarshals binary data received from a client, prepares a writer (cf. type writer)
// for responding to the request and invokes the bulbState’s handle receiver.
func (b *bulb) process(pc *ipv4.PacketConn, src net.Addr, data []byte) error {
	msg := implifx.ReceivableLanMessage{}
	if err := msg.UnmarshalBinary(data); err != nil {
		return fmt.Errorf("can't unmarshal %d bytes as %T: %w", len(data), msg, err)
	}

	lh := controlifx.LanHeader{
		Frame: controlifx.LanHeaderFrame{
			Size:   controlifx.LanHeaderSize,
			Source: msg.Header.Frame.Source,
		},
		FrameAddress: controlifx.LanHeaderFrameAddress{
			Target:   b.hwaddr,
			Sequence: msg.Header.FrameAddress.Sequence,
		},
	}

	writer := func(always bool, t uint16, reply encoding.BinaryMarshaler) error {
		if msg.Header.FrameAddress.AckRequired {
			if err := b.send(pc, lh, src, controlifx.AcknowledgementType, nil); err != nil {
				return fmt.Errorf("can't acknowledge frame %v: %w", msg, err)
			}
		}

		if always || msg.Header.FrameAddress.ResRequired {
			if err := b.send(pc, lh, src, t, reply); err != nil {
				return fmt.Errorf("can't respond to frame %v with %v: %w", msg, reply, err)
			}
		}

		return nil
	}

	if err := b.s.handle(msg, writer); err != nil {
		return fmt.Errorf("can't handle %v: %w", msg, err)
	}

	if msg.Header.ProtocolHeader.Type == controlifx.LightSetPowerType {
		p := msg.Payload.(*implifx.LightSetPowerLanMessage)
		log.Infof("Handling LightSetPower %+v", p)
		b.g.set(p.Level)
		if b.serial != nil {
			b.serial <- p.Level
		}
	}

	if msg.Header.ProtocolHeader.Type == controlifx.LightSetColorType && b.g.isPWM() {
		p := msg.Payload.(*implifx.LightSetColorLanMessage)
		log.Infof("Handling LightSetColor %+v (HSBK: %+v)", p, p.Color)
		b.g.set(p.Color.Brightness)
	}

	return nil
}

// bulbState represents the state of a bulb. The following code is almost literally taken
// from https://github.com/lifx-tools/emulifx/blob/master/server/server.go (thanks!), with
// minor changes made (the functions are now receivers of a bulbState and the actions are
// processed in (*bulb).process instead. The method receivers here only modify the bulb’s
// state and write a response.)
type bulbState struct {
	service             int8
	port                uint16
	time                int64
	resetSwitchPosition uint8
	dummyLoadOn         bool
	hostInfo            struct {
		signal         float32
		tx             uint32
		rx             uint32
		mcuTemperature uint16
	}
	hostFirmware struct {
		build   int64
		install int64
		version uint32
	}
	wifiInfo struct {
		signal         float32
		tx             uint32
		rx             uint32
		mcuTemperature int16
	}
	wifiFirmware struct {
		build   int64
		install int64
		version uint32
	}
	powerLevel uint16
	label      string
	tags       struct {
		tags  int64
		label string
	}
	version struct {
		vendor  uint32
		product uint32
		version uint32
	}
	info struct {
		time     int64
		uptime   int64
		downtime int64
	}
	mcuRailVoltage  uint32
	factoryTestMode struct {
		on       bool
		disabled bool
	}
	site     [6]byte
	location struct {
		location  [16]byte
		label     string
		updatedAt int64
	}
	group struct {
		group     [16]byte
		label     string
		updatedAt int64
	}
	owner struct {
		owner     [16]byte
		label     string
		updatedAt int64
	}
	state struct {
		color controlifx.HSBK
		dim   int16
		label string
		tags  uint64
	}
	lightRailVoltage  uint32
	lightTemperature  int16
	lightSimpleEvents []struct {
		time     int64
		power    uint16
		duration uint32
		waveform int8
		max      uint16
	}
	wanStatus  int8
	wanAuthKey [32]byte
	wanHost    struct {
		host               string
		insecureSkipVerify bool
	}
	wifi struct {
		networkInterface int8
		status           int8
	}
	wifiAccessPoints struct {
		networkInterface int8
		ssid             string
		security         int8
		strength         int16
		channel          uint16
	}
	wifiAccessPoint struct {
		networkInterface int8
		ssid             string
		pass             string
		security         int8
	}
	sensorAmbientLightLux float32
	sensorDimmerVoltage   uint32

	// Extra.
	startTime int64
}

// newBulbState returns a new, initialized bulbState.
func newBulbState(name string) *bulbState {
	bs := &bulbState{}

	bs.label = name

	bs.service = controlifx.UdpService
	bs.port = uint16(*port) // FIXME: this ignores a port setting in the config

	bs.hostFirmware.build = 1467178139000000000
	bs.hostFirmware.version = 1968197120
	bs.wifiInfo.signal = 1e-5
	bs.wifiFirmware.build = 1456093684000000000

	bs.version.vendor = controlifx.White800HighVVendorId
	bs.version.product = controlifx.White800HighVProductId

	bs.state.color.Kelvin = 3500

	bs.startTime = time.Now().UnixNano()

	return bs
}

// writer is a function for writing a response. “always” specifies whether the reply
// should always be sent or only if the request required a response, “t” is the type
// of the payload and “msg” the payload to send.
type writer func(always bool, t uint16, msg encoding.BinaryMarshaler) error

// handle processes the incoming message “msg” by updating the bulbState’s state
// and sending a reply with “w”.
func (bs *bulbState) handle(msg implifx.ReceivableLanMessage, w writer) error {
	log.Debugf("receive message type %d (payload %T): %+v", msg.Header.ProtocolHeader.Type, msg.Payload, msg)
	switch msg.Header.ProtocolHeader.Type {
	case controlifx.GetServiceType:
		return bs.getService(w)
	case controlifx.GetHostInfoType:
		return bs.getHostInfo(w)
	case controlifx.GetHostFirmwareType:
		return bs.getHostFirmware(w)
	case controlifx.GetWifiInfoType:
		return bs.getWifiInfo(w)
	case controlifx.GetWifiFirmwareType:
		return bs.getWifiFirmware(w)
	case controlifx.GetPowerType:
		return bs.getPower(w)
	case controlifx.SetPowerType:
		return bs.setPower(msg, w)
	case controlifx.GetLabelType:
		return bs.getLabel(w)
	case controlifx.SetLabelType:
		return bs.setLabel(msg, w)
	case controlifx.GetVersionType:
		return bs.getVersion(w)
	case controlifx.GetInfoType:
		return bs.getInfo(w)
	case controlifx.GetLocationType:
		return bs.getLocation(w)
	case controlifx.GetGroupType:
		return bs.getGroup(w)
	case controlifx.GetOwnerType:
		return bs.getOwner(w)
	case controlifx.SetOwnerType:
		return bs.setOwner(msg, w)
	case controlifx.EchoRequestType:
		return bs.echoRequest(msg, w)
	case controlifx.LightGetType:
		return bs.lightGet(w)
	case controlifx.LightSetColorType:
		return bs.lightSetColor(msg, w)
	case controlifx.LightGetPowerType:
		return bs.lightGetPower(w)
	case controlifx.LightSetPowerType:
		return bs.lightSetPower(msg, w)
	}

	return nil
}

func (bs *bulbState) getService(w writer) error {
	return w(true, controlifx.StateServiceType, &implifx.StateServiceLanMessage{
		Service: controlifx.UdpService,
		Port:    uint32(bs.port),
	})
}

func (bs *bulbState) getHostInfo(w writer) error {
	return w(true, controlifx.StateHostInfoType, &implifx.StateHostInfoLanMessage{})
}

func (bs *bulbState) getHostFirmware(w writer) error {
	return w(true, controlifx.StateHostFirmwareType, &implifx.StateHostFirmwareLanMessage{
		Build:   uint64(bs.hostFirmware.build),
		Version: bs.hostFirmware.version,
	})
}

func (bs *bulbState) getWifiInfo(w writer) error {
	return w(true, controlifx.StateWifiInfoType, &implifx.StateWifiInfoLanMessage{
		Signal: bs.wifiInfo.signal,
		Tx:     bs.wifiInfo.tx,
		Rx:     bs.wifiInfo.rx,
	})
}

func (bs *bulbState) getWifiFirmware(w writer) error {
	return w(true, controlifx.StateWifiFirmwareType, &implifx.StateWifiFirmwareLanMessage{
		Build:   uint64(bs.wifiFirmware.build),
		Version: bs.wifiFirmware.version,
	})
}

func (bs *bulbState) getPower(w writer) error {
	return w(true, controlifx.StatePowerType, &implifx.StatePowerLanMessage{
		Level: bs.powerLevel,
	})
}

func (bs *bulbState) setPower(msg implifx.ReceivableLanMessage, w writer) error {
	responsePayload := &implifx.StatePowerLanMessage{
		Level: bs.powerLevel,
	}
	bs.powerLevel = msg.Payload.(*implifx.SetPowerLanMessage).Level

	return w(false, controlifx.StatePowerType, responsePayload)
}

func (bs *bulbState) getLabel(w writer) error {
	return w(true, controlifx.StateLabelType, &implifx.StateLabelLanMessage{
		Label: bs.label,
	})
}

func (bs *bulbState) setLabel(msg implifx.ReceivableLanMessage, w writer) error {
	bs.label = msg.Payload.(*implifx.SetLabelLanMessage).Label

	return w(false, controlifx.StateLabelType, &implifx.StateLabelLanMessage{
		Label: bs.label,
	})
}

func (bs *bulbState) getVersion(w writer) error {
	return w(true, controlifx.StateVersionType, &implifx.StateVersionLanMessage{
		Vendor:  bs.version.vendor,
		Product: bs.version.product,
		Version: bs.version.version,
	})
}

func (bs *bulbState) getInfo(w writer) error {
	now := time.Now().UnixNano()

	return w(true, controlifx.StateInfoType, &implifx.StateInfoLanMessage{
		Time:     uint64(now),
		Uptime:   uint64(now - bs.startTime),
		Downtime: 0,
	})
}

func (bs *bulbState) getLocation(w writer) error {
	return w(true, controlifx.StateLocationType, &implifx.StateLocationLanMessage{
		Location:  bs.location.location,
		Label:     bs.location.label,
		UpdatedAt: uint64(bs.location.updatedAt),
	})
}

func (bs *bulbState) getGroup(w writer) error {
	return w(true, controlifx.StateGroupType, &implifx.StateGroupLanMessage{
		Group:     bs.group.group,
		Label:     bs.group.label,
		UpdatedAt: uint64(bs.group.updatedAt),
	})
}

func (bs *bulbState) getOwner(w writer) error {
	return w(true, controlifx.StateOwnerType, &implifx.StateOwnerLanMessage{
		Owner:     bs.owner.owner,
		Label:     bs.owner.label,
		UpdatedAt: uint64(bs.owner.updatedAt),
	})
}

func (bs *bulbState) setOwner(msg implifx.ReceivableLanMessage, w writer) error {
	payload := msg.Payload.(*implifx.SetOwnerLanMessage)
	bs.owner.owner = payload.Owner
	bs.owner.label = payload.Label
	bs.owner.updatedAt = time.Now().UnixNano()

	return w(false, controlifx.StateOwnerType, &implifx.StateOwnerLanMessage{
		Owner:     bs.owner.owner,
		Label:     bs.owner.label,
		UpdatedAt: uint64(bs.owner.updatedAt),
	})
}

func (bs *bulbState) echoRequest(msg implifx.ReceivableLanMessage, w writer) error {
	return w(true, controlifx.EchoResponseType, &implifx.EchoResponseLanMessage{
		Payload: msg.Payload.(*implifx.EchoRequestLanMessage).Payload,
	})
}

func (bs *bulbState) lightGet(w writer) error {
	return w(true, controlifx.LightStateType, &implifx.LightStateLanMessage{
		Color: bs.state.color,
		Power: bs.powerLevel,
		Label: bs.label,
	})
}

func (bs *bulbState) lightSetColor(msg implifx.ReceivableLanMessage, w writer) error {
	responsePayload := &implifx.LightStateLanMessage{
		Color: bs.state.color,
		Power: bs.powerLevel,
		Label: bs.label,
	}
	payload := msg.Payload.(*implifx.LightSetColorLanMessage)
	bs.state.color = payload.Color

	return w(false, controlifx.LightStateType, responsePayload)
}

func (bs *bulbState) lightGetPower(w writer) error {
	return w(true, controlifx.LightStatePowerType, &implifx.LightStatePowerLanMessage{
		Level: bs.powerLevel,
	})
}

func (bs *bulbState) lightSetPower(msg implifx.ReceivableLanMessage, w writer) error {
	responsePayload := &implifx.StatePowerLanMessage{
		Level: bs.powerLevel,
	}
	payload := msg.Payload.(*implifx.LightSetPowerLanMessage)
	bs.powerLevel = payload.Level

	return w(false, controlifx.LightStatePowerType, responsePayload)
}
