package archive

import (
    "context"
    "fmt"
)

// MerkuriyLongResponseReader - стратегия чтения длинного ответа еркурия (профили мощности,
// журналы). онтракт CONTRACTS.md §6.4: команда чтения через открытую сессию,
// приём ответа до 255 байт (возможно постранично), режим повтора кода для
// нестабильных каналов (GPRS). ока не реализовано - см. backlog T8.
type MerkuriyLongResponseReader struct{}

func NewMerkuriyLongResponseReader() *MerkuriyLongResponseReader {
    return &MerkuriyLongResponseReader{}
}

func (r *MerkuriyLongResponseReader) Strategy() string {
    return "merkuriy_long_response"
}

func (r *MerkuriyLongResponseReader) Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
    return nil, fmt.Errorf("merkuriy_long_response not implemented")
}

func init() {
    Register(NewMerkuriyLongResponseReader())
}