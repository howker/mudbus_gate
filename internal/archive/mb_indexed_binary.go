package archive

import (
    "context"
    "errors"

    "mbgw/internal/protocol/modbus"
)

// MBIndexedBinary is the archive-read strategy for VZLET IVK-TER
// (CONTRACTS.md §6.2): a single register-block read at an address computed
// from the record index within a ring buffer. Base address and record size
// are hardcoded here for the vertical slice; profile-driven params (per
// archive) are a follow-up (see backlog).
type MBIndexedBinary struct{}

const (
    ivkArchiveBaseAddr    = 200 // base IR address of record #0
    ivkArchiveRecordRegs  = 4   // registers per record (8 bytes)
)

func NewMBIndexedBinary() *MBIndexedBinary { return &MBIndexedBinary{} }

func (r *MBIndexedBinary) Strategy() string { return "mb_indexed_binary" }

func (r *MBIndexedBinary) Read(ctx context.Context, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    addr := ivkArchiveBaseAddr + q.FromIndex*ivkArchiveRecordRegs

    req, err := modbus.BuildReadPDUWithQty("IR", addr, uint16(ivkArchiveRecordRegs))
    if err != nil {
        return nil, err
    }

    respPDU, err := tx.Transact(ctx, req)
    if err != nil {
        return nil, err
    }
    if len(respPDU) < 2 {
        return nil, errors.New("archive response too short")
    }
    raw := respPDU[2:] // skip function code + byte count

    if len(raw) < 8 {
        return nil, errors.New("archive record too short")
    }

    fields := decodeRecord(raw)
    if len(q.RecordLayout) > 0 {
        fields = decodeByLayout(raw, q.RecordLayout, q.WordOrder32, q.WordOrder64)
    }

    rec := ArchiveRecord{
        Raw:    append([]byte(nil), raw...),
        Fields: fields,
        CRCOK:  true,
    }
    return []ArchiveRecord{rec}, nil
}

func init() { Register(NewMBIndexedBinary()) }