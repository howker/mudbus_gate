package archive
import (
    "context"
    "errors"
    "fmt"
    "mbgw/internal/protocol/modbus"
)
// MBIndexedBinary is the archive-read strategy for VZLET IVK-TER
// (CONTRACTS.md §6.2): a single register-block read at an address computed
// from the record index within a ring buffer. base_addr/record_regs come
// from the profile's archive.params (no hardcoded fallback, per
// IMPLEMENTATION_BACKLOG.md T7: "апрещено: хардкод форматов записей вне
// профиля") - a profile that omits them is a configuration error.
type MBIndexedBinary struct{}
func NewMBIndexedBinary() *MBIndexedBinary { return &MBIndexedBinary{} }
func (r *MBIndexedBinary) Strategy() string { return "mb_indexed_binary" }
// indexedBinaryParams extracts and validates the two required params.
// Accepts int/int64/float64 for baseAddr/recordRegs, since YAML->map[string]any
// via yaml.v3 decodes plain integers as int, but callers constructing
// ArchiveQuery.Params by hand (tests, JSON-sourced config) may use float64.
func indexedBinaryParams(params map[string]any) (baseAddr int, recordRegs int, err error) {
    baseAddr, err = intParam(params, "base_addr")
    if err != nil {
        return 0, 0, err
    }
    recordRegs, err = intParam(params, "record_regs")
    if err != nil {
        return 0, 0, err
    }
    if recordRegs <= 0 {
        return 0, 0, fmt.Errorf("mb_indexed_binary: params.record_regs must be > 0, got %d", recordRegs)
    }
    return baseAddr, recordRegs, nil
}
func intParam(params map[string]any, key string) (int, error) {
    raw, ok := params[key]
    if !ok {
        return 0, fmt.Errorf("mb_indexed_binary: missing required archive param %q in profile", key)
    }
    switch v := raw.(type) {
    case int:
        return v, nil
    case int64:
        return int(v), nil
    case float64:
        return int(v), nil
    default:
        return 0, fmt.Errorf("mb_indexed_binary: archive param %q has unsupported type %T", key, raw)
    }
}
func (r *MBIndexedBinary) Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    baseAddr, recordRegs, err := indexedBinaryParams(q.Params)
    if err != nil {
        return nil, err
    }
    addr := baseAddr + q.FromIndex*recordRegs
    req, err := modbus.BuildReadPDUWithQty("IR", addr, uint16(recordRegs))
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