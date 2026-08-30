// energosphere_sync.go pushes the four heat-metering values (heat energy,
// mass, temperature, pressure) from mbgw's own collected ВКМ-360 archive
// (mbgw_vkm.db) directly into Энергосфера's Mains table via extdb.go's
// MainsWriter, bypassing the ЭС device drivers (УВП280 / ТСРВ-024) entirely.
//
// WHY THIS EXISTS: the УВП280 driver in this ЭС rejects heat/pressure
// outright (State=1) regardless of content — proven exhaustively (varying
// value, unit, precision, headers all failed identically, even byte-for-
// byte through a transparent proxy with zero mbgw involvement). A
// subsequent attempt to emulate a ВЗЛЕТ ТСРВ-024 device (which DOES
// deliver heat/pressure successfully for the real, already-working ВОС-2
// point in this same ЭС) hit a full day of undiagnosable driver behavior
// specifically over TCP — every byte-for-byte comparison against the real
// device's own responses matched exactly, yet the ЭС driver never
// progressed to reading the archive on our carrier. RS-485 was the only
// connection type confirmed to work end-to-end (full 217-record backfill
// on a fresh point), which mbgw cannot offer without an actual physical or
// virtual COM port. Direct DB delivery is the one confirmed-working path
// for these two channels; FINAL_TRD's "Интеграция с Энергосферой" clause
// explicitly names "сетевой доступ к БД" as a legitimate delivery method,
// so this is not an architectural workaround, it's one of the sanctioned
// options — see LLD.md's internal/integration/extdb.go entry.
//
// OUTAGE RESILIENCE (the original hard requirement carried over from the
// device-protocol carriers): this runs as its own process, separate from
// the southbound collector. Each pass reads whatever the collector has
// accumulated in mbgw_vkm.db and writes into Mains only the (channel,
// timestamp) points not already present. If the ЭС database was
// unreachable, the next pass catches up from mbgw_vkm.db — a link outage
// never leaves a permanent hole. Idempotent by construction: re-running
// over an overlapping window inserts nothing new (confirmed live,
// 2026-08-21: a full second pass over an already-synced window reported
// "0 inserted, N skipped, 0 errors").
//
// KNOWN GAP vs. the full architecture (FINAL_TRD T10/T15): every point is
// currently written with a hardcoded State=0 ("достоverно"), with no
// quality evaluation (internal/quality's range/stale/sensor-fault checks)
// and no audit trail (internal/config's audit_log). Both are natural next
// steps once internal/quality exists in a form this package can call —
// deliberately NOT bolted on here as a guess at that interface.
package integration

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	sqliterepo "mbgw/internal/storage/sqlite"
)

// Config holds everything the sync needs that must not be baked into the
// binary or committed to git: the SQL Server connection (with the ЭС
// database password) and the per-channel unit conversion factors (tuned
// against the live working mass/temperature channels before heat/pressure
// were trusted — see the factor_* doc below).
//
// Read from a plain text file next to the exe (default es_sync.txt), same
// "key = value" convention the northbound carriers' *_config.txt files
// use. The password lives only in this file on the server.
type Config struct {
	SQLServer   string
	SQLDatabase string
	SQLUser     string
	SQLPassword string
	SQLPort     int

	DeviceID string
	Pipe     int

	// Channel IDs in ЭС Mains table (ID_Channel), confirmed against the
	// live ЭС channel editor (2026-08-18) for point TEST_VKM7.
	ChanHeat     int
	ChanMass     int
	ChanTemp     int
	ChanPressure int

	// value_es = value_raw * factor. Confirmed live (2026-08-21) against
	// the meter's own printed report: heat and temperature match the
	// report to ~4-5 significant figures with factor=1.0 (raw device
	// units — Joules, °C — pass straight through); mass and pressure are
	// plausible in the same raw-unit reading. All four currently ship as
	// 1.0; kept per-channel and file-configurable in case a future meter
	// or channel mapping needs a real conversion.
	FactorHeat     float64
	FactorMass     float64
	FactorTemp     float64
	FactorPressure float64

	IntervalSec   int
	BackfillHours int
	DryRun        bool

	// TimeShiftMinutes — сдвиг метки времени (в минутах), применяемый к
	// MeasureDate ПЕРЕД записью в Mains. Подробное объяснение, зачем это
	// нужно и почему значение настраиваемое, а не захардкоженное — см.
	// ESConnection.TimeShiftMinutes в internal/storage/sqlite/
	// repo_device_config.go. 0 = без сдвига (поведение по умолчанию, как
	// было до появления этой настройки).
	TimeShiftMinutes int
}

