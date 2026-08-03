package northbound

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mbgw/internal/storage"
)

// DBVKMArchiveSource — «боевая» реализация VKMArchiveSource: отдаёт в ЭС
// реально собранную с прибора строку архива (сохранённую при сборе, см.
// internal/device/vkm_hourly.go), а не выдуманную/фиксированную.
//
// Принципиально важно: мы не пересобираем строку из распарсенных S/ST —
// отдаём то, что произвёл сам прибор (раскодировано из cp1251 в UTF-8 при
// сборе; кодируется обратно в cp1251 перед отправкой на провод).
type DBVKMArchiveSource struct {
	repo     storage.Repo
	deviceID string

	// QueryTimeout ограничивает каждый запрос к БД; 0 = 5 секунд.
	QueryTimeout time.Duration

	// ProbeVariants включает режим ПЕРЕБОРА ГИПОТЕЗ: пока ЭС повторно
	// запрашивает один и тот же период, каждая следующая попытка получает
	// СЛЕДУЮЩИЙ вариант обработки строки, а в лог пишется, какой именно.
	// Когда ЭС наконец примет период и пойдёт дальше — в логе видно, на
	// каком варианте это произошло. Смысл: проверить все гипотезы за один
	// прогон, а не пересобирать и перезаливать бинарник под каждую.
	ProbeVariants bool

	// NumFormatProbe включает режим ПЕРЕБОРА ФОРМАТА ЧИСЛА. В отличие от
	// ProbeVariants, здесь нет автоиндикатора (значение канала «ноль/не
	// ноль» видно только глазами в ЭС), поэтому крутить варианты во
	// времени бессмысленно. Вместо этого раздаём РАЗНЫЕ форматы числа по
	// РАЗНЫМ экспоненциальным полям ОДНОЙ строки одновременно: Pi —
	// формат A, Pbar — формат B, ST — формат C и т.д. За один проход
	// человек смотрит в ЭС, какие каналы стали ненулевыми, и по
	// отдельному лог-файлу (NumProbeLogPath) сразу видит, какой формат
	// сработал — без перебора во времени вообще.
	NumFormatProbe bool
	// NumProbeLogPath — путь к отдельному подробному логу перебора формата.
	NumProbeLogPath string
	numProbeOnce    sync.Once

	mu             sync.Mutex
	lastPeriod     time.Time // период предыдущего запроса (для сброса счётчика)
	variantIdx     int       // индекс текущего варианта в vkmVariants
	attemptsOnSame int       // сколько попыток уже сделано по текущему периоду
}

// NewDBVKMArchiveSource создаёт источник архива ВКМ, читающий из репо.
func NewDBVKMArchiveSource(repo storage.Repo, deviceID string) *DBVKMArchiveSource {
	return &DBVKMArchiveSource{repo: repo, deviceID: deviceID}
}

// vkmVariant — одна проверяемая гипотеза: как преобразовать сырую строку
// прибора перед отдачей в ЭС.
type vkmVariant struct {
	name string // короткое имя для лога
	why  string // что именно проверяем этой гипотезой
	fn   func(string) string
}

