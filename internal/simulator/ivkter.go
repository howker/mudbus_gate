package simulator

import (
    "encoding/binary"
    "log"
    "net"

    "mbgw/internal/protocol/modbus"
)

// ivkterRegisters holds static current-value registers for the IVK-TER
// vertical-slice emulator.
var ivkterRegisters = map[int][]byte{
    100: {0x42, 0xC8, 0x00, 0x00}, // float 100.0 (flow rate)
    102: {0x41, 0xA0, 0x00, 0x00}, // float 20.0 (temperature)
}

// ivkterHourlyRecord is a fixed test hourly-archive record (30 bytes,
// str_arh_ivk_ter.pdf table 3): archive_time=1700000000 (not the
// nonexistent-record marker), v_plus=12345.6, v_minus=100.25, q_avg=55.5,
// resistance=1200.0, errors=0, comm_fail_time=0, flowmeter_type=1,
// downtime=0, power_loss_time=0. No CRC (IVK-TER archive records have
// none - unlike TSRV-024's).
var ivkterHourlyRecord = []byte{
    0x65, 0x53, 0xF1, 0x00, 0x46, 0x40, 0xE6, 0x66, 0x42, 0xC8, 0x80, 0x00,
    0x42, 0x5E, 0x00, 0x00, 0x44, 0x96, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
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
        if len(pdu) < 1 {
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

// ivkterFirmwareVersion is a fake firmware version string returned by
// function 17 (Report Slave ID), for testing the diagnostics path.
var ivkterFirmwareVersion = []byte("76.63.00.08")

func buildIVKTERResponse(pdu []byte) []byte {
    funcCode := pdu[0]

    if funcCode == modbus.FuncCodeReportSlaveID {
        data := append([]byte{0xFF}, ivkterFirmwareVersion...) // runStatus=ON + version bytes
        return append([]byte{funcCode, byte(len(data))}, data...)
    }

    if funcCode == modbus.FuncCodeArchive65 {
        // Request format (see prtkl_Modbus_pril_1.pdf, matched by
        // BuildArchive65IndexPDU/BuildArchive65TimePDU):
        //   func(1) | arrayNumber(2) | recordCount(2) | mode(1) | data(2 or 6)
        // The simulator does not distinguish array/mode/index/time for
        // this vertical slice - it always returns the same fixed hourly
        // record, regardless of which archive_type/index/time was asked
        // for (see backlog for a fuller emulator).
        if len(pdu) < 6 {
            return nil
        }
        return append([]byte{funcCode, byte(len(ivkterHourlyRecord))}, ivkterHourlyRecord...)
    }

    if funcCode != 0x03 && funcCode != 0x04 {
        return nil
    }

    if len(pdu) < 5 {
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