func defaultConfig() Config {
	return Config{
		SQLServer:      "localhost",
		SQLDatabase:    "CSD_Astrakhan",
		SQLUser:        "AdminBaz",
		SQLPort:        1433,
		DeviceID:       "vkm360_real",
		Pipe:           1,
		ChanHeat:       230001, // Тепловая энергия (B9)
		ChanMass:       229994, // Масса на подающем (B2)
		ChanTemp:       230008, // Температура на подающем (G5)
		ChanPressure:   230004, // "Барометрическое давление" (G1) — по факту несёт избыточное давление теплоносителя; канала с точным названием под избыточное давление в этой ЭС нет, решено оставить как есть (2026-08-21) — см. package doc.
		FactorHeat:     1.0,
		FactorMass:     1.0,
		FactorTemp:     1.0,
		FactorPressure: 1.0,
		IntervalSec:    60,
		BackfillHours:  168,
		DryRun:         false,
	}
}

// LoadConfig reads the config file. A missing file is a hard error — this
// command cannot run without at least the SQL password, so it fails loudly
// rather than silently using a blank password.
func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("не открыть %s (нужен файл с параметрами подключения к БД ЭС): %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "\uFEFF")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		switch key {
		case "sql_server":
			if val == "(local)" || val == "." {
				val = "localhost" // SSMS-ism the mssql driver doesn't understand
			}
			cfg.SQLServer = val
		case "sql_database":
			cfg.SQLDatabase = val
		case "sql_user":
			cfg.SQLUser = val
		case "sql_password":
			cfg.SQLPassword = val
		case "sql_port":
			cfg.SQLPort = atoiOr(val, cfg.SQLPort)
		case "device_id":
			cfg.DeviceID = val
		case "pipe":
			cfg.Pipe = atoiOr(val, cfg.Pipe)
		case "chan_heat":
			cfg.ChanHeat = atoiOr(val, cfg.ChanHeat)
		case "chan_mass":
			cfg.ChanMass = atoiOr(val, cfg.ChanMass)
		case "chan_temp":
			cfg.ChanTemp = atoiOr(val, cfg.ChanTemp)
		case "chan_pressure":
			cfg.ChanPressure = atoiOr(val, cfg.ChanPressure)
		case "factor_heat":
			cfg.FactorHeat = floatOr(val, cfg.FactorHeat)
		case "factor_mass":
			cfg.FactorMass = floatOr(val, cfg.FactorMass)
		case "factor_temp":
			cfg.FactorTemp = floatOr(val, cfg.FactorTemp)
		case "factor_pressure":
			cfg.FactorPressure = floatOr(val, cfg.FactorPressure)
		case "interval_sec":
			cfg.IntervalSec = atoiOr(val, cfg.IntervalSec)
		case "backfill_hours":
			cfg.BackfillHours = atoiOr(val, cfg.BackfillHours)
		case "time_shift_minutes":
			cfg.TimeShiftMinutes = atoiOr(val, cfg.TimeShiftMinutes)
		case "dry_run":
			cfg.DryRun = val == "1" || strings.EqualFold(val, "true") || strings.EqualFold(val, "yes")
		}
	}
	if err := sc.Err(); err != nil {
		return Config{}, fmt.Errorf("чтение %s: %w", path, err)
	}
	if cfg.SQLPassword == "" {
		return Config{}, fmt.Errorf("в %s не задан sql_password — без него подключение к БД ЭС невозможно", path)
	}
	return cfg, nil
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

