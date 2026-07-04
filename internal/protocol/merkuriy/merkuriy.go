package merkuriy
import (
"fmt"
"mbgw/internal/protocol/modbus"
)
// BuildFrame builds a Merkuriy channel frame:
// [addr 1B][code 1B][param 0..1B][ext 0..1B][data N][CRC16-modbus 2B]
func BuildFrame(addr byte, code byte, payload []byte) []byte {
body := make([]byte, 0, 2+len(payload)+2)
body = append(body, addr, code)
body = append(body, payload...)
crc := modbus.CRC16(body)
body = append(body, byte(crc&0xFF), byte(crc>>8))
return body
}
// ParseFrame validates CRC and returns addr, code, data (without CRC).
func ParseFrame(frame []byte) (addr byte, code byte, data []byte, err error) {
if len(frame) < 4 {
return 0, 0, nil, fmt.Errorf("merkuriy frame too short")
}
body := frame[:len(frame)-2]
calcCRC := modbus.CRC16(body)
frameCRC := uint16(frame[len(frame)-2]) | (uint16(frame[len(frame)-1]) << 8)
if calcCRC != frameCRC {
return 0, 0, nil, fmt.Errorf("invalid CRC")
}
return frame[0], frame[1], frame[2 : len(frame)-2], nil
}
// BuildTestLink builds cmd 0x00 test-link frame (addr).
func BuildTestLink(addr byte) []byte {
return BuildFrame(addr, 0x00, nil)
}
// BuildOpenChannel builds cmd 0x01 open-channel frame: addr, level, 6-byte password.
func BuildOpenChannel(addr byte, level byte, password [6]byte) []byte {
payload := make([]byte, 0, 7)
payload = append(payload, level)
payload = append(payload, password[:]...)
return BuildFrame(addr, 0x01, payload)
}
// BuildCloseChannel builds cmd 0x02 close-channel frame.
func BuildCloseChannel(addr byte) []byte {
return BuildFrame(addr, 0x02, nil)
}
