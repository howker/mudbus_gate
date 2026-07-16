package northbound

import (
	"encoding/binary"
	"encoding/hex"
)

// DiscoveryOverride lets one specific (unit, function, address) request
// get an exact, hand-crafted response — useful once a discovery session
// reveals, say, a status register that must read as "ready" before
// Энергосфера proceeds to the next step of its archive-read sequence.
type DiscoveryOverride struct {
	Unit        byte   `yaml:"unit"`
	Function    byte   `yaml:"function"`
	Address     uint16 `yaml:"address"`
	ResponseHex string `yaml:"response_hex"` // PDU bytes AFTER the function code, hex-encoded
}

// DiscoveryFixture drives DiscoveryServer's stub responses. It never reads
// live data — there is none to read, DiscoveryServer has no device and no
// Repo — every response is either a hand-set override or a generic,
// structurally valid placeholder built only to keep a Modbus dialogue
// moving long enough to observe what the peer asks for next.
type DiscoveryFixture struct {
	ReadFillByte byte                `yaml:"read_fill_byte"`
	Overrides    []DiscoveryOverride `yaml:"overrides"`
}

// Respond builds the response PDU (function code + data, no MBAP) for one
// request PDU (function code + body).
func (f DiscoveryFixture) Respond(unitID, function byte, body []byte) []byte {
	for _, ov := range f.Overrides {
		if ov.Unit != unitID || ov.Function != function || len(body) < 2 {
			continue
		}
		if binary.BigEndian.Uint16(body[0:2]) != ov.Address {
			continue
		}
		if raw, err := hex.DecodeString(ov.ResponseHex); err == nil {
			return append([]byte{function}, raw...)
		}
	}

	switch function {
	case funcReadHolding, funcReadInput:
		return f.stubRead(function, body)
	case funcWriteSingleCoil, funcWriteSingleRegister:
		return f.stubWriteSingleAck(function, body)
	case funcWriteMultipleCoils, funcWriteMultipleRegisters:
		return f.stubWriteMultipleAck(function, body)
	default:
		// An honest "not supported" is more useful during discovery than
		// a fabricated ack shape we can't back up — real Modbus masters
		// already handle "device doesn't support this function" as a
		// normal case, so this does not derail the dialogue any more
		// than talking to a real device would.
		return []byte{function | 0x80, excIllegalFunction}
	}
}

// stubRead answers 03/04 with qty registers filled with ReadFillByte —
// a structurally valid, content-free placeholder.
func (f DiscoveryFixture) stubRead(function byte, body []byte) []byte {
	if len(body) < 4 {
		return []byte{function | 0x80, excIllegalDataValue}
	}
	qty := binary.BigEndian.Uint16(body[2:4])
	if qty == 0 || qty > maxReadQuantity {
		qty = 1
	}
	data := make([]byte, int(qty)*2)
	for i := range data {
		data[i] = f.ReadFillByte
	}
	resp := make([]byte, 2+len(data))
	resp[0] = function
	resp[1] = byte(len(data))
	copy(resp[2:], data)
	return resp
}

// stubWriteSingleAck answers 05/06: a real slave's success ack is the
// request's addr+value echoed back verbatim.
func (f DiscoveryFixture) stubWriteSingleAck(function byte, body []byte) []byte {
	if len(body) < 4 {
		return []byte{function | 0x80, excIllegalDataValue}
	}
	resp := make([]byte, 5)
	resp[0] = function
	copy(resp[1:], body[0:4])
	return resp
}

// stubWriteMultipleAck answers 0F/10: a real slave's success ack is
// addr+quantity only (the byte-count and data from the request are not
// echoed back).
func (f DiscoveryFixture) stubWriteMultipleAck(function byte, body []byte) []byte {
	if len(body) < 4 {
		return []byte{function | 0x80, excIllegalDataValue}
	}
	resp := make([]byte, 5)
	resp[0] = function
	copy(resp[1:], body[0:4])
	return resp
}
