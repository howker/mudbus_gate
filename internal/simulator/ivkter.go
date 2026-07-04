package simulator

import (
    "encoding/binary"
    "log"
    "net"

    "mbgw/internal/protocol/modbus"
)

// ivkterRegisters holds static current-value registers for the IVK-TER
// vertical-slice emulator (no session, no archive protocol yet - see
// backlog for u32+float composite and function 65 archive support).
var ivkterRegisters = map[int][]byte{
    100: {0x42, 0xC8, 0x00, 0x00}, // float 100.0 (flow rate)
    102: {0x41, 0xA0, 0x00, 0x00}, // float 20.0 (temperature)
    // Archive record #0 at base address 200 (4 registers / 8 bytes):
    // u32 int part = 12345, float frac part = 0.6789 -> 12345.6789 composite.
    200: {0x00, 0x00, 0x30, 0x39, 0x3F, 0x2D, 0xCC, 0x64},
}

func RunIVKTER(addr string) error {
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
        go handleIVKTERConn(conn)
    }
}

func handleIVKTERConn(conn net.Conn) {
    defer conn.Close()

    buf := make([]byte, 512)
    for {
        n, err := conn.Read(buf)
        if err != nil || n < 8 {
            return
        }
        frame := append([]byte(nil), buf[:n]...)

        txID, unitID, pdu, err := modbus.ParseTCPFrame(frame)
        if err != nil {
            log.Printf("[sim ivk-ter] bad frame: %v\n", err)
            continue
        }
        if len(pdu) < 5 {
            continue
        }

        respPDU := buildIVKTERResponse(pdu)
        if respPDU == nil {
            continue
        }

        respFrame := modbus.BuildTCPFrame(txID, unitID, respPDU)
        if _, err := conn.Write(respFrame); err != nil {
            return
        }
    }
}

func buildIVKTERResponse(pdu []byte) []byte {
    funcCode := pdu[0]
    if funcCode != 0x03 && funcCode != 0x04 {
        return nil
    }

    addr := int(binary.BigEndian.Uint16(pdu[1:3]))
    qty := int(binary.BigEndian.Uint16(pdu[3:5]))

    data := readIVKTERRegisters(addr, qty)
    if data == nil {
        return nil
    }
    resp := make([]byte, 2+len(data))
    resp[0] = funcCode
    resp[1] = byte(len(data))
    copy(resp[2:], data)
    return resp
}

func readIVKTERRegisters(addr int, qty int) []byte {
    out := make([]byte, 0, qty*2)
    remaining := qty * 2
    a := addr
    for remaining > 0 {
        raw, ok := ivkterRegisters[a]
        if !ok {
            return nil
        }
        out = append(out, raw...)
        remaining -= len(raw)
        a += len(raw) / 2
    }
    if remaining != 0 {
        return nil
    }
    return out
}