func floatOr(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return v
	}
	return def
}

func (c Config) interval() time.Duration {
	if c.IntervalSec > 0 {
		return time.Duration(c.IntervalSec) * time.Second
	}
	return 60 * time.Second
}

// tagFactor pairs a ВКМ archive tag with the ЭС channel + multiplier it
// feeds. The four are processed identically; only tag/channel/factor/label
// differ.
type tagFactor struct {
	tag     string // ВКМ archive tag: "ST","S","T","Pi"
	channel int
	factor  float64
	label   string
}

// RunEnergosphereSync opens both databases and loops every interval,
// syncing the backfill window, until ctx is cancelled.
// trigger, если не nil, позволяет вызывающему коду попросить сделать
// ВНЕПЛАНОВЫЙ проход прямо сейчас, не дожидаясь обычного часового тикера
// — используется кнопкой «Синхронизировать сейчас» и «Принудительным
// переопросом» (чтобы данные появлялись в ЭС по ходу сбора, а не только
// после полного завершения долгой операции — добавлено 2026-08-27).
// Переиспользует уже открытые repo/writer того же цикла — не открывает
// новое подключение к БД ЭС на каждый вызов. Буферизованный (размер 1) —
// несколько быстрых подряд запросов сливаются в один внеплановый проход,
// не накапливаются в очередь.
func RunEnergosphereSync(ctx context.Context, sqlitePath string, cfg Config, trigger <-chan struct{}) error {
	repo, err := sqliterepo.New(sqlitePath)
	if err != nil {
		return fmt.Errorf("open mbgw_vkm.db (%s): %w", sqlitePath, err)
	}
	defer repo.Close()

	writer, err := OpenMainsWriter(SQLServerConfig{
		Server:   cfg.SQLServer,
		Database: cfg.SQLDatabase,
		User:     cfg.SQLUser,
		Password: cfg.SQLPassword,
		Port:     cfg.SQLPort,
	})
	if err != nil {
		return fmt.Errorf("open ЭС SQL Server: %w", err)
	}
	defer writer.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = writer.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("подключение к БД ЭС не удалось (проверь es_sync.txt: сервер/база/логин/пароль): %w", err)
	}
	log.Printf("[es-sync] подключение к БД ЭС OK: сервер=%s база=%s логин=%s\n", cfg.SQLServer, cfg.SQLDatabase, cfg.SQLUser)

	if n, oldest, newest, found, err := repo.CountVKMRaw(ctx, cfg.DeviceID, cfg.Pipe); err != nil {
		log.Printf("[es-sync] предупреждение: не смог опросить исходную БД: %v\n", err)
	} else if !found {
		log.Printf("[es-sync] ВНИМАНИЕ: в mbgw_vkm.db нет ни одной записи для device=%s pipe=%d — southbound (run) собрал данные?\n", cfg.DeviceID, cfg.Pipe)
	} else {
		log.Printf("[es-sync] исходная БД: %d записей ВКМ, период с %s по %s\n",
			n, oldest.Format("02.01.2006 15:04"), newest.Format("02.01.2006 15:04"))
	}

	targets := []tagFactor{
		{"ST", cfg.ChanHeat, cfg.FactorHeat, "тепло"},
		{"S", cfg.ChanMass, cfg.FactorMass, "масса"},
		{"T", cfg.ChanTemp, cfg.FactorTemp, "температура"},
		{"Pi", cfg.ChanPressure, cfg.FactorPressure, "давление"},
	}

	if cfg.DryRun {
		log.Println("[es-sync] РЕЖИМ DRY-RUN: в БД ЭС ничего не пишется, только лог того, что было бы записано.")
	}

	runEnergosphereSyncOnce(ctx, repo, writer, cfg, targets)
	ticker := time.NewTicker(cfg.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[es-sync] остановлен")
			return nil
		case <-ticker.C:
			runEnergosphereSyncOnce(ctx, repo, writer, cfg, targets)
		case <-trigger:
			log.Println("[es-sync] внеплановая синхронизация по запросу")
			runEnergosphereSyncOnce(ctx, repo, writer, cfg, targets)
		}
	}
}

