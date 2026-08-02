package northbound

import (
	"context"
	"log"
	"regexp"
	"strings"
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
	return normalizeVKMNumbers(raw), true
}

// numAfterHeaderRe matches an integer immediately following the closing
// bracket of a tag's header block ("}" or ">") — the value's start, before
// any unit text. Used only to detect whether that value has NO decimal
// point; the actual insertion decision also checks what follows (must not
// already be '.', 'e', 'E', or another digit — i.e. it really is a bare
// integer, not the integer part of something else).
var numAfterHeaderRe = regexp.MustCompile(`[}>](-?\d+)`)

// normalizeVKMNumbers добавляет ".0" к целым числовым значениям (без
// десятичной точки) перед отправкой в ЭС — ЭКСПЕРИМЕНТАЛЬНЫЙ фикс,
// основанный на живом наблюдении (2026-08-02): все получасовые периоды,
// которые ЭС успешно приняла, содержали ТОЛЬКО дробные значения (напр.
// dP=6566.1); все периоды, на которых ЭС бесконечно повторяла запрос без
// продвижения, содержали dP как ЦЕЛОЕ число без точки (напр. dP=12397).
// Похоже, парсер драйвера ЭС (УВП-280) требует десятичную точку в
// значении и не может разобрать целое число без неё. Правится только
// строка, отдаваемая НА ПРОВОД — сохранённые в БД сырые данные не
// трогаются (так что если гипотеза окажется неверной или потребует
// уточнения, откатить — вопрос удаления одного вызова, без потери
// собранной истории).
//
// Поле Time — единственное исключение: оно содержит дату/время
// ("30/07/26 08:30:00-..."), не число, и по формальному виду "число сразу
// после '}'" тоже могло бы совпасть с наивным регэкспом (день месяца) —
// разбираем по тегам явно, чтобы Time гарантированно не тронуть.
func normalizeVKMNumbers(raw string) string {
	entries := strings.Split(raw, ";")
	for i, entry := range entries {
		eq := strings.Index(entry, "=")
		if eq < 0 {
			continue
		}
		tag := entry[:eq]
		if tag == "Time" {
			continue // дата/время, не значение — не трогаем
		}
		entries[i] = numAfterHeaderRe.ReplaceAllStringFunc(entry, func(m string) string {
			bracket := m[0]
			numPart := m[1:]
			// Проверяем символ СРАЗУ после найденного числа в исходном
			// entry — если это точка, 'e'/'E' или ещё цифра, число на
			// самом деле не целое (это часть большего числа), трогать
			// нельзя. Ищем позицию совпадения в исходной строке заново,
			// т.к. ReplaceAllStringFunc не даёт контекст после матча.
			idx := strings.Index(entry, m)
			if idx < 0 {
				return m
			}
			after := idx + len(m)
			if after < len(entry) {
				c := entry[after]
				if c == '.' || c == 'e' || c == 'E' || (c >= '0' && c <= '9') {
					return m // не целое — часть большего числа, не трогаем
				}
			}
			return string(bracket) + numPart + ".0"
		})
	}
	return strings.Join(entries, ";")
}