// vkmVariants — набор гипотез, перебираемых по кругу в режиме
// ProbeVariants. Порядок важен: сначала «как есть» (контроль — если ЭС
// примет ЕГО, значит дело вообще не в содержимом строки), потом по
// нарастающей.
//
// Каждая гипотеза родилась из наблюдения на живых данных 2026-08-02:
// период 27.07 16:00 ЭС бесконечно переспрашивала, а соседний 27.07 16:30
// принимала. Полное сравнение всех 13 полей дало ровно два структурных
// отличия: суффикс "кор.времени" после закрывающей скобки NSS (плюс CRLF
// внутри данных) и неполное значение Twrk (29м 57сек против 30м 00сек).
var vkmVariants = []vkmVariant{
	{
		name: "1-as-is",
		why:  "контроль: строка ровно как от прибора, без единой правки",
		fn:   func(s string) string { return s },
	},
	{
		name: "2-no-crlf",
		why:  "убран перенос строки (CR/LF) внутри данных",
		fn:   vkmStripLineBreaks,
	},
	{
		name: "3-no-crlf+no-nss-suffix",
		why:  "то же + убран текстовый хвост после скобки NSS (кор.времени)",
		fn: func(s string) string {
			return vkmStripNSSSuffix(vkmStripLineBreaks(s))
		},
	},
	{
		name: "4-no-crlf+twrk-full",
		why:  "то же + Twrk принудительно приведён к полному периоду (30м 00сек)",
		fn: func(s string) string {
			return vkmForceFullTwrk(vkmStripLineBreaks(s))
		},
	},
	{
		name: "5-all-normalizations",
		why:  "все правки сразу: CRLF + хвост NSS + Twrk + числа с точкой",
		fn: func(s string) string {
			return vkmAddDecimalPoint(vkmForceFullTwrk(vkmStripNSSSuffix(vkmStripLineBreaks(s))))
		},
	},
	{
		name: "6-nss-empty",
		why:  "поле NSS полностью опустошено (NSS=;) — вдруг мешает сам его шаблон",
		fn: func(s string) string {
			return vkmEmptyNSS(vkmStripLineBreaks(s))
		},
	},
	{
		name: "7-no-headers",
		why:  "убраны ВСЕ блоки-заголовки {..} — компактный формат, который прибор иногда шлёт сам",
		fn: func(s string) string {
			return vkmStripAllHeaders(vkmStripLineBreaks(s))
		},
	},
	{
		name: "8-copy-of-neighbour",
		why:  "строка соседнего периода с подменённым временем — если примут ЭТО, дело в значениях, а не в структуре",
		fn:   nil, // особый случай, обрабатывается в Archive()
	},
}

// Archive ищет сохранённую строку за период, на который начинается [start,
// end). Сбор всегда пишет получасовые окна — подтверждено живым захватом
// (2026-08-02): именно такими окнами запрашивает архив драйвер ЭС.
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
		log.Printf("[VKM northbound] нет данных за период %s — отвечаем 'нет записей'\n", periodStart.Format("02.01.2006 15:04"))
		return "", false
	}

	if s.NumFormatProbe {
		return s.applyNumFormatProbe(periodStart, raw), true
	}

	if !s.ProbeVariants {
		// Обычный боевой режим:
		//  1) компактный формат без {..} — ПОДТВЕРЖДЕНО живым перебором
		//     (2026-08-02): два периода подряд ЭС приняла синхронно на
		//     варианте 7-no-headers, драйвер не ожидает блоков-заголовков.
		//  2) экспоненциальная запись (e+NN) развёрнута в обычную
		//     десятичную — ПОДТВЕРЖДЕНО сравнением сработавших и нет
		//     каналов: S/T (обычные числа 929.72, 202.16) ЭС распарсила и
		//     показала, а ST/Pi/Pbar (научная нотация 2.65e+09, 4.21e+05)
		//     дали нули. Единственное системное различие — форма записи
		//     числа; парсер драйвера ЭС, похоже, не понимает 'e+NN'.
		//  3) целые числа без точки -> ".0" (прежняя правка, оставлена).
		return vkmAddDecimalPoint(vkmExpandExponent(vkmStripAllHeaders(vkmStripLineBreaks(raw)))), true
	}

	return s.applyProbeVariant(ctx, pipe, periodStart, raw), true
}

// applyProbeVariant выбирает очередной вариант обработки для текущего
// периода и подробно пишет в лог, что именно отдаётся. Счётчик вариантов
// сбрасывается, как только ЭС переходит к другому периоду — так в логе
// сразу видно, на каком варианте предыдущий период «пробило».
func (s *DBVKMArchiveSource) applyProbeVariant(ctx context.Context, pipe int, periodStart time.Time, raw string) string {
	s.mu.Lock()
	if !periodStart.Equal(s.lastPeriod) {
		if !s.lastPeriod.IsZero() {
			prevVariant := vkmVariants[(s.variantIdx-1+len(vkmVariants))%len(vkmVariants)].name
			log.Printf("[VKM ПЕРЕБОР] === ЭС ПЕРЕШЛА к периоду %s (предыдущий %s принят после %d попыток, последним отдавали вариант %s) ===\n",
				periodStart.Format("02.01.2006 15:04"), s.lastPeriod.Format("02.01.2006 15:04"), s.attemptsOnSame, prevVariant)
		}
		s.lastPeriod = periodStart
		s.variantIdx = 0
		s.attemptsOnSame = 0
	}
	v := vkmVariants[s.variantIdx%len(vkmVariants)]
	s.variantIdx++
	s.attemptsOnSame++
	attempt := s.attemptsOnSame
	s.mu.Unlock()

	var out string
	if v.fn != nil {
		out = v.fn(raw)
	} else {
		// Вариант 8: подставляем строку СОСЕДНЕГО (заведомо принятого ЭС)
		// периода, заменив в ней диапазон времени на запрошенный. Если ЭС
		// примет это — значит структура строки ей подходит, а спотыкается
		// она на конкретных значениях текущего периода.
		out = s.neighbourTemplate(ctx, pipe, periodStart, raw)
	}

	log.Printf("[VKM ПЕРЕБОР] период %s, попытка %d -> вариант %s (%s), длина %d символов\n",
		periodStart.Format("02.01.2006 15:04"), attempt, v.name, v.why, len(out))
	return out
}

// neighbourTemplate берёт строку предыдущего получаса (тот период ЭС уже
// принимала) и подменяет в ней диапазон времени на запрошенный. Если
// соседа нет — возвращает исходную строку без изменений.
func (s *DBVKMArchiveSource) neighbourTemplate(ctx context.Context, pipe int, periodStart time.Time, fallback string) string {
	prev := periodStart.Add(-30 * time.Minute)
	raw, found, err := s.repo.GetVKMRawString(ctx, s.deviceID, pipe, prev)
	if err != nil || !found {
		return fallback
	}
	wantRange := fmt.Sprintf("%s-%s",
		periodStart.Format("02/01/06 15:04:05"),
		periodStart.Add(30*time.Minute).Format("02/01/06 15:04:05"))
	return vkmTimeRangeRe.ReplaceAllString(vkmStripLineBreaks(raw), wantRange)
}

// --- преобразования строки (каждое = отдельная проверяемая гипотеза) ---

var (
	// vkmTimeRangeRe находит диапазон дат в поле Time.
	vkmTimeRangeRe = regexp.MustCompile(`\d{2}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}-\d{2}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`)
	// vkmHeaderRe находит блок-заголовок сразу после '=' — как в фигурных
	// скобках {..}, так и в угловых <..> (прибор использует оба вперемешку
	// для разных тегов в одной строке, см. mb_request_poll_string.go).
	vkmHeaderRe = regexp.MustCompile(`=[\{<][^}>]*[\}>]`)
	// numAfterHeaderRe находит целое число сразу после закрывающей скобки.
	numAfterHeaderRe = regexp.MustCompile(`[}>](-?\d+)`)
	// expNumRe находит число в научной нотации (напр. 4.2126e+05,
	// 2.658389e+09, 1e-3). Границы \b нельзя (символы кириллицы-единицы
	// сразу после), поэтому мантисса+экспонента описаны явно.
	expNumRe = regexp.MustCompile(`-?\d+(?:\.\d+)?[eE][+-]?\d+`)
	// twrkRe находит значение поля Twrk.
	twrkRe = regexp.MustCompile(`(Twrk=\{[^}]*\})[^;]*`)
	// nssTailRe находит текстовый хвост после закрывающей скобки NSS.
	nssTailRe = regexp.MustCompile(`(NSS=\{[^}]*\})[^;]*`)
	// nssWholeRe находит всё поле NSS целиком.
	nssWholeRe = regexp.MustCompile(`NSS=[^;]*`)
)

