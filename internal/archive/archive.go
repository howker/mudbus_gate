package archive

import (
    "context"
    "time"
)

// ArchiveQuery описывает, что и за какой период/индекс нужно прочитать из архива прибора.
type ArchiveQuery struct {
    DeviceID  string
    ArchiveID string
    Instance  int
    From      time.Time
    To        time.Time
    FromIndex int
    ToIndex   int

    // RecordLayout, WordOrder32, WordOrder64 carry the profile-declared
    // record structure and byte order so strategies can decode fields
    // properly instead of falling back to the generic decodeRecord
    // heuristic. RecordLayout uses []profile.RecordField via an alias
    // to avoid importing the profile package directly in ArchiveQuery
    // callers that only need Read (see mb_indexed_binary.go/mb_func65.go).
    RecordLayout []RecordLayoutField
    WordOrder32  string
    WordOrder64  string
}

// RecordLayoutField mirrors profile.RecordField's shape without importing
// the profile package here (keeps archive package dependency-light per
// LLD.md layering rules: archive depends on protocol/codec/session, not
// on higher-level packages like profile/device).
type RecordLayoutField struct {
    Offset int
    Name   string
    Type   string
    Unit   string
    Scale  float64
    CRC    bool
    Epoch  string
}

// ArchiveRecord - одна декодированная запись архива.
type ArchiveRecord struct {
    RecordTS time.Time
    Fields   map[string]any // декодированные поля record_layout (имя поля -> значение)
    CRCOK    bool
    Raw      []byte
}

// Transactor - минимальный контракт, которого достаточно стратегии архива,
// чтобы провести обмен с прибором (запрос/ответ), не зависящий напрямую
// от конкретных пакетов protocol/session (избегаем циклов импорта).
type Transactor interface {
    Transact(ctx context.Context, req []byte) (resp []byte, err error)
}

// ArchiveReader - стратегия чтения архива прибора.
// Read сам проводит транзакцию (может быть многошаговой - см. mb_request_poll_string)
// и возвращает уже декодированные записи.
type ArchiveReader interface {
    Strategy() string
    Read(ctx context.Context, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error)
}