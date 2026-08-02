package northbound

import (
	"context"
	"log"
	"time"

	"mbgw/internal/storage"
)

// DBVKMArchiveSource — «боевая» реализация VKMArchiveSource: отдаёт в ЭС
// реально собранную с прибора строку архива (сохранённую при почасовом
// сборе, см. internal/device/vkm_hourly.go), а не выдуманную/фиксированную
// (как FixedVKMSource, который был нужен только для отладки механики
// протокола до появления живого прибора).
//
// Принципиально важно: мы не пересобираем строку из распарсенных S/ST —
// отдаём ровно то, что произвёл сам прибор (уже раскодировано из cp1251 в
// UTF-8 при сборе; кодируем обратно в cp1251 перед отправкой на провод —
// см. writeVKMResultString/cp1251Encode). Так драйвер ЭС видит те же
// байты, что видел бы при прямом опросе настоящего ВКМ-360.
type DBVKMArchiveSource struct {
	repo     storage.Repo
	deviceID string

	// QueryTimeout ограничивает каждый запрос к БД; 0 = 5 секунд.
	QueryTimeout time.Duration
}

// NewDBVKMArchiveSource создаёт источник архива ВКМ, читающий из репо.
func NewDBVKMArchiveSource(repo storage.Repo, deviceID string) *DBVKMArchiveSource {
	return &DBVKMArchiveSource{repo: repo, deviceID: deviceID}
}

// Archive ищет сохранённую строку за период, на который начинается
// [start, end). Наш сбор всегда пишет получасовые окна (periodStart,
// periodStart+30мин) — подтверждено живым захватом (2026-08-02): именно
// такими окнами реально запрашивает архив драйвер ЭС (УВП-280), а не
// часовыми, как предполагалось раньше. Если ЭС просит окно другой длины/
// выравнивания, вернём ok=false (нет записей), а не подгонять что-то
// приблизительное.
func (s *DBVKMArchiveSource) Archive(pipe int, start, end time.Time, opts uint16) (string, bool) {
	timeout := s.QueryTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	periodStart := start.Truncate(30 * time.Minute)
	raw, found, err := s.repo.GetVKMRawString(ctx, s.deviceID, pipe, periodStart)
	if err != nil {
		log.Printf("[VKM northbound] ошибка чтения архива за %s: %v\n", periodStart.Format("02.01.2006 15:04"), err)
		return "", false
	}
	if !found {
		return "", false
	}
	return raw, true
}