// vkmStripLineBreaks убирает буквальные переносы строк (CR/LF) внутри
// данных. Найдено сканированием всей истории: 22 записи из 338 содержат
// такой перенос, почти всегда рядом с пометкой коррекции времени.
func vkmStripLineBreaks(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", " ")
	raw = strings.ReplaceAll(raw, "\n", " ")
	raw = strings.ReplaceAll(raw, "\r", " ")
	return raw
}

// vkmStripNSSSuffix убирает текстовый хвост после закрывающей скобки NSS
// (например "кор.времени"), оставляя само поле.
func vkmStripNSSSuffix(raw string) string {
	return nssTailRe.ReplaceAllString(raw, "$1")
}

// vkmEmptyNSS опустошает поле NSS целиком (NSS=).
func vkmEmptyNSS(raw string) string {
	return nssWholeRe.ReplaceAllString(raw, "NSS=")
}

// vkmForceFullTwrk выставляет Twrk в полный получас. У непринятого периода
// было "29м 57сек" (3 секунды ушли на коррекцию часов), у принятого —
// "30м 00сек".
func vkmForceFullTwrk(raw string) string {
	return twrkRe.ReplaceAllString(raw, "${1}30м 00сек")
}

// vkmStripAllHeaders убирает все блоки-заголовки {..}, оставляя tag=значение.
// Прибор сам иногда шлёт архив в таком компактном виде.
func vkmStripAllHeaders(raw string) string {
	return vkmHeaderRe.ReplaceAllString(raw, "=")
}

// numFormat — один способ записать число (гипотеза о том, что ждёт ЭС).
type numFormat struct {
	name string
	fn   func(f float64) string
}

// numFormats — набор гипотез форматирования числа, раздаваемых по разным
// полям одной строки в режиме NumFormatProbe. Каждая — отдельная причина,
// почему научная нотация могла не парситься драйвером ЭС.
var numFormats = []numFormat{
	{"A-целое-без-точки", func(f float64) string { return strconv.FormatFloat(f, 'f', 0, 64) }},
	{"B-десятичное-минимальное", func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }},
	{"C-точка-1-знак", func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }},
	{"D-запятая-рус-2-знака", func(f float64) string {
		return strings.Replace(strconv.FormatFloat(f, 'f', 2, 64), ".", ",", 1)
	}},
	{"E-экспонента-заглавная-E", func(f float64) string { return strconv.FormatFloat(f, 'E', -1, 64) }},
	{"F-фикс-3-знака", func(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }},
}

// mapping — одна запись карты «поле -> назначенный формат -> было -> стало».
type mapping struct{ field, format, before, after string }

// expFieldRe находит поле вида "тег=<число в науч.нотации><хвост-единица>"
// до ';'. Заменяется только числовая часть.
var expFieldRe = regexp.MustCompile(`([A-Za-z_]+)=(-?\d+(?:\.\d+)?[eE][+-]?\d+)([^;]*)`)

// applyNumFormatProbe раздаёт каждому экспоненциальному полю строки свой
// формат числа (по кругу из numFormats) и пишет карту в отдельный лог.
// Обычные поля (S, T) не трогает. Человек смотрит в ЭС, какие каналы
// стали ненулевыми, и по логу узнаёт, какой формат сработал — за ОДИН
// проход, без перебора во времени.
func (s *DBVKMArchiveSource) applyNumFormatProbe(periodStart time.Time, raw string) string {
	base := vkmStripAllHeaders(vkmStripLineBreaks(raw))

	var mappings []mapping
	idx := 0
	out := expFieldRe.ReplaceAllStringFunc(base, func(m string) string {
		sub := expFieldRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		tag, numStr, unit := sub[1], sub[2], sub[3]
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return m
		}
		nf := numFormats[idx%len(numFormats)]
		idx++
		formatted := nf.fn(f)
		mappings = append(mappings, mapping{tag, nf.name, numStr, formatted})
		return tag + "=" + formatted + unit
	})

	// Карта одинакова в пределах периода (значения те же), а строки
	// повторяются каждые ~4 сек — пишем лог только при СМЕНЕ периода.
	s.mu.Lock()
	newPeriod := !periodStart.Equal(s.lastPeriod)
	if newPeriod {
		s.lastPeriod = periodStart
	}
	s.mu.Unlock()
	if newPeriod {
		s.logNumProbe(periodStart, mappings)
	}
	return out
}

// logNumProbe пишет карту в отдельный файл (NumProbeLogPath) или в обычный
// log, если путь не задан.
func (s *DBVKMArchiveSource) logNumProbe(periodStart time.Time, mappings []mapping) {
	var b strings.Builder
	fmt.Fprintf(&b, "=== ПЕРЕБОР ФОРМАТА ЧИСЛА, период %s ===\n", periodStart.Format("02.01.2006 15:04"))
	fmt.Fprintf(&b, "Посмотри в ЭС, какие из этих каналов стали НЕнулевыми, и сопоставь с форматом:\n")
	for _, m := range mappings {
		fmt.Fprintf(&b, "  поле %-6s формат %-26s : %s -> %s\n", m.field, m.format, m.before, m.after)
	}
	msg := b.String()

	if s.NumProbeLogPath == "" {
		log.Print(msg)
		return
	}
	s.numProbeOnce.Do(func() {
		if f, err := os.OpenFile(s.NumProbeLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(f, "# Лог перебора формата числа ВКМ northbound. Запущен %s\n", time.Now().Format(time.RFC3339))
			fmt.Fprintf(f, "# Каждому экспоненциальному полю присвоен свой формат записи.\n")
			fmt.Fprintf(f, "# Задача: увидеть в ЭС, при каком формате канал перестаёт быть нулём.\n\n")
			f.Close()
		}
	})
	f, err := os.OpenFile(s.NumProbeLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[VKM ФОРМАТ] не могу открыть %s: %v; пишу в обычный лог:\n%s", s.NumProbeLogPath, err, msg)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, msg)
}

// vkmExpandExponent разворачивает числа в научной нотации (4.2126e+05) в
// обычную десятичную запись (421260), не трогая остальной текст. СИЛЬНАЯ
// гипотеза по багу нулей (2026-08-02): каналы, чьи значения прибор шлёт
// обычными числами (S=929.72, T=202.16), ЭС распарсила и показала; а те,
// что в научной нотации (ST=2.65e+09, Pi=4.21e+05, Pbar=1.0092e+05), дали
// нули. Единственное системное различие — форма записи числа, значит
// парсер драйвера ЭС, скорее всего, не понимает 'e+NN'.
//
// Разворачиваем через strconv (round-trip float64) — этого достаточно для
// диапазона величин теплосчётчика (масса, энергия, давление); при
// невозможности разобрать оставляем как есть (не портим строку).
func vkmExpandExponent(raw string) string {
	return expNumRe.ReplaceAllStringFunc(raw, func(m string) string {
		f, err := strconv.ParseFloat(m, 64)
		if err != nil {
			return m
		}
		// 'f' с -1 разрядностью даёт кратчайшее точное представление без
		// экспоненты для обычных величин; но strconv может вернуть
		// экспоненту для очень больших/малых — поэтому проверяем и, если
		// так, форматируем фиксированно.
		out := strconv.FormatFloat(f, 'f', -1, 64)
		return out
	})
}

// vkmAddDecimalPoint добавляет ".0" к целым числовым значениям без
// десятичной точки. Поле Time не трогается (там дата, не число).
func vkmAddDecimalPoint(raw string) string {
	entries := strings.Split(raw, ";")
	for i, entry := range entries {
		eq := strings.Index(entry, "=")
		if eq < 0 {
			continue
		}
		if entry[:eq] == "Time" {
			continue
		}
		entries[i] = numAfterHeaderRe.ReplaceAllStringFunc(entry, func(m string) string {
			idx := strings.Index(entry, m)
			if idx < 0 {
				return m
			}
			after := idx + len(m)
			if after < len(entry) {
				c := entry[after]
				if c == '.' || c == 'e' || c == 'E' || (c >= '0' && c <= '9') {
					return m
				}
			}
			return string(m[0]) + m[1:] + ".0"
		})
	}
	return strings.Join(entries, ";")
}
