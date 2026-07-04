package archive

import (
    "context"
    "encoding/binary"
    "errors"

    "mbgw/internal/protocol/modbus"
)

// MBFunc65 is the archive-read strategy using Modbus function 65 (0x41),
// specific to VZLET TSRV-024 (CONTRACTS.md §6.3): request by index or by
// time (period rounding is the caller's/profile's responsibility), a
// nonexistent-record marker of 0x00000000 or 0xFFFFFFFF in the first 4
// bytes (record time), and a CRC16-modbus checksum in the last 2 bytes.
// Full record_layout-driven field decoding is still a separate task;
// decodeRecord remains a placeholder for individual fields.
type MBFunc65 struct{}

func NewMBFunc65() *MBFunc65 { return &MBFunc65{} }

func (r *MBFunc65) Strategy() string { return "mb_func65" }

func (r *MBFunc65) Read(ctx context.Context, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    mode := byte(modbus.ArchiveModeByIndex)
    var value uint32
    if !q.From.IsZero() {
        mode = modbus.ArchiveModeByTime
        value = uint32(q.From.Unix())
    } else {
        value = uint32(q.FromIndex)
    }

    // Archive type index is taken from q.Instance for now (profile-level
    // archive type selection is a separate concern - see backlog).
    archiveType := byte(q.Instance)

    req := modbus.BuildArchive65PDU(archiveType, mode, value)

    respPDU, err := tx.Transact(ctx, req)
    if err != nil {
        return nil, err
    }

    raw, err := modbus.ParseArchive65Response(respPDU)
    if err != nil {
        return nil, err
    }
    if len(raw) == 0 {
        return nil, errors.New("empty func65 archive payload")
    }

    // Nonexistent-record marker check (first 4 bytes = time).
    if len(raw) >= 4 {
        ts := binary.BigEndian.Uint32(raw[0:4])
        if ts == 0x00000000 || ts == 0xFFFFFFFF {
            return []ArchiveRecord{}, nil
        }
    }

    // Record CRC check (last 2 bytes), per CONTRACTS.md §5.4: CRC16-modbus
    // over the whole record except the last 2 bytes, little-endian on wire.
    crcOK := true
    if len(raw) >= 2 {
        recordBody := raw[:len(raw)-2]
        wantCRC := uint16(raw[len(raw)-2]) | (uint16(raw[len(raw)-1]) << 8)
        gotCRC := modbus.CRC16(recordBody)
        crcOK = gotCRC == wantCRC
    }

    fields := decodeRecord(raw)
    if len(q.RecordLayout) > 0 {
        fields = decodeByLayout(raw, q.RecordLayout, q.WordOrder32, q.WordOrder64)
    }

    rec := ArchiveRecord{
        Raw:    append([]byte(nil), raw...),
        Fields: fields,
        CRCOK:  crcOK,
    }
    return []ArchiveRecord{rec}, nil
}

func init() { Register(NewMBFunc65()) }