package simulator

import (
    "encoding/binary"
    "log"
    "net"
    "sync"
    "time"

    "mbgw/internal/protocol/modbus"
)

var ivkterMu sync.Mutex

// ivkterRegisters holds static current-value registers for the IVK-TER
// vertical-slice emulator. This simplification does not distinguish HR
// from IR (both function 03 and 04 read the same map) - acceptable for
// this vertical slice, and it conveniently means a function-16 write to
// "HR 0x8000" is immediately visible on the next "IR 0x8000" read,
// mirroring a real device's single underlying clock register.
//
// Register 32768 (0x8000) starts ~90s behind "now" (mirroring the same
// deliberate-drift pattern used in the Merkuriy simulator), so a live
// set-time test has something real to correct.
var ivkterRegisters = map[int][]byte{
    100: {0x42, 0xC8, 0x00, 0x00}, // float 100.0 (flow rate)
    102: {0x41, 0xA0, 0x00, 0x00}, // float 20.0 (temperature)
}

func init() {
    buf := make([]byte, 4)
    binary.BigEndian.PutUint32(buf, uint32(time.Now().Add(-90*time.Second).Unix()))
    ivkterRegisters[32768] = buf
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

    // Function 16 (0x10): write multiple registers. Only used by this
    // vertical slice for the HR 0x8000 "time set" register, but handled
    // generically for any address present in ivkterRegisters.
    if funcCode == 0x10 {
        return buildIVKTERWriteResponse(pdu)
    }

    if funcCode != 0x03 && funcCode != 0x04 {
        return nil
    }

    if len(pdu) < 5 {
        return nil
    }

    addr := int(binary.BigEndian.Uint16(pdu[1:3]))
    qty := int(binary.BigEndian.Uint16(pdu[3:5]))

    ivkterMu.Lock()
    data := readIVKTERRegisters(addr, qty)
    ivkterMu.Unlock()
    if data == nil {
        return nil
    }
    resp := make([]byte, 2+len(data))
    resp[0] = funcCode
    resp[1] = byte(len(data))
    copy(resp[2:], data)
    return resp
}

// buildIVKTERWriteResponse handles function 16 (write multiple
// registers): [func(1)][addr(2)][qty(2)][byteCount(1)][data(byteCount)].
// Response echoes addr+qty per the standard function 16 reply shape.
func buildIVKTERWriteResponse(pdu []byte) []byte {
    if len(pdu) < 6 {
        return nil
    }
    addr := int(binary.BigEndian.Uint16(pdu[1:3]))
    qty := int(binary.BigEndian.Uint16(pdu[3:5]))
    byteCount := int(pdu[5])
    if len(pdu) < 6+byteCount || byteCount != qty*2 {
        return nil
    }
    data := pdu[6 : 6+byteCount]

    ivkterMu.Lock()
    a := addr
    for i := 0; i < byteCount; i += 2 {
        ivkterRegisters[a] = append([]byte(nil), data[i:i+2]...)
        a++
    }
    ivkterMu.Unlock()

    resp := make([]byte, 5)
    resp[0] = 0x10
    binary.BigEndian.PutUint16(resp[1:3], uint16(addr))
    binary.BigEndian.PutUint16(resp[3:5], uint16(qty))
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