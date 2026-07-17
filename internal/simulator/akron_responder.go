package simulator

import (
	"encoding/hex"
	"strings"
	"time"
)

// This file makes the raw-discovery Akron responder fully configurable at
// RUNTIME via a plain struct (loaded from YAML by the northbound package),
// so behaviour can be changed by editing a text file on the Энергосфера
// server and restarting — no rebuild, no re-upload over a slow link.
//
// Answer precedence for an inbound RTU PDU (function code = pdu[0]):
//  1. an explicit per-command override from config (Overrides[code])
//  2. built-in smart answers: 101 identification, 03/04 register reads
//     (with a LIVE clock for the time registers), 104/105 archive rows
//  3. the command-110 probe reply (configurable shape)
//  4. otherwise: fall through to the base emulator (buildAkron01Response),
//     then nil.

// AkronSimConfig is the runtime behaviour of the discovery responder.
type AkronSimConfig struct {
	// Cmd110 selects the reply shape for the undocumented command 110:
	// "nil" | "empty" | "ready" | "zero" | "echo". Default "ready" (the
	// shape that made Энергосфера's driver treat the device as alive).
	Cmd110 string

	// LiveClock, when true (default), answers reads of the time registers
	// (0x0010–0x0013) with the real current time in BCD instead of the
	// emulator's fixed 2008 fixture — this removes the artificial 18-year
	// clock skew that blocked the driver from requesting archives.
	LiveClock bool

	// Identity fields for command 101 (all optional, sensible defaults).
	DeviceType  byte   // default 0x00
	FirmwareBCD byte   // default 0x37 (v3.7)
	SerialLENum uint32 // default 12345, encoded little-endian 4 bytes

	// Overrides maps a decimal function code (as a string key, e.g. "110")
	// to a full response PDU in hex (function code + data, no CRC). This is
	// the escape hatch: any new/unknown command Энергосфера sends can be
	// answered by adding a line here — no code change, no rebuild.
	Overrides map[string]string
}

// DefaultAkronSimConfig is used when no config file is supplied.
func DefaultAkronSimConfig() AkronSimConfig {
	return AkronSimConfig{
		Cmd110:      "ready",
		LiveClock:   true,
		DeviceType:  0x00,
		FirmwareBCD: 0x37,
		SerialLENum: 12345,
	}
}

var activeAkronCfg = DefaultAkronSimConfig()

// SetAkronSimConfig installs the runtime config. Call once at startup
// before serving connections. Empty/zero fields are normalised to defaults.
func SetAkronSimConfig(c AkronSimConfig) {
	if c.Cmd110 == "" {
		c.Cmd110 = "ready"
	}
	if c.DeviceType == 0 && c.FirmwareBCD == 0 && c.SerialLENum == 0 {
		// looks unset — keep identity defaults but honour LiveClock/Cmd110
		c.DeviceType = 0x00
		c.FirmwareBCD = 0x37
		c.SerialLENum = 12345
	}
	activeAkronCfg = c
}

// ActiveAkronSummary returns a short human-readable description of the
// active config, for the startup log line.
func ActiveAkronSummary() string {
	c := activeAkronCfg
	clk := "fixed-2008"
	if c.LiveClock {
		clk = "live"
	}
	ov := len(c.Overrides)
	return "cmd110=" + c.Cmd110 + " clock=" + clk + " overrides=" + itoa(ov)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// AkronResponsePDU is the discovery responder entry point.
// Input: RTU PDU (function code + data). Output: response PDU, or nil.
func AkronResponsePDU(pdu []byte) []byte {
	if len(pdu) < 1 {
		return nil
	}
	code := pdu[0]

	// 1. explicit override from config
	if raw, ok := activeAkronCfg.Overrides[itoa(int(code))]; ok {
		if b, err := hex.DecodeString(strings.TrimSpace(raw)); err == nil && len(b) > 0 {
			return b
		}
	}

	// 2. built-in smart answers
	switch code {
	case 101:
		return akronIdentityPDU()
	case 0x03, 0x04:
		if activeAkronCfg.LiveClock {
			if resp, handled := akronLiveClockRead(pdu); handled {
				return resp
			}
		}
		return buildAkron01Response(pdu)
	case 104, 105:
		return buildAkron01Response(pdu)
	case 110:
		return akron110PDU()
	}

	// 4. fall through to base emulator
	return buildAkron01Response(pdu)
}

func akron110PDU() []byte {
	switch activeAkronCfg.Cmd110 {
	case "empty":
		return []byte{110, 0x00}
	case "ready":
		return []byte{110, 0x01, 0x01}
	case "zero":
		return []byte{110, 0x01, 0x00}
	case "echo":
		return []byte{110}
	default: // "nil"
		return nil
	}
}

func akronIdentityPDU() []byte {
	c := activeAkronCfg
	s := c.SerialLENum
	serial := []byte{byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24)} // little-endian
	pdu := make([]byte, 0, 8)
	pdu = append(pdu, 101, 6, c.DeviceType, c.FirmwareBCD)
	pdu = append(pdu, serial...)
	return pdu
}

// akronLiveClockRead handles function 03/04 reads that fall entirely within
// the time-register block (0x0010–0x0013), answering with the real current
// time in the Akron BCD layout. Returns handled=false for reads outside
// that block, so normal (non-time) register reads go to the base emulator.
//
// Register layout (per doc table 2):
//
//	0x0010: second(BCD), minute(BCD)
//	0x0011: hour(BCD),   day_of_week
//	0x0012: date(BCD),   month(BCD)
//	0x0013: year-2000(BCD), id_am
func akronLiveClockRead(pdu []byte) (resp []byte, handled bool) {
	if len(pdu) < 5 {
		return nil, false
	}
	addr := int(pdu[1])<<8 | int(pdu[2])
	qty := int(pdu[3])<<8 | int(pdu[4])
	// Only handle reads that start at 0x0010 and stay within 0x0010–0x0013.
	if addr != 0x0010 || qty < 1 || qty > 4 {
		return nil, false
	}

	now := time.Now()
	dow := int(now.Weekday()) // Sunday=0; Akron's exact convention unknown, kept as-is
	regs := [4][2]byte{
		{bcd(now.Second()), bcd(now.Minute())},
		{bcd(now.Hour()), byte(dow)},
		{bcd(now.Day()), bcd(int(now.Month()))},
		{bcd(now.Year() % 100), 0x00},
	}

	data := make([]byte, 0, qty*2)
	for i := 0; i < qty; i++ {
		data = append(data, regs[i][0], regs[i][1])
	}
	out := make([]byte, 0, 2+len(data))
	out = append(out, pdu[0], byte(len(data)))
	out = append(out, data...)
	return out, true
}

// bcd encodes a 0–99 value as packed BCD.
func bcd(v int) byte {
	v %= 100
	return byte((v/10)<<4 | (v % 10))
}
