package simulator

import (
    "encoding/binary"
    "log"
    "net"

    "mbgw/internal/protocol/modbus"
)

// tsrv024Registers holds static current-value registers for the TSRV-024
// vertical-slice emulator (no session, no function-65 archive protocol yet -
// see backlog for CRC-checked records and DST handling).
var tsrv024Registers = map[int][]byte{
    100: {0x3F, 0x80, 0x00, 0x00}, // float 1.0 (heat power, Gcal/h)
    104: {0x09, 0xE9},             // uint16 2537 -> scaled_int *0.01 = 25.37 (temperature)
}

func RunTSRV024(addr string) error {
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
        go handleTSRV024Conn(conn)
    }
}

func handleTSRV024Conn(conn net.Conn) {
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
            log.Printf("[sim tsrv024] bad frame: %v\n", err)
            continue
        }
        if len(pdu) < 5 {
            continue
        }

        respPDU := buildTSRV024Response(pdu)
        if respPDU == nil {
            continue
        }

        respFrame := modbus.BuildTCPFrame(txID, unitID, respPDU)
        if _, err := conn.Write(respFrame); err != nil {
            return
        }
    }
}

// tsrv024ArchiveTestRecord is a fixed test archive record for function 65:
// 4 bytes time (0x00000001, not the nonexistent-record marker) + 2 bytes
// data (0x002A = 42) + real CRC16-modbus over the first 6 bytes (0x04D0,
// little-endian on wire: D0 04), so mb_func65's CRC check passes for real.
var tsrv024ArchiveTestRecord = []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x2A, 0xD0, 0x04}

func buildTSRV024Response(pdu []byte) []byte {
    funcCode := pdu[0]

    if funcCode == modbus.FuncCodeArchive65 {
        return append([]byte{funcCode, byte(len(tsrv024ArchiveTestRecord))}, tsrv024ArchiveTestRecord...)
    }

    if funcCode != 0x03 && funcCode != 0x04 {
        return nil
    }

    addr := int(binary.BigEndian.Uint16(pdu[1:3]))
    qty := int(binary.BigEndian.Uint16(pdu[3:5]))

    data := readTSRV024Registers(addr, qty)
    if data == nil {
        return nil
    }
    resp := make([]byte, 2+len(data))
    resp[0] = funcCode
    resp[1] = byte(len(data))
    copy(resp[2:], data)
    return resp
}

func readTSRV024Registers(addr int, qty int) []byte {
    out := make([]byte, 0, qty*2)
    remaining := qty * 2
    a := addr
    for remaining > 0 {
        raw, ok := tsrv024Registers[a]
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