func runEnergosphereSyncOnce(ctx context.Context, repo *sqliterepo.Repo, writer *MainsWriter, cfg Config, targets []tagFactor) {
	now := time.Now()
	from := now.Add(-time.Duration(cfg.BackfillHours) * time.Hour)

	rows, err := repo.GetVKMRawStringsRange(ctx, cfg.DeviceID, cfg.Pipe, from, now)
	if err != nil {
		log.Printf("[es-sync] чтение исходной БД: %v\n", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	var inserted, skipped, failed int
	for _, row := range rows {
		// Сдвиг метки времени применяется ОДИН раз здесь, до всех
		// дальнейших действий — так он гарантированно одинаков и в
		// проверке существования точки, и в самой записи, и в строках
		// лога (иначе легко получить рассинхрон: проверяем одно время,
		// пишем другое). Зачем этот сдвиг вообще нужен — см.
		// Config.TimeShiftMinutes.
		esTime := row.TsHour.Add(time.Duration(cfg.TimeShiftMinutes) * time.Minute)

		for _, t := range targets {
			rawVal, ok := parseVKMTagFloat(row.RawString, t.tag)
			if !ok {
				continue
			}
			value := rawVal * t.factor

			present, err := writer.PointExists(ctx, t.channel, esTime)
			if err != nil {
				log.Printf("[es-sync] проверка наличия точки (%s ch=%d %s): %v\n",
					t.label, t.channel, esTime.Format("02.01 15:04"), err)
				failed++
				continue
			}
			if present {
				skipped++
				continue
			}

			if cfg.DryRun {
				log.Printf("[es-sync] DRY-RUN записал бы: %s ch=%d %s value=%g\n",
					t.label, t.channel, esTime.Format("02.01.2006 15:04"), value)
				inserted++
				continue
			}

			// state=0 ("достоверно") hardcoded — see package doc's "KNOWN GAP".
			if err := writer.InsertPoint(ctx, t.channel, esTime, value, 0); err != nil {
				if IsDuplicateKeyError(err) {
					skipped++
					continue
				}
				log.Printf("[es-sync] запись (%s ch=%d %s): %v\n",
					t.label, t.channel, esTime.Format("02.01 15:04"), err)
				failed++
				continue
			}
			inserted++
		}
	}
	if inserted > 0 || failed > 0 {
		log.Printf("[es-sync] проход завершён: записано %d, пропущено (уже есть) %d, ошибок %d, окно %s..%s\n",
			inserted, skipped, failed,
			from.Format("02.01 15:04"), now.Format("02.01 15:04"))
	}
}

// ForceResyncRange принудительно ПЕРЕЗАПИСЫВАЕТ (не пропускает уже
// существующие) точки Mains за указанный диапазон [from, to] для
// заданного прибора — отдельное, явно запрошенное оператором действие
// (добавлено 2026-08-30, прямой запрос оператора: "бывает что с прибора
// попали искажённые данные и нужно переопросить прибор и чтобы новые
// данные попали в эс"). НЕ часть обычного автоматического прохода —
// runEnergosphereSyncOnce выше по-прежнему только вставляет новое и
// пропускает существующее, это сознательный выбор, чтобы не создавать
// лишнюю нагрузку на БД ЭС проверкой/перезаписью каждой точки при
// каждом обычном часовом цикле.
//
// Типичные поводы: (а) с прибора один раз пришли искажённые данные,
// потом сделали «Принудительный переопрос» — свежие верные значения
// появились в НАШЕЙ локальной базе, но в ЭС остались старые, раз там
// уже что-то есть под той же меткой времени и обычная синхронизация их
// не трогает; (б) поменяли множитель канала (см. «Каналы ЭС» в
// api_admin_ui.go) — новые точки сами пойдут в правильных единицах, а
// уже отправленная история так и останется в старых, пока её явно не
// перезаписать.
//
// Для каждой точки диапазона: пробуем UpdatePoint (перезаписать, если
// уже есть); если затронуто 0 строк — точки ещё не было, вставляем
// обычным InsertPoint.
func ForceResyncRange(ctx context.Context, repo *sqliterepo.Repo, writer *MainsWriter, cfg Config, from, to time.Time) (updated, inserted, failed int, err error) {
	rows, err := repo.GetVKMRawStringsRange(ctx, cfg.DeviceID, cfg.Pipe, from, to)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("чтение исходной БД: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, 0, nil
	}

	targets := []tagFactor{
		{"ST", cfg.ChanHeat, cfg.FactorHeat, "тепло"},
		{"S", cfg.ChanMass, cfg.FactorMass, "масса"},
		{"T", cfg.ChanTemp, cfg.FactorTemp, "температура"},
		{"Pi", cfg.ChanPressure, cfg.FactorPressure, "давление"},
	}

	for _, row := range rows {
		// Тот же сдвиг метки времени, что и в обычном проходе выше —
		// см. runEnergosphereSyncOnce, тот же принцип: применяется один
		// раз, до всех дальнейших действий.
		esTime := row.TsHour.Add(time.Duration(cfg.TimeShiftMinutes) * time.Minute)

		for _, t := range targets {
			rawVal, ok := parseVKMTagFloat(row.RawString, t.tag)
			if !ok {
				continue
			}
			value := rawVal * t.factor

			affected, uerr := writer.UpdatePoint(ctx, t.channel, esTime, value, 0)
			if uerr != nil {
				log.Printf("[es-sync] принудительная перезапись (%s ch=%d %s): %v\n",
					t.label, t.channel, esTime.Format("02.01 15:04"), uerr)
				failed++
				continue
			}
			if affected > 0 {
				updated++
				continue
			}

			// Точки ещё не было вообще — вставляем как обычно.
			if ierr := writer.InsertPoint(ctx, t.channel, esTime, value, 0); ierr != nil {
				if IsDuplicateKeyError(ierr) {
					// Гонка: кто-то вставил её между UpdatePoint и
					// InsertPoint — не беда, пробуем перезаписать ещё раз.
					if _, uerr2 := writer.UpdatePoint(ctx, t.channel, esTime, value, 0); uerr2 == nil {
						updated++
						continue
					}
				}
				log.Printf("[es-sync] принудительная запись (%s ch=%d %s): %v\n",
					t.label, t.channel, esTime.Format("02.01 15:04"), ierr)
				failed++
				continue
			}
			inserted++
		}
	}

	log.Printf("[es-sync] принудительная пересинхронизация завершена: переписано %d, вставлено новых %d, ошибок %d, окно %s..%s\n",
		updated, inserted, failed, from.Format("02.01 15:04"), to.Format("02.01 15:04"))
	return updated, inserted, failed, nil
}

// parseVKMTagFloat extracts one tag's numeric value from a raw ВКМ archive
// string ("tag{header}=value unit;…"). Same convention the northbound
// carriers' parsers use; duplicated here (tiny, self-contained) so
// integration has no dependency on the northbound package.
func parseVKMTagFloat(raw, tag string) (float64, bool) {
	for _, entry := range strings.Split(raw, ";") {
		eq := strings.Index(entry, "=")
		if eq < 0 {
			continue
		}
		if entry[:eq] != tag {
			continue
		}
		rest := entry[eq+1:]
		headerLen := 0
		if len(rest) > 0 && (rest[0] == '{' || rest[0] == '<') {
			closeCh := byte('}')
			if rest[0] == '<' {
				closeCh = '>'
			}
			if idx := strings.IndexByte(rest, closeCh); idx >= 0 {
				headerLen = idx + 1
			}
		}
		valuePart := strings.TrimSpace(rest[headerLen:])
		numEnd := 0
		for numEnd < len(valuePart) {
			c := valuePart[numEnd]
			if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
				numEnd++
			} else {
				break
			}
		}
		if numEnd == 0 {
			return 0, false
		}
		f, err := strconv.ParseFloat(valuePart[:numEnd], 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
