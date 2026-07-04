package archive

import (
    "context"
    "encoding/binary"
    "fmt"
    "strconv"
    "strings"
    "time"

    "mbgw/internal/protocol/modbus"
)

// MBRequestPollString implements the VKM-360 stateful archive strategy
// (CONTRACTS.md §6.1): write request registers 7900-7914 (options register
// 7914 written last triggers the collection), poll status register 8000
// until ready, then read length (8002) and the tagged ANSI string (8003+).

const (
    vkmArchIDReg       = 7900
    vkmArchPipeReg     = 7901
    vkmArchStartTimeReg = 7902 // 6 registers: day,month,year,hour,minute,second
    vkmArchEndTimeReg   = 7908 // 6 registers
    vkmArchOptionsReg   = 7914 // written last, triggers collection

    vkmArchStatusReg  = 8000
    vkmArchEchoIDReg  = 8001
    vkmArchLenReg     = 8002
    vkmArchDataStart  = 8003
    vkmArchDataEnd    = 9999

    vkmArchStatusExpired    = 0
    vkmArchStatusCollecting = 1
    vkmArchStatusReady      = 2
    vkmArchStatusNoRecords  = 3
    vkmArchStatusBadStart   = 4
    vkmArchStatusBadEnd     = 5
    vkmArchStatusBadPipe    = 6
    vkmArchStatusTooLarge   = 7

    vkmArchPollInterval  = 200 * time.Millisecond
    vkmArchCollectTimeout = 15 * time.Second
    vkmArchBusyRetryDelay = 500 * time.Millisecond
    vkmArchBusyMaxWait    = 300 * time.Second
)

type MBRequestPollString struct{}

func NewMBRequestPollString() *MBRequestPollString { return &MBRequestPollString{} }

func (r *MBRequestPollString) Strategy() string { return "mb_request_poll_string" }

func (r *MBRequestPollString) Read(ctx context.Context, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    if err := r.startRequest(ctx, tx, q); err != nil {
        return nil, err
    }

    if err := r.waitReady(ctx, tx); err != nil {
        return nil, err
    }

    raw, err := r.readResultString(ctx, tx)
    if err != nil {
        return nil, err
    }

    return parseTaggedString(raw)
}

// startRequest writes the request registers. The options register (7914) is
// written LAST, which is what actually triggers the device to start collecting.
// If a previous request is still running, the device answers with Modbus
// exception 06 (BUSY); this is retried with backoff up to vkmArchBusyMaxWait.
func (r *MBRequestPollString) startRequest(ctx context.Context, tx Transactor, q ArchiveQuery) error {
    timeRegs := func(t time.Time) []uint16 {
        return []uint16{
            uint16(t.Day()), uint16(t.Month()), uint16(t.Year()),
            uint16(t.Hour()), uint16(t.Minute()), uint16(t.Second()),
        }
    }

    writes := []struct {
        addr   int
        values []uint16
    }{
        {vkmArchIDReg, []uint16{1}},
        {vkmArchPipeReg, []uint16{uint16(q.Instance)}},
        {vkmArchStartTimeReg, timeRegs(q.From)},
        {vkmArchEndTimeReg, timeRegs(q.To)},
    }

    deadline := time.Now().Add(vkmArchBusyMaxWait)
    for _, w := range writes {
        if err := r.writeRegistersWithBusyRetry(ctx, tx, w.addr, w.values, deadline); err != nil {
            return fmt.Errorf("write archive request register %d: %w", w.addr, err)
        }
    }

    // Options register is written last and triggers collection.
    // Bit 2 (include_tags) and bit 1 (include_header) are set so the
    // returned string includes tags, per CONTRACTS §6.1 format.
    options := uint16(0b0110)
    if err := r.writeRegistersWithBusyRetry(ctx, tx, vkmArchOptionsReg, []uint16{options}, deadline); err != nil {
        return fmt.Errorf("write archive options register: %w", err)
    }

    return nil
}

func (r *MBRequestPollString) writeRegistersWithBusyRetry(ctx context.Context, tx Transactor, addr int, values []uint16, deadline time.Time) error {
    var pduReq []byte
    var err error
    if len(values) == 1 {
        pduReq = modbus.BuildWriteSingleRegisterPDU(addr, values[0])
    } else {
        pduReq, err = modbus.BuildWriteMultipleRegistersPDU(addr, values)
        if err != nil {
            return err
        }
    }

    for {
        _, err := tx.Transact(ctx, pduReq)
        if err == nil {
            return nil
        }
        if !isBusyErr(err) {
            return err
        }
        if time.Now().After(deadline) {
            return fmt.Errorf("busy retry deadline exceeded: %w", err)
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(vkmArchBusyRetryDelay):
        }
    }
}

