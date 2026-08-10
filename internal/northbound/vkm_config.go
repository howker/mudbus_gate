package northbound

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VKM carrier tunables, read from a plain key=value text file next to the
// exe on the Энергосфера server. The whole point is to iterate on the live
// system WITHOUT rebuilding/re-uploading the binary: edit the file, and the
// change is picked up on the next request (the file is re-read on a short
// interval, so usually not even a carrier restart is needed).
//
// File format (one per line, '#' comments and blank lines ignored):
//
//	year_mode = full        # full | y2000 | y1900  — clock year register encoding
//	clock_offset_sec = 0    # shift reported device clock by ±N seconds
//	min_ready_ms = 0        # keep status "collecting" for at least N ms
//	                        # after a request before reporting "ready"
//	period_format = datetime  # datetime | unix — period rendering (opts bit3)
//	time_layout = 02.01.2006 15:04:05   # timestamp format inside the string
//	archive_string = ...    # raw override; leave unset to auto-build a
//	                        # spec-compliant string (period + parameters)
//	cur_byte_order_probe = false  # true: раздать РАЗНЫМ офсетам области
//	                        # текущих показаний (2000-2600, IR) РАЗНЫЙ
//	                        # порядок байт float32 (ABCD/DCBA/BADC/CDAB),
//	                        # с картой в cur_byte_order_probe_log — на
//	                        # случай, если "Тепловая энергия"/"Давление"
//	                        # остаются нулями при заведомо верных значениях:
//	                        # возможно, порядок байт для ЭТОЙ области не
//	                        # совпадает с архивным протоколом (110/112/114).
//	cur_byte_order_probe_log = vkm_byteorder_probe.txt
//	field_scale = ST:0.001,Pi:0.01   # умножить значение конкретных тегов
//	                        # перед отдачей в ЭС (формат "ТЕГ:МНОЖИТЕЛЬ",
//	                        # через запятую). Найдено эмпирически
//	                        # (2026-08-05): драйвер УВП280 сам ставит
//	                        # State=1 (недостоверно) конкретно на Тепловую
//	                        # энергию/Давление, даже когда значение верное
//	                        # и конфигурация канала идентична рабочим
//	                        # (масса/температура) — единственное системное
//	                        # отличие этих полей это ПОРЯДОК ВЕЛИЧИНЫ числа
//	                        # (Дж~1e9 против кг~1e3). Проверка: подобрать
//	                        # масштаб, при котором драйвер сам перестаёт
//	                        # считать значение недостоверным.
//
// РАССМОТРЕНО И ОТКЛОНЕНО (2026-08-05): идея переставить '=' на место
// ПОСЛЕ шапки ("tag{header}=value" вместо текущего "tag={header}value") —
// именно так задокументирован формат в registri_mbrrtu_vkm.pdf, отличие
// от того, что реально шлёт эта прошивка прибора. Опровергнуто логически
// до реализации: масса/температура УЖЕ пишутся в БД ЭС достоверными
// (State=0) с ТЕКУЩИМ, недокументированным порядком, в ТОЙ ЖЕ строке, что
// и тепловая энергия/давление (State=1, недостоверно) — если бы порядок
// '=' был причиной, не работали бы все поля разом, а не только часть.
//
// Missing file or missing keys → documented defaults, so the carrier runs
// fine with no config file at all.
const vkmConfigFileName = "vkm_config.txt"

type vkmConfig struct {
	yearMode            string
	clockOffset         time.Duration
	minReady            time.Duration
	archiveString       string
	timeLayout          string
	periodFormat        string
	curByteOrderProbe   bool
	curByteOrderLogPath string
	// stripHeaders/expandExponent — два ключевых преобразования архивной
	// строки перед отдачей в ЭС, вынесены в конфиг, чтобы перебирать их
	// комбинации правкой ТЕКСТОВОГО файла, без пересборки бинарника.
	// История (2026-08-03): вывод «ЭС принимает только формат без шапок»
	// оказался, скорее всего, ошибочным — продвижение ЭС по периодам не
	// означало успешный разбор значений (все каналы остались нулями;
	// «принятие» на 7-й попытке перебора было, вероятно, «сдалась и
	// записала нули», а не «распарсила»). Новая рабочая теория: парсер ЭС
	// опознаёт величины ПО ТЕКСТУ ШАПКИ ({Масса теплонос.} и т.п. — так
	// объясняется, почему с прямого прибора масса/температура доходят), а
	// экспоненциальную запись значений не понимает (так объясняется,
	// почему тепло/давление не доходят ДАЖЕ с прямого прибора).
	// Комбинация «шапки оставить + экспоненту развернуть» не проверялась
	// ни разу — теперь это дефолт.
	stripHeaders   bool
	expandExponent bool
	// fieldScale — множитель для конкретных тегов (ключ = имя тега, как
	// в архивной строке), применяется перед отдачей в ЭС.
	fieldScale map[string]float64
}

