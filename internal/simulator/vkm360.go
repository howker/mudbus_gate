package simulator

import (
    "encoding/binary"
    "log"
    "net"

    "mbgw/internal/protocol/modbus"
)

// vkm360Registers holds static current-value registers, answered on function
// 03/04 reads. Archive protocol registers (7900-7914, 8000-9999) are handled
// separately below via per-connection state, since they represent a stateful
// multi-step exchange (CONTRACTS.md §6.1), not a fixed lookup table.
var vkm360Registers = map[int][]byte{
    110: {0x49, 0x96, 0x02, 0xD2},
    112: {0x42, 0xF6, 0xE9, 0xD5},
    114: {0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93},
    2000: {0x47, 0x43, 0x50, 0x00},
    2004: {0x41, 0xA0, 0x00, 0x00},
    2008: {0x42, 0xF6, 0xE9, 0xD5}, // pipe 1
    2108: {0x43, 0x48, 0x00, 0x00}, // pipe 2 mass flow rate = 200.0
    4002: {0x00, 0x00},
}

const (
    archIDReg     = 7900
    archPipeReg   = 7901
    archStartReg  = 7902
    archEndReg    = 7908
    archOptsReg   = 7914
    archStatusReg = 8000
    archLenReg    = 8002
    archDataStart = 8003

    archStatusCollecting = 1
    archStatusReady       = 2
)

// vkm360ArchiveState tracks the emulated archive request lifecycle for a
// single connection. A fresh connection starts with no pending request;
// writing archIDReg..archOptsReg starts one; the FIRST status poll after
// that returns "collecting", every subsequent poll returns "ready" with a
// fixed test payload. This exercises the real polling loop in
// mb_request_poll_string.waitReady rather than trivially succeeding on the
// first status read.
type vkm360ArchiveState struct {
    requested   bool
    statusPolls int
}

const vkm360TestArchiveString = "V01{Расход}=123.45 кг/с;V02{Масса}=678.90 кг;"

func RunVKM360(addr string) error {
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
        go handleVKM360Conn(conn)
    }
}

func handleVKM360Conn(conn net.Conn) {
    defer conn.Close()

    state := &vkm360ArchiveState{}
    buf := make([]byte, 512)

    for {
        n, err := conn.Read(buf)
        if err != nil || n < 8 {
            return
        }
        frame := append([]byte(nil), buf[:n]...)

        txID, unitID, pdu, err := modbus.ParseTCPFrame(frame)
        if err != nil {
            log.Printf("[sim vkm360] bad frame: %v\n", err)
            continue
        }
        if len(pdu) < 5 {
            continue
        }

        respPDU := buildVKM360Response(pdu, state)
        if respPDU == nil {
            continue
        }

        respFrame := modbus.BuildTCPFrame(txID, unitID, respPDU)
        if _, err := conn.Write(respFrame); err != nil {
            return
        }
    }
}

func buildVKM360Response(pdu []byte, state *vkm360ArchiveState) []byte {
    funcCode := pdu[0]
    addr := int(binary.BigEndian.Uint16(pdu[1:3]))

    switch funcCode {
    case 0x03, 0x04:
        return buildVKM360ReadResponse(funcCode, addr, pdu, state)
    case 0x06:
        handleVKM360Write(addr, state)
        return append([]byte{funcCode}, pdu[1:5]...)
    case 0x10:
        handleVKM360Write(addr, state)
        return append([]byte{funcCode}, pdu[1:5]...)
    }
    return nil
}

// handleVKM360Write marks an archive request as "started" once the options
// register (which is always written LAST by mb_request_poll_string) is seen.
func handleVKM360Write(addr int, state *vkm360ArchiveState) {
    if addr == archOptsReg {
        state.requested = true
        state.statusPolls = 0
    }
}

func buildVKM360ReadResponse(funcCode byte, addr int, pdu []byte, state *vkm360ArchiveState) []byte {
    qty := int(binary.BigEndian.Uint16(pdu[3:5]))

    switch {
    case addr == archStatusReg:
        return archiveStatusResponse(funcCode, state)
    case addr == archLenReg:
        return archiveLenResponse(funcCode)
    case addr == archDataStart:
        return archiveDataResponse(funcCode, qty)
    }

    data := readVKM360Registers(addr, qty)
    if data == nil {
        return nil
    }
    resp := make([]byte, 2+len(data))
    resp[0] = funcCode
    resp[1] = byte(len(data))
    copy(resp[2:], data)
    return resp
}

func archiveStatusResponse(funcCode byte, state *vkm360ArchiveState) []byte {
    status := uint16(archStatusReady)
    if state.requested {
        state.statusPolls++
        if state.statusPolls == 1 {
            status = archStatusCollecting
        } else {
            status = archStatusReady
        }
    }
    data := make([]byte, 2)
    binary.BigEndian.PutUint16(data, status)
    return []byte{funcCode, 2, data[0], data[1]}
}

func archiveLenResponse(funcCode byte) []byte {
    data := make([]byte, 2)
    binary.BigEndian.PutUint16(data, uint16(len(vkm360TestArchiveString)))
    return []byte{funcCode, 2, data[0], data[1]}
}

func archiveDataResponse(funcCode byte, qty int) []byte {
    payload := []byte(vkm360TestArchiveString)
    maxBytes := qty * 2
    if len(payload) > maxBytes {
        payload = payload[:maxBytes]
    } else {
        padded := make([]byte, maxBytes)
        copy(padded, payload)
        payload = padded
    }
    resp := make([]byte, 2+len(payload))
    resp[0] = funcCode
    resp[1] = byte(len(payload))
    copy(resp[2:], payload)
    return resp
}

func readVKM360Registers(addr int, qty int) []byte {
    out := make([]byte, 0, qty*2)
    remaining := qty * 2
    a := addr
    for remaining > 0 {
        raw, ok := vkm360Registers[a]
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