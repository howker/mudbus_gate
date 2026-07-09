package simulator

import (
"fmt"
"log"
"net"

"mbgw/internal/protocol/modbus"
)

// merkuriyStatusOK is the "all normal" status byte (X0), per CONTRACTS.md
// section 3.
const merkuriyStatusOK = 0x00

// merkuriyArchiveRow is a fixed 4-byte test record (two uint16 fields),
// matching the same fixture used in archive/merkuriy_long_response_test.go's
// TestMerkuriyLongResponse_MultiRecordSplit - sufficient to prove the RTU
// round-trip end-to-end. A profile-accurate power-profile record_layout
// (matching the real field structure from section 4.6) is a separate,
// later step once profiles/merkuriy.yaml is written for real.
var merkuriyArchiveRow = []byte{0x00, 0x2A, 0x00, 0x2B} // a=42, b=43

func RunMerkuriy(addr string) error {
ln, err := net.Listen("tcp", addr)
if err != nil {
return err
}
defer ln.Close()

for {
conn, err := ln.Accept()
if err != nil {
return err
}
go handleMerkuriyConn(conn)
}
}

func handleMerkuriyConn(conn net.Conn) {
defer conn.Close()

buf := make([]byte, 512)
for {
n, err := conn.Read(buf)
if err != nil || n < 4 {
// Minimum Merkuriy frame: addr(1)+code(1)+CRC(2) = 4 bytes.
return
}
frame := append([]byte(nil), buf[:n]...)

body := frame[:len(frame)-2]
wantCRC := uint16(frame[len(frame)-2]) | (uint16(frame[len(frame)-1]) << 8)
if modbus.CRC16(body) != wantCRC {
log.Printf("[sim merkuriy] bad CRC\n")
continue
}

addr := body[0]
code := body[1]
params := body[2:]

respData := buildMerkuriyResponse(code, params)
if respData == nil {
continue
}

resp := append([]byte{addr}, respData...)
crc := modbus.CRC16(resp)
resp = append(resp, byte(crc&0xFF), byte(crc>>8))

if _, err := conn.Write(resp); err != nil {
return
}
}
}

// buildMerkuriyResponse returns the response DATA field only (no address,
// no CRC - those are added by the caller, matching the real device's
// [addr][data][CRC] frame shape with no separate "code" byte, per
// CONTRACTS.md section 3 / figure 1.2).
func buildMerkuriyResponse(code byte, params []byte) []byte {
switch code {
case 0x00: // test-link
return []byte{merkuriyStatusOK}
case 0x01: // open-channel (level + 6-byte password)
return []byte{merkuriyStatusOK}
case 0x02: // close-channel
return []byte{merkuriyStatusOK}
case 0x16: // relative addressing read (command 0x16, section 4.6)
if len(params) < 4 {
return nil
}
count := int(params[3])
if count < 1 {
count = 1
}
data := make([]byte, 0, count*len(merkuriyArchiveRow))
for i := 0; i < count; i++ {
data = append(data, merkuriyArchiveRow...)
}
return data
default:
return nil
}
}

func Run(device string, addr string) error {
switch device {
case "merkuriy":
return RunMerkuriy(addr)
case "vkm360":
return RunVKM360(addr)
case "ivk-ter":
return RunIVKTER(addr)
case "tsrv024":
return RunTSRV024(addr)
case "acron-01":
return RunAkron01(addr)
default:
return fmt.Errorf("unsupported simulator: %s", device)
}
}