func defaultVKMConfig() vkmConfig {
	return vkmConfig{
		yearMode:    "full",
		clockOffset: 0,
		minReady:    0,
		// Empty by default so a caller-supplied string (CLI --vkm-string,
		// or ConfigArchiveSource.Fallback) is used unless the config file
		// explicitly overrides it.
		archiveString:       "",
		periodFormat:        "datetime",
		curByteOrderProbe:   false,
		curByteOrderLogPath: "vkm_byteorder_probe.txt",
		// Комбинация "шапки убраны + экспонента как есть" даёт стабильное
		// продвижение ЭС по периодам (без повторов) — ПОДТВЕРЖДЕНО живьём
		// (2026-08-04). ВАЖНАЯ ОГОВОРКА: продвижение по датам подтверждает
		// только то, что ЭС не отбраковывает саму строку целиком — это
		// НЕ доказывает, что значения полей распознаются и попадают на
		// каналы (проверено отдельно, живым сравнением с БД ЭС: на этой
		// комбинации каналы оставались пустыми). Причина пустых каналов
		// оказалась в другом месте — см. NeedMain/State в БД ЭС, не в
		// формате строки (порядок '=' относительно шапки тоже проверялся
		// и отклонён — см. doc-комментарий выше файла).
		// ПОДТВЕРЖДЕНО прозрачным сетевым прокси (2026-08-10): ЭС ожидает
		// строку БУКВАЛЬНО такой, какой её прислал прибор — без единого
		// изменения. Все преобразования (убрать шапки, развернуть
		// экспоненту, масштабировать поля), которые мы перебирали весь
		// день, были неверным направлением: реальная причина нулей была
		// не в формате northbound, а в том, что southbound сам
		// переспрашивал "секундный" формат Time как будто он брак (см.
		// isVKMTimeAnomalous в device/vkm_hourly.go, retry убран) —
		// именно секундный формат несёт полную точность чисел, которую
		// ЭС и принимает. Дефолт теперь — ничего не трогать.
		stripHeaders:   false,
		expandExponent: false,
	}
}

var (
	vkmCfgMu      sync.Mutex
	vkmCfgPath    = vkmConfigFileName // overridable in tests
	vkmCfgCache   vkmConfig
	vkmCfgLoaded  bool
	vkmCfgModTime time.Time
	vkmCfgChecked time.Time
)

// loadVKMConfig returns the current config, re-reading the file at most once
// per second and only when its modification time changed. Any parse/read
// error falls back to the last good config (or defaults), never crashes the
// carrier — a typo in the file must not take polling down.
func loadVKMConfig() vkmConfig {
	vkmCfgMu.Lock()
	defer vkmCfgMu.Unlock()

	now := time.Now()
	if vkmCfgLoaded && now.Sub(vkmCfgChecked) < time.Second {
		return vkmCfgCache
	}
	vkmCfgChecked = now

	info, err := os.Stat(vkmCfgPath)
	if err != nil {
		if !vkmCfgLoaded {
			vkmCfgCache = defaultVKMConfig()
			vkmCfgLoaded = true
		}
		return vkmCfgCache
	}
	if vkmCfgLoaded && info.ModTime().Equal(vkmCfgModTime) {
		return vkmCfgCache
	}

	data, err := os.ReadFile(vkmCfgPath)
	if err != nil {
		if !vkmCfgLoaded {
			vkmCfgCache = defaultVKMConfig()
			vkmCfgLoaded = true
		}
		return vkmCfgCache
	}

	vkmCfgCache = parseVKMConfig(string(data))
	vkmCfgModTime = info.ModTime()
	vkmCfgLoaded = true
	return vkmCfgCache
}

