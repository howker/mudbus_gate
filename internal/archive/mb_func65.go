package archive
import (
    "context"
    "encoding/binary"
    "errors"
    "fmt"

    "mbgw/internal/protocol/modbus"
)
// MBFunc65 is the archive-read strategy using Modbus function 65 (0x41),
// the generic VZLET "read record array" mechanism (prtkl_Modbus_pril_1.pdf,
// Приложение А) shared by every VZLET device (IVK-TER, TSRV-024, and any
// future VZLET model) - not TSRV-specific despite the historical name.
// archive_type (the array/archive number) comes from the profile's
// archive.params, since it differs per device and per archive within a
// device (CONTRACTS.md §6.2/§6.3). Nonexistent-record marker: time
// 0x00000000/0xFFFFFFFF in the first 4 bytes. CRC (§5.4) is checked only
// when the profile's record_layout marks a field crc:true - some VZLET
// devices' archive records have no CRC at all (e.g. IVK-TER's 30-byte
// hourly record, str_arh_ivk_ter.pdf table 3), so a blanket "last 2 bytes
// are CRC" is wrong in general.
type MBFunc65 struct{}
func NewMBFunc65() *MBFunc65 { return &MBFunc65{} }
func (r *MBFunc65) Strategy() string { return "mb_func65" }
// archiveTypeParam extracts the required "archive_type" param (the VZLET
// array/archive number, 0-based per prtkl_Modbus_pril_1.pdf).
func archiveTypeParam(params map[string]any) (uint16, error) {
    raw, ok := params["archive_type"]
    if !ok {
        return 0, fmt.Errorf("mb_func65: missing required archive param %q in profile", "archive_type")
    }
    switch v := raw.(type) {
    case int:
        return uint16(v), nil
    case int64:
        return uint16(v), nil
    case float64:
        return uint16(v), nil
    default:
        return 0, fmt.Errorf("mb_func65: archive param %q has unsupported type %T", "archive_type", raw)
    }
}
// layoutHasCRC reports whether the profile's record_layout declares a
// crc:true field - meaning this device's archive records are followed by
// a CRC16-modbus checksum (CONTRACTS.md §5.4). Devices whose records have
// no CRC at all (e.g. IVK-TER) simply don't set this on any field.
func layoutHasCRC(layout []RecordLayoutField) bool {
    for _, f := range layout {
        if f.CRC {
            return true
        }
    }
    return false
}
func (r *MBFunc65) Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    arrayNumber, err := archiveTypeParam(q.Params)
    if err != nil {
        return nil, err
    }

    var req []byte
    if !q.From.IsZero() {
        req = modbus.BuildArchive65TimePDU(arrayNumber, 1, q.From)
    } else {
        req = modbus.BuildArchive65IndexPDU(arrayNumber, 1, uint16(q.FromIndex))
    }

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

    // Record CRC check (last 2 bytes), per CONTRACTS.md §5.4, only for
    // devices whose record_layout declares a crc:true field. Devices with
    // no CRC in their record format (e.g. IVK-TER) are trusted as-is.
    crcOK := true
    if layoutHasCRC(q.RecordLayout) && len(raw) >= 2 {
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