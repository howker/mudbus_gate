package simulator

import (
    "encoding/binary"
    "log"
    "net"

    "mbgw/internal/protocol/modbus"
)

// tsrv024Registers holds static current-value registers for the TSRV-024
// vertical-slice emulator.
var tsrv024Registers = map[int][]byte{
    100: {0x3F, 0x80, 0x00, 0x00}, // float 1.0 (heat power, Gcal/h)
    104: {0x09, 0xE9},             // uint16 2537 -> scaled_int *0.01 = 25.37 (temperature)
}

// tsrv024HourlyRecord is a fixed test hourly-TS-archive record (172 bytes,
// str_arh_tsrv024.pdf table 1): archive_time=1700003600 (not the
// nonexistent-record marker), clean_work_time=60, all downtime/NS
// counters=0, current_algorithm=1, ns_flags=0, total_heat=123.45 Gcal,
// heat_water_draw=10.5, total_mass=50.25, heat/mass/volume per pipes 1-2
// nonzero (pipes 3-4 zero), weighted/avg temps and pressures per pipes
// 1-2 nonzero, cold_water_temp=5.00C, real CRC16-modbus over the first
// 170 bytes (little-endian on wire).
var tsrv024HourlyRecord = []byte{
    0x65, 0x53, 0xFF, 0x10, 0x00, 0x3C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
    0x42, 0xF6, 0xE6, 0x66, 0x41, 0x28, 0x00, 0x00, 0x42, 0x49, 0x00, 0x00,
    0x41, 0xF0, 0x00, 0x00, 0x41, 0xC8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x41, 0x48, 0x00, 0x00, 0x41, 0x20, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x41, 0x70, 0x00, 0x00,
    0x41, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x1D, 0x4C, 0x1C, 0xE8, 0x00, 0x00, 0x00, 0x00, 0x1B, 0x58, 0x1A, 0x90,
    0x00, 0x00, 0x00, 0x00, 0x02, 0x58, 0x02, 0x4E, 0x00, 0x00, 0x00, 0x00,
    0x01, 0xF4, 0x2C, 0x77,
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
        if len(pdu) < 1 {
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

func buildTSRV024Response(pdu []byte) []byte {
    funcCode := pdu[0]
    if funcCode == modbus.FuncCodeArchive65 {
        // Request format (BuildArchive65IndexPDU/BuildArchive65TimePDU):
        //   func(1) | arrayNumber(2) | recordCount(2) | mode(1) | data(2 or 6)
        // This vertical-slice emulator always returns the same fixed
        // hourly record regardless of the requested archive_type/index/time.
        if len(pdu) < 6 {
            return nil
        }
        return append([]byte{funcCode, byte(len(tsrv024HourlyRecord))}, tsrv024HourlyRecord...)
    }
    if funcCode != 0x03 && funcCode != 0x04 {
        return nil
    }
    if len(pdu) < 5 {
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