func parseVKMConfig(text string) vkmConfig {
	// Strip a leading UTF-8 BOM: Windows PowerShell 5.1's
	// `Set-Content -Encoding UTF8` writes one by default, which — left
	// in place — glues onto the FIRST line's key (e.g. "\ufeffyear_mode")
	// and silently fails to match, reverting that one setting to its
	// default. Found live: re-saving vkm_config.txt as UTF-8 to fix
	// Cyrillic encoding in archive_string silently disabled year_mode
	// (first line in the file), which brought back the multi-billion-
	// second clock skew that halts ЭС polling entirely (24.07.2026).
	text = strings.TrimPrefix(text, "\uFEFF")

	cfg := defaultVKMConfig()
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// strip trailing inline comment for numeric/enum keys (but NOT for
		// archive_string, where '#' could be legitimate content)
		switch key {
		case "year_mode":
			if v := stripInlineComment(val); v == "full" || v == "y2000" || v == "y1900" {
				cfg.yearMode = v
			}
		case "clock_offset_sec":
			if n, err := strconv.Atoi(stripInlineComment(val)); err == nil {
				cfg.clockOffset = time.Duration(n) * time.Second
			}
		case "min_ready_ms":
			if n, err := strconv.Atoi(stripInlineComment(val)); err == nil && n >= 0 {
				cfg.minReady = time.Duration(n) * time.Millisecond
			}
		case "archive_string":
			cfg.archiveString = val
		case "period_format":
			// How the period parameter renders when opts bit3 is set:
			// "datetime" (second-precision timestamps) or "unix" (epoch
			// seconds). See vkmPeriodValue.
			if v := stripInlineComment(val); v == "datetime" || v == "unix" {
				cfg.periodFormat = v
			}
		case "time_layout":
			// Go reference-time layout for timestamps inside the archive
			// string, e.g. "02.01.2006 15:04:05" or "02.01.06 15:04".
			if v := strings.TrimSpace(val); v != "" {
				cfg.timeLayout = v
			}
		case "cur_byte_order_probe":
			if v := stripInlineComment(val); v == "true" || v == "1" {
				cfg.curByteOrderProbe = true
			} else if v == "false" || v == "0" {
				cfg.curByteOrderProbe = false
			}
		case "cur_byte_order_probe_log":
			if v := strings.TrimSpace(val); v != "" {
				cfg.curByteOrderLogPath = v
			}
		case "strip_headers":
			if v := stripInlineComment(val); v == "true" || v == "1" {
				cfg.stripHeaders = true
			} else if v == "false" || v == "0" {
				cfg.stripHeaders = false
			}
		case "expand_exponent":
			if v := stripInlineComment(val); v == "true" || v == "1" {
				cfg.expandExponent = true
			} else if v == "false" || v == "0" {
				cfg.expandExponent = false
			}
		case "field_scale":
			v := stripInlineComment(val)
			if v == "" {
				continue
			}
			m := make(map[string]float64)
			for _, pair := range strings.Split(v, ",") {
				pair = strings.TrimSpace(pair)
				if pair == "" {
					continue
				}
				kv := strings.SplitN(pair, ":", 2)
				if len(kv) != 2 {
					continue
				}
				tag := strings.TrimSpace(kv[0])
				mult, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
				if err != nil || tag == "" {
					continue
				}
				m[tag] = mult
			}
			if len(m) > 0 {
				cfg.fieldScale = m
			}
		}
	}
	return cfg
}

func stripInlineComment(v string) string {
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ConfigArchiveSource serves the archive string from the config file,
// re-read on each request. This is the source used in production smoke
// runs so the string can be changed by EDITING A TEXT FILE on the
// Энергосфера server — no rebuild, no re-upload, no restart.
//
// It exists because the archive string's exact format (tags, header
// fields, units, separators) is the one thing we cannot derive without a
// live ВКМ or a vendor spec: the driver reads our string and rejects it,
// and each candidate format previously cost a full build-and-ship cycle
// over a slow link. With this source, trying a format is a text edit.
//
// A CLI-supplied string (--vkm-string) still wins when the config file has
// no archive_string key, so existing invocations keep working.
type ConfigArchiveSource struct {
	Fallback string // used when the config file specifies no archive_string
}

func (c ConfigArchiveSource) Archive(pipe int, start, end time.Time, opts uint16) (string, bool) {
	cfg := loadVKMConfig()

	// A literal archive_string in the config wins outright — an escape
	// hatch for trying a raw hand-crafted string on the live server.
	if cfg.archiveString != "" {
		return cfg.archiveString, true
	}
	if c.Fallback != "" {
		return c.Fallback, true
	}

	// Otherwise build a spec-compliant string (see BuildVKMArchiveString):
	// period first, then the data parameters. The period comes from the
	// window the master requested, so the record is timestamped with the
	// interval it actually asked for.
	if start.IsZero() {
		start = time.Now().Add(-time.Hour).Truncate(time.Hour)
	}
	if end.IsZero() {
		end = start.Add(time.Hour)
	}
	return BuildVKMArchiveString(start, end, cfg.timeLayout, defaultVKMParams(), opts), true
}