// waitReady polls the status register until the archive is ready to be read,
// or a terminal error/timeout condition is reached.
func (r *MBRequestPollString) waitReady(ctx context.Context, tx Transactor) error {
    deadline := time.Now().Add(vkmArchCollectTimeout)
    for {
        pduReq, err := modbus.BuildReadPDU("IR", vkmArchStatusReg, "int16")
        if err != nil {
            return err
        }
        pduResp, err := tx.Transact(ctx, pduReq)
        if err != nil {
            if isBusyErr(err) {
                if time.Now().After(deadline) {
                    return fmt.Errorf("status poll busy timeout")
                }
                select {
                case <-ctx.Done():
                    return ctx.Err()
                case <-time.After(vkmArchPollInterval):
                }
                continue
            }
            return err
        }
        if len(pduResp) < 4 {
            return fmt.Errorf("status response too short")
        }
        status := binary.BigEndian.Uint16(pduResp[2:4])

        switch status {
        case vkmArchStatusReady:
            return nil
        case vkmArchStatusCollecting:
            if time.Now().After(deadline) {
                return fmt.Errorf("archive collection timeout")
            }
            select {
            case <-ctx.Done():
                return ctx.Err()
            case <-time.After(vkmArchPollInterval):
            }
        case vkmArchStatusNoRecords:
            return fmt.Errorf("no records in requested period")
        case vkmArchStatusExpired:
            return fmt.Errorf("archive request expired or not accepted")
        case vkmArchStatusBadStart:
            return fmt.Errorf("invalid start time")
        case vkmArchStatusBadEnd:
            return fmt.Errorf("invalid end time")
        case vkmArchStatusBadPipe:
            return fmt.Errorf("invalid pipe number")
        case vkmArchStatusTooLarge:
            return fmt.Errorf("requested period too large")
        default:
            return fmt.Errorf("unknown archive status: %d", status)
        }
    }
}

// readResultString reads the string length and then the tagged ANSI payload.
func (r *MBRequestPollString) readResultString(ctx context.Context, tx Transactor) (string, error) {
    lenPDU, err := modbus.BuildReadPDU("IR", vkmArchLenReg, "int16")
    if err != nil {
        return "", err
    }
    lenResp, err := tx.Transact(ctx, lenPDU)
    if err != nil {
        return "", fmt.Errorf("read archive length: %w", err)
    }
    if len(lenResp) < 4 {
        return "", fmt.Errorf("length response too short")
    }
    strLen := int(binary.BigEndian.Uint16(lenResp[2:4]))
    if strLen <= 0 {
        return "", fmt.Errorf("invalid archive string length: %d", strLen)
    }

    maxRegs := vkmArchDataEnd - vkmArchDataStart + 1
    neededRegs := (strLen + 1) / 2
    if neededRegs > maxRegs {
        return "", fmt.Errorf("archive string length %d exceeds available register range", strLen)
    }

    dataPDU, err := modbus.BuildReadPDUWithQty("IR", vkmArchDataStart, uint16(neededRegs))
    if err != nil {
        return "", err
    }
    dataResp, err := tx.Transact(ctx, dataPDU)
    if err != nil {
        return "", fmt.Errorf("read archive string: %w", err)
    }
    if len(dataResp) < 2 {
        return "", fmt.Errorf("archive string response too short")
    }

    payload := dataResp[2:]
    if len(payload) > strLen {
        payload = payload[:strLen]
    }
    return string(payload), nil
}

// parseTaggedString parses the VKM archive string format:
//   тег{шапка}=значение ед.изм;тег2{шапка2}=значение2 ед.изм2;...
func parseTaggedString(raw string) ([]ArchiveRecord, error) {
    raw = strings.TrimRight(raw, "\x00")
    entries := strings.Split(raw, ";")

    fields := make(map[string]any)
    for _, entry := range entries {
        entry = strings.TrimSpace(entry)
        if entry == "" {
            continue
        }

        eq := strings.Index(entry, "=")
        if eq < 0 {
            continue
        }
        tagPart := entry[:eq]
        valuePart := strings.TrimSpace(entry[eq+1:])

        tag := tagPart
        if br := strings.Index(tagPart, "{"); br >= 0 {
            tag = strings.TrimSpace(tagPart[:br])
        }

        valueStr := valuePart
        unit := ""
        if sp := strings.LastIndex(valuePart, " "); sp >= 0 {
            valueStr = valuePart[:sp]
            unit = valuePart[sp+1:]
        }

        if f, err := strconv.ParseFloat(strings.TrimSpace(valueStr), 64); err == nil {
            fields[tag] = f
        } else {
            fields[tag] = strings.TrimSpace(valueStr)
        }
        if unit != "" {
            fields[tag+"_unit"] = unit
        }
    }

    rec := ArchiveRecord{
        RecordTS: time.Now().UTC(),
        Fields:   fields,
        CRCOK:    true,
        Raw:      []byte(raw),
    }
    return []ArchiveRecord{rec}, nil
}

// isBusyErr reports whether err represents a Modbus exception 06 (SLAVE DEVICE BUSY).
// Delegates to modbus.IsBusyError, which matches via errors.As on *modbus.ExceptionError
// (see internal/protocol/modbus/exception.go) instead of string matching.
func isBusyErr(err error) bool {
    return modbus.IsBusyError(err)
}

func init() { Register(NewMBRequestPollString()) }