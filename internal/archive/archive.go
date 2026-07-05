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

    // Params carries the profile-declared archive.params block (mirrors
    // profile.Archive.Params via map[string]any, same layering rationale
    // as RecordLayout above: archive must not import profile directly).
    // Strategy-specific keys are documented on each strategy
    // (e.g. mb_indexed_binary.go: "base_addr", "record_regs";
    // mb_func65.go: "archive_type"). Per IMPLEMENTATION_BACKLOG.md T7
    // ("апрещено: хардкод форматов записей вне профиля"), strategies
    // must treat a missing required key as an error, not fall back to a
    // hardcoded default.
    Params map[string]any
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

// ArchiveSession is a minimal, locally-declared interface (not imported
// from internal/session, to keep archive's dependency direction correct
// per LLD.md - archive must not depend on session) that lets an archive
// strategy check session readiness before a multi-step exchange. This
// matters for protocols like Merkuriy, where long-response reads should
// only be attempted while the channel session is ready; sess may be nil
// for devices with session type "none" (e.g. TSRV-024), and strategies
// that don't need it (e.g. mb_indexed_binary, mb_func65) simply ignore it.
type ArchiveSession interface {
    State() string
}

// ArchiveReader - стратегия чтения архива прибора.
// Read сам проводит транзакцию (может быть многошаговой - см. mb_request_poll_string)
// и возвращает уже декодированные записи. sess предоставляет доступ к
// состоянию сессии устройства (см. ArchiveSession); может быть nil.
type ArchiveReader interface {
    Strategy() string
    Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error)
}