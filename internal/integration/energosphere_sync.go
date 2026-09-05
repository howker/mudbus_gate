// energosphere_sync.go pushes collected meter values (ВКМ-360: heat
// energy, mass, temperature, pressure; Akron: volume) from mbgw's own
// collected archive directly into Энергосфера's PointMains table via
// extdb.go's PointMainsWriter, bypassing the ЭС device drivers entirely.
//
// WHY THIS EXISTS (историческая причина для ВКМ, где всё и началось): the
// УВП280 driver in this ЭС rejects heat/pressure outright (State=1)
// regardless of content — proven exhaustively (varying value, unit,
// precision, headers all failed identically, even byte-for-byte through a
// transparent proxy with zero mbgw involvement). A subsequent attempt to
// emulate a ВЗЛЕТ ТСРВ-024 device (which DOES deliver heat/pressure
// successfully for the real, already-working ВОС-2 point in this same ЭС)
// hit a full day of undiagnosable driver behavior specifically over TCP —
// every byte-for-byte comparison against the real device's own responses
// matched exactly, yet the ЭС driver never progressed to reading the
// archive on our carrier. RS-485 was the only connection type confirmed
// to work end-to-end (full 217-record backfill on a fresh point), which
// mbgw cannot offer without an actual physical or virtual COM port.
// Direct DB delivery is the one confirmed-working path for these two
// channels; FINAL_TRD's "Интеграция с Энергосферой" clause explicitly
// names "сетевой доступ к БД" as a legitimate delivery method, so this is
// not an architectural workaround, it's one of the sanctioned options —
// see LLD.md's internal/integration/extdb.go entry.
//
// ОБЪЕДИНЕНО (2026-08-31, прямой запрос оператора: "переделать опрос
// акрона... сделать также как вкм"): раньше Akron получал данные в ЭС
// СОВСЕМ ДРУГИМ путём — эмуляцией физического прибора для родного
// драйвера ЭС (см. internal/northbound/akron_live.go, теперь
// закомментирован в cmd/mbgw/server.go). Теперь оба типа приборов идут
// через ЭТОТ ЖЕ механизм — единственная разница в том, ОТКУДА берутся
// исходные данные (см. collectVKMReadings/collectAkronReadings ниже),
// дальше весь путь (проверка/запись в PointMains, повтор по таймеру,
// внеплановый запуск, принудительная пересинхронизация) общий.
//
// OUTAGE RESILIENCE (the original hard requirement carried over from the
// device-protocol carriers): this runs as its own goroutine, separate
// from the southbound collector. Each pass reads whatever the collector
// has accumulated locally and writes into PointMains only the (point,
// timestamp) rows not already present. If the ЭС database was
// unreachable, the next pass catches up from the local database — a link
// outage never leaves a permanent hole. Idempotent by construction:
// re-running over an overlapping window inserts nothing new (confirmed
// live, 2026-08-21: a full second pass over an already-synced window
// reported "0 inserted, N skipped, 0 errors").
//
// KNOWN GAP vs. the full architecture (FINAL_TRD T10/T15): every point is
// currently written with a hardcoded State=0 ("достоверно"), with no
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
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"mbgw/internal/health"
	"mbgw/internal/interval"
	sqliterepo "mbgw/internal/storage/sqlite"
)

// PointMapping — одна точка ЭС (ID_PP в PointMains), в которую пишем
// один параметр прибора с заданным множителем. У ВКМ таких точек обычно
// 4 на прибор (масса/тепло/температура/давление, теги "S"/"ST"/"T"/"Pi"),
// у Akron — одна (объём, тег "V").
type PointMapping struct {
	Tag     string // тег параметра прибора — см. collectVKMReadings/collectAkronReadings
	PointID int    // ID_PP в PointMains
	Factor  float64
	Label   string // человекочитаемое название для лога

	// Optional safety range applied to the FINAL value, after Factor and
	// all counter/interval conversion. nil means that side is disabled.
	MinValue *float64
	MaxValue *float64
}

// Config holds everything the sync needs that must not be baked into the
// binary or committed to git: the SQL Server connection (with the ЭС
// database password) and the точки+множители, куда писать.
//
// ИЗМЕНЕНО (2026-08-31): раньше здесь было 4 отдельных захардкоженных
// поля (ChanHeat/ChanMass/ChanTemp/ChanPressure) плюс 4 отдельных
// Factor* — работало только для ВКМ с ровно этими четырьмя величинами.
// Теперь один список Points любой длины — подходит и для ВКМ (4 записи),
// и для Akron (1 запись), и вообще для любого будущего типа прибора с
// любым числом величин, без изменения структуры Config.
//
// Read from a plain text file next to the exe (default es_sync.txt, для
// СТАРОГО отдельного режима запуска `mbgw es-sync` — см. main.go) ИЛИ
// собирается в памяти из БД (см. buildIntegrationConfig в cmd/mbgw/
// server.go — это то, что реально используется в режиме `mbgw server`,
// единственном режиме, применяемом в проде сегодня).
type Config struct {
	SQLServer   string
	SQLDatabase string
	SQLUser     string
	SQLPassword string
	SQLPort     int

	DeviceID string
	Pipe     int    // используется только при Kind="vkm360" (номер трубопровода в архиве); Akron это поле игнорирует
	Kind     string // "vkm360" | "akron" — какой источник исходных данных использовать, см. collectVKMReadings/collectAkronReadings

	Points []PointMapping

	IntervalSec   int
	BackfillHours int
	DryRun        bool

	// TimeShiftMinutes — общий сдвиг метки времени перед записью в
	// PointMains. Применяется и к ВКМ, и к Akron.
	//
	// 0 = без сдвига. Отрицательное значение сдвигает метку назад.
	// Для текущей ЭС живьём подтверждена необходимость коррекции -90 минут
	// как для ВКМ, так и для Akron.
	TimeShiftMinutes int
}

// pointMappingByTag ищет запись в Points по тегу — используется LoadConfig
// (старый текстовый режим) для точечного обновления конкретной точки/
// множителя, не трогая остальные.
func (c *Config) pointMappingByTag(tag string) *PointMapping {
	for i := range c.Points {
		if c.Points[i].Tag == tag {
			return &c.Points[i]
		}
	}
	return nil
}

func defaultConfig() Config {
	return Config{
		SQLServer:   "localhost",
		SQLDatabase: "CSD_Astrakhan",
		SQLUser:     "AdminBaz",
		SQLPort:     1433,
		DeviceID:    "vkm360_real",
		Pipe:        1,
		Kind:        "vkm360",
		Points: []PointMapping{
			// Confirmed live (2026-08-21) against the meter's own printed
			// report: heat and temperature match the report to ~4-5
			// significant figures with factor=1.0 (raw device units —
			// Joules, °C — pass straight through); mass and pressure are
			// plausible in the same raw-unit reading.
			{Tag: "ST", PointID: 230001, Factor: 1.0, Label: "тепло"},      // Тепловая энергия (B9)
			{Tag: "S", PointID: 229994, Factor: 1.0, Label: "масса"},       // Масса на подающем (B2)
			{Tag: "T", PointID: 230008, Factor: 1.0, Label: "температура"}, // Температура на подающем (G5)
			{Tag: "Pi", PointID: 230004, Factor: 1.0, Label: "давление"},   // "Барометрическое давление" (G1) — по факту несёт избыточное давление теплоносителя
		},
		IntervalSec:   60,
		BackfillHours: 168,
		DryRun:        false,
	}
}

// LoadConfig reads the config file. A missing file is a hard error — this
// command cannot run without at least the SQL password, so it fails loudly
// rather than silently using a blank password.
//
// Это СТАРЫЙ, отдельный режим запуска (`mbgw es-sync`) — в проде сегодня
// не используется, применяется режим `mbgw server` (см. buildIntegrationConfig
// в cmd/mbgw/server.go, собирает Config из БД, а не из текстового файла).
// Оставлено рабочим ради обратной совместимости, а не как основной путь.
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
		case "kind":
			cfg.Kind = val
		// Ключи файла (chan_*/factor_*) намеренно оставлены как были —
		// это формат СТАРОГО текстового файла es_sync.txt, менять его
		// ради внутреннего переименования Config не было причины.
		case "chan_heat":
			if m := cfg.pointMappingByTag("ST"); m != nil {
				m.PointID = atoiOr(val, m.PointID)
			}
		case "chan_mass":
			if m := cfg.pointMappingByTag("S"); m != nil {
				m.PointID = atoiOr(val, m.PointID)
			}
		case "chan_temp":
			if m := cfg.pointMappingByTag("T"); m != nil {
				m.PointID = atoiOr(val, m.PointID)
			}
		case "chan_pressure":
			if m := cfg.pointMappingByTag("Pi"); m != nil {
				m.PointID = atoiOr(val, m.PointID)
			}
		case "factor_heat":
			if m := cfg.pointMappingByTag("ST"); m != nil {
				m.Factor = floatOr(val, m.Factor)
			}
		case "factor_mass":
			if m := cfg.pointMappingByTag("S"); m != nil {
				m.Factor = floatOr(val, m.Factor)
			}
		case "factor_temp":
			if m := cfg.pointMappingByTag("T"); m != nil {
				m.Factor = floatOr(val, m.Factor)
			}
		case "factor_pressure":
			if m := cfg.pointMappingByTag("Pi"); m != nil {
				m.Factor = floatOr(val, m.Factor)
			}
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
//
// Работает ОДИНАКОВО для обоих типов приборов — cfg.Kind определяет,
// как читать исходные данные (см. runPointSyncOnce ниже), весь
// остальной путь (подключение, таймер, повтор, лог) общий.
func RunEnergosphereSync(ctx context.Context, sqlitePath string, cfg Config, trigger <-chan struct{}) error {
	repo, err := sqliterepo.New(sqlitePath)
	if err != nil {
		return fmt.Errorf("open local db (%s): %w", sqlitePath, err)
	}
	defer repo.Close()

	if err := repo.InitESSyncCursorSchema(ctx); err != nil {
		return fmt.Errorf("init ES sync cursor: %w", err)
	}

	writer, err := OpenPointMainsWriter(SQLServerConfig{
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

	// БД ЭС может быть недоступна в момент старта mbgw. Это не должно
	// навсегда убивать worker синхронизации: локальный сбор продолжает
	// работать, а здесь ждём восстановления ЭС с ограниченным backoff.
	retryDelay := 5 * time.Second
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = writer.Ping(pingCtx)
		cancel()

		if err == nil {
			log.Printf("[es-sync] подключение к БД ЭС OK: сервер=%s база=%s логин=%s\n", cfg.SQLServer, cfg.SQLDatabase, cfg.SQLUser)
			break
		}

		if ctx.Err() != nil {
			log.Println("[es-sync] остановлен до восстановления подключения к БД ЭС")
			return nil
		}

		log.Printf("[es-sync] БД ЭС недоступна: %v; повтор через %s\n", err, retryDelay)

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			log.Println("[es-sync] остановлен до восстановления подключения к БД ЭС")
			return nil
		case <-timer.C:
		}

		if retryDelay < 30*time.Second {
			retryDelay *= 2
			if retryDelay > 30*time.Second {
				retryDelay = 30 * time.Second
			}
		}
	}

	if cfg.Kind == "vkm360" {
		if n, oldest, newest, found, err := repo.CountVKMRaw(ctx, cfg.DeviceID, cfg.Pipe); err != nil {
			log.Printf("[es-sync] предупреждение: не смог опросить исходную БД: %v\n", err)
		} else if !found {
			log.Printf("[es-sync] ВНИМАНИЕ: в локальной БД нет ни одной записи для device=%s pipe=%d — southbound собрал данные?\n", cfg.DeviceID, cfg.Pipe)
		} else {
			log.Printf("[es-sync] исходная БД: %d записей ВКМ, период с %s по %s\n",
				n, oldest.Format("02.01.2006 15:04"), newest.Format("02.01.2006 15:04"))
		}
	}

	if cfg.DryRun {
		log.Println("[es-sync] РЕЖИМ DRY-RUN: в БД ЭС ничего не пишется, только лог того, что было бы записано.")
	}

	runPointSyncOnce(ctx, repo, writer, cfg)
	ticker := time.NewTicker(cfg.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[es-sync] остановлен")
			return nil
		case <-ticker.C:
			runPointSyncOnce(ctx, repo, writer, cfg)
		case <-trigger:
			log.Println("[es-sync] внеплановая синхронизация по запросу")
			runPointSyncOnce(ctx, repo, writer, cfg)
		}
	}
}

// pointReading — одна кандидатная точка на запись: какому PointMapping
// она соответствует, на какой момент времени и с каким значением.
// Промежуточное представление между "прочитали из своей локальной базы"
// (collectVKMReadings/collectAkronReadings) и "записали/проверили в
// PointMains" (writeReading) — единое для обоих источников данных.
type pointReading struct {
	mapping PointMapping
	ts      time.Time
	value   float64
}

// validatePointReading is the final safety barrier immediately before
// writing a calculated value to ЭС. Ranges are optional/configurable;
// NaN/Inf are always rejected because SQL/financial data must never
// receive a non-finite measurement.
func validatePointReading(r pointReading) error {
	if math.IsNaN(r.value) || math.IsInf(r.value, 0) {
		return fmt.Errorf("неконечное значение %g", r.value)
	}
	if r.mapping.MinValue != nil && r.value < *r.mapping.MinValue {
		return fmt.Errorf("значение %g ниже минимума %g", r.value, *r.mapping.MinValue)
	}
	if r.mapping.MaxValue != nil && r.value > *r.mapping.MaxValue {
		return fmt.Errorf("значение %g выше максимума %g", r.value, *r.mapping.MaxValue)
	}
	return nil
}

// collectVKMReadings читает диапазон сырых строк архива ВКМ и извлекает
// из каждой все теги, перечисленные в cfg.Points — тот же путь, что был
// и раньше, просто вынесен в отдельную функцию, чтобы runPointSyncOnce
// мог одинаково работать что с этим источником, что с Akron'овским.
func collectVKMReadings(ctx context.Context, repo *sqliterepo.Repo, cfg Config, from, now time.Time) ([]pointReading, error) {
	rows, err := repo.GetVKMRawStringsRange(ctx, cfg.DeviceID, cfg.Pipe, from, now)
	if err != nil {
		return nil, err
	}
	var out []pointReading
	for _, row := range rows {
		// Сдвиг метки времени применяется ОДИН раз здесь, до всех
		// дальнейших действий — так он гарантированно одинаков и в
		// проверке существования точки, и в самой записи, и в строках
		// лога (иначе легко получить рассинхрон: проверяем одно время,
		// пишем другое). Зачем этот сдвиг вообще нужен — см.
		// Config.TimeShiftMinutes.
		esTime := row.TsHour.Add(time.Duration(cfg.TimeShiftMinutes) * time.Minute)
		for _, m := range cfg.Points {
			rawVal, ok := parseVKMTagFloat(row.RawString, m.Tag)
			if !ok {
				continue
			}
			out = append(out, pointReading{mapping: m, ts: esTime, value: rawVal * m.Factor})
		}
	}
	return out, nil
}

// collectAkronReadings читает часовые снимки Akron из archive_hourly.
//
// Для тега V archive_hourly хранит накопительный счётчик, поэтому расход
// за час вычисляется как разность двух соседних часовых снимков:
//
//	V(17:00) - V(16:00) = расход за интервал 16:00..17:00.
//
// Энергосфера хранит объём в получасовых интервалах. Поэтому полученный
// часовой расход делится поровну между двумя получасовками:
//
//	100 м3 за 16:00..17:00 -> 50 м3 на 16:30 и 50 м3 на 17:00.
//
// Метка означает КОНЕЦ интервала. После формирования получасовых меток
// к каждой из них применяется cfg.TimeShiftMinutes.
//
// Если между соседними снимками не ровно один час или накопительный
// счётчик уменьшился, такой час пропускается: нельзя приписывать расход
// нескольких часов одному интервалу или отправлять отрицательный объём.
func collectAkronReadings(ctx context.Context, repo *sqliterepo.Repo, cfg Config, from, now time.Time) ([]pointReading, error) {
	var out []pointReading
	shift := time.Duration(cfg.TimeShiftMinutes) * time.Minute

	for _, m := range cfg.Points {
		queryFrom := from
		if m.Tag == "V" {
			// Для расчёта первой дельты нужна предыдущая часовая точка.
			queryFrom = from.Add(-time.Hour)
		}

		rows, err := repo.GetHourlyArchiveRange(ctx, cfg.DeviceID, "", m.Tag, queryFrom, now)
		if err != nil {
			return nil, err
		}

		// Защитная ветка на случай появления у Akron параметров,
		// которые уже являются готовыми интервальными значениями.
		if m.Tag != "V" {
			for _, row := range rows {
				if row.TsHour.Before(from) {
					continue
				}

				out = append(out, pointReading{
					mapping: m,
					ts:      row.TsHour.Add(shift),
					value:   row.Value * m.Factor,
				})
			}
			continue
		}

		var prevValue float64
		var prevTS time.Time
		havePrev := false

		for _, row := range rows {
			if havePrev && !row.TsHour.Before(from) {
				delta, ok := interval.CounterDelta(
					prevTS, prevValue,
					row.TsHour, row.Value,
					time.Hour,
				)
				if ok {
					half := delta * m.Factor / 2

					// row.TsHour — конец часового интервала.
					// Создаём две получасовые точки: HH:30 и следующий HH:00.
					firstHalfEnd := row.TsHour.Add(-30 * time.Minute).Add(shift)
					secondHalfEnd := row.TsHour.Add(shift)

					out = append(out,
						pointReading{
							mapping: m,
							ts:      firstHalfEnd,
							value:   half,
						},
						pointReading{
							mapping: m,
							ts:      secondHalfEnd,
							value:   half,
						},
					)
				} else if row.TsHour.Sub(prevTS) != time.Hour {
					log.Printf(
						"[es-sync] Akron %s: пропуск %s — нет соседнего часового снимка перед ним (предыдущий %s)\n",
						cfg.DeviceID,
						row.TsHour.Format("02.01.2006 15:04"),
						prevTS.Format("02.01.2006 15:04"),
					)
				} else {
					log.Printf(
						"[es-sync] Akron %s: пропуск %s — накопительный V уменьшился: %g -> %g\n",
						cfg.DeviceID,
						row.TsHour.Format("02.01.2006 15:04"),
						prevValue,
						row.Value,
					)
				}
			}

			prevValue = row.Value
			prevTS = row.TsHour
			havePrev = true
		}
	}

	return out, nil
}

// collectReadings выбирает источник исходных данных по cfg.Kind.
// Неизвестный/пустой Kind трактуется как "vkm360" — тот же путь, что
// был единственным до появления Akron здесь же (без этого старые
// вызовы, ещё не проставляющие Kind явно, сломались бы молча).
func collectReadings(ctx context.Context, repo *sqliterepo.Repo, cfg Config, from, now time.Time) ([]pointReading, error) {
	switch cfg.Kind {
	case "akron":
		return collectAkronReadings(ctx, repo, cfg, from, now)
	default:
		return collectVKMReadings(ctx, repo, cfg, from, now)
	}
}

// collectReadingsForESRange принимает диапазон в той же временной шкале,
// которую оператор видит в Энергосфере. collectReadings, напротив, читает
// локальный архив до применения TimeShiftMinutes. Поэтому границы сначала
// переводятся обратно во время локального архива, а готовые точки затем
// дополнительно фильтруются по исходному диапазону ЭС.
//
// Это особенно важно для Akron V: одна часовая дельта превращается в две
// получасовые точки. Без обратного пересчёта верхней границы последняя
// получасовка диапазона могла не попасть в ForceResync при ненулевом сдвиге.
func collectReadingsForESRange(ctx context.Context, repo *sqliterepo.Repo, cfg Config, from, to time.Time) ([]pointReading, error) {
	shift := time.Duration(cfg.TimeShiftMinutes) * time.Minute
	sourceFrom := from.Add(-shift)
	sourceTo := to.Add(-shift)

	readings, err := collectReadings(ctx, repo, cfg, sourceFrom, sourceTo)
	if err != nil {
		return nil, err
	}

	out := readings[:0]
	for _, r := range readings {
		if r.ts.Before(from) || r.ts.After(to) {
			continue
		}
		out = append(out, r)
	}

	return out, nil
}

// runPointSyncOnce — обычный автоматический проход синхронизации.
//
// Первый проход для точки, у которой ещё нет cursor, смотрит назад на
// cfg.BackfillHours. После подтверждённой записи/наличия точки в ЭС
// сохраняется cursor по (device, ID_PP), и следующие проходы читают
// локальный архив только от самого раннего подтверждённого cursor.
//
// ВАЖНО: cursor двигается только после подтверждения конкретной точки:
// PointExists=true, успешный InsertPoint или duplicate-key (то есть точка
// уже успела появиться). Если на одной точке возникла ошибка, дальнейшие
// более новые значения ЭТОЙ ЖЕ точки в текущем проходе не двигают cursor
// через дырку. Остальные точки продолжают работать независимо.
//
// ForceResyncRange ниже cursor намеренно не использует и не двигает.
func runPointSyncOnce(ctx context.Context, repo *sqliterepo.Repo, writer *PointMainsWriter, cfg Config) {
	now := time.Now()
	fallbackFrom := now.Add(-time.Duration(cfg.BackfillHours) * time.Hour)

	cursors := make(map[int]time.Time)
	allHaveCursor := len(cfg.Points) > 0
	var earliestSourceCursor time.Time
	shift := time.Duration(cfg.TimeShiftMinutes) * time.Minute

	for _, m := range cfg.Points {
		cur, found, err := repo.GetESSyncCursor(ctx, cfg.DeviceID, m.PointID)
		if err != nil {
			log.Printf("[es-sync] чтение cursor (%s ID_PP=%d): %v\n", m.Label, m.PointID, err)
			allHaveCursor = false
			continue
		}
		if !found {
			allHaveCursor = false
			continue
		}

		cursors[m.PointID] = cur

		// Cursor хранит уже СДВИНУТУЮ метку ЭС. collectReadings принимает
		// диапазон в шкале локального архива, поэтому сдвиг разворачиваем
		// обратно только для выбора нижней границы чтения.
		sourceCursor := cur.Add(-shift)
		if earliestSourceCursor.IsZero() || sourceCursor.Before(earliestSourceCursor) {
			earliestSourceCursor = sourceCursor
		}
	}

	from := fallbackFrom
	if allHaveCursor && !earliestSourceCursor.IsZero() {
		from = earliestSourceCursor
	}

	readings, err := collectReadings(ctx, repo, cfg, from, now)
	if err != nil {
		log.Printf("[es-sync] чтение исходной БД: %v\n", err)
		return
	}
	if len(readings) == 0 {
		return
	}

	var inserted, skipped, skippedByCursor, failed int
	blocked := make(map[int]bool)

	advanceCursor := func(r pointReading) bool {
		if err := repo.SetESSyncCursor(ctx, cfg.DeviceID, r.mapping.PointID, r.ts); err != nil {
			log.Printf("[es-sync] запись cursor (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), err)
			failed++
			blocked[r.mapping.PointID] = true
			return false
		}
		cursors[r.mapping.PointID] = r.ts
		return true
	}

	for _, r := range readings {
		pointID := r.mapping.PointID

		if cur, ok := cursors[pointID]; ok && !r.ts.After(cur) {
			skippedByCursor++
			continue
		}

		// Не перескакиваем cursor через ошибку более раннего значения
		// этой же точки. Иначе дырка стала бы невидимой навсегда.
		if blocked[pointID] {
			continue
		}

		if err := validatePointReading(r); err != nil {
			log.Printf("[es-sync] БЛОКИРОВКА записи (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, pointID, r.ts.Format("02.01 15:04"), err)
			failed++
			blocked[pointID] = true
			continue
		}

		present, err := writer.PointExists(ctx, pointID, r.ts)
		if err != nil {
			log.Printf("[es-sync] проверка наличия точки (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, pointID, r.ts.Format("02.01 15:04"), err)
			failed++
			blocked[pointID] = true
			continue
		}
		if present {
			skipped++
			advanceCursor(r)
			continue
		}

		if cfg.DryRun {
			log.Printf("[es-sync] DRY-RUN записал бы: %s ID_PP=%d %s value=%g\n",
				r.mapping.Label, pointID, r.ts.Format("02.01.2006 15:04"), r.value)
			inserted++
			// DRY-RUN ничего не подтвердил в ЭС, поэтому cursor не двигаем.
			continue
		}

		// state=0 ("достоверно") hardcoded — see package doc's "KNOWN GAP".
		if err := writer.InsertPoint(ctx, pointID, r.ts, r.value, 0); err != nil {
			if IsDuplicateKeyError(err) {
				skipped++
				advanceCursor(r)
				continue
			}
			log.Printf("[es-sync] запись (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, pointID, r.ts.Format("02.01 15:04"), err)
			failed++
			blocked[pointID] = true
			continue
		}

		inserted++
		health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
		advanceCursor(r)
	}

	if inserted > 0 || failed > 0 {
		log.Printf("[es-sync] проход завершён: записано %d, пропущено (уже есть) %d, пропущено по cursor %d, ошибок %d, окно %s..%s\n",
			inserted, skipped, skippedByCursor, failed,
			from.Format("02.01 15:04"), now.Format("02.01 15:04"))
	}
}

// PreviewForceResyncRange performs the same source read and safety
// validation as ForceResyncRange, but does NOT change PointMains.
//
// It answers the operator's key question before a destructive resync:
// how many values would overwrite existing rows, how many would be new
// inserts, and how many are blocked by validation or DB-check errors.
func PreviewForceResyncRange(ctx context.Context, repo *sqliterepo.Repo, writer *PointMainsWriter, cfg Config, from, to time.Time) (wouldUpdate, wouldInsert, blocked int, err error) {
	readings, err := collectReadingsForESRange(ctx, repo, cfg, from, to)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("чтение исходной БД: %w", err)
	}
	if len(readings) == 0 {
		return 0, 0, 0, nil
	}

	for _, r := range readings {
		if verr := validatePointReading(r); verr != nil {
			blocked++
			continue
		}

		present, perr := writer.PointExists(ctx, r.mapping.PointID, r.ts)
		if perr != nil {
			log.Printf("[es-sync] preview: ошибка проверки точки (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), perr)
			blocked++
			continue
		}
		if present {
			wouldUpdate++
		} else {
			wouldInsert++
		}
	}

	log.Printf("[es-sync] preview принудительной пересинхронизации: будет переписано %d, вставлено %d, заблокировано/ошибок %d, окно %s..%s\n",
		wouldUpdate, wouldInsert, blocked, from.Format("02.01 15:04"), to.Format("02.01 15:04"))

	return wouldUpdate, wouldInsert, blocked, nil
}

// ForceResyncRange принудительно ПЕРЕЗАПИСЫВАЕТ (не пропускает уже
// существующие) точки PointMains за указанный диапазон [from, to] для
// заданного прибора — отдельное, явно запрошенное оператором действие
// (добавлено 2026-08-30, прямой запрос оператора: "бывает что с прибора
// попали искажённые данные и нужно переопросить прибор и чтобы новые
// данные попали в эс"). НЕ часть обычного автоматического прохода —
// runPointSyncOnce выше по-прежнему только вставляет новое и пропускает
// существующее, это сознательный выбор, чтобы не создавать лишнюю
// нагрузку на БД ЭС проверкой/перезаписью каждой точки при каждом
// обычном часовом цикле. Работает для ОБОИХ типов приборов, как и
// runPointSyncOnce — источник данных выбирается тем же collectReadings.
//
// Типичные поводы: (а) с прибора один раз пришли искажённые данные,
// потом сделали «Принудительный переопрос» — свежие верные значения
// появились в НАШЕЙ локальной базе, но в ЭС остались старые, раз там
// уже что-то есть под той же меткой времени и обычная синхронизация их
// не трогает; (б) поменяли множитель точки (см. «Точки ЭС» в
// api_admin_ui.go) — новые точки сами пойдут в правильных единицах, а
// уже отправленная история так и останется в старых, пока её явно не
// перезаписать.
//
// Для каждой точки диапазона: пробуем UpdatePoint (перезаписать, если
// уже есть); если затронуто 0 строк — точки ещё не было, вставляем
// обычным InsertPoint.
func ForceResyncRange(ctx context.Context, repo *sqliterepo.Repo, writer *PointMainsWriter, cfg Config, from, to time.Time) (updated, inserted, failed int, err error) {
	readings, err := collectReadingsForESRange(ctx, repo, cfg, from, to)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("чтение исходной БД: %w", err)
	}
	if len(readings) == 0 {
		return 0, 0, 0, nil
	}

	for _, r := range readings {
		if verr := validatePointReading(r); verr != nil {
			log.Printf("[es-sync] БЛОКИРОВКА принудительной записи (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), verr)
			failed++
			continue
		}

		affected, uerr := writer.UpdatePoint(ctx, r.mapping.PointID, r.ts, r.value, 0)
		if uerr != nil {
			log.Printf("[es-sync] принудительная перезапись (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), uerr)
			failed++
			continue
		}
		if affected > 0 {
			updated++
			health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
			continue
		}

		// Точки ещё не было вообще — вставляем как обычно.
		if ierr := writer.InsertPoint(ctx, r.mapping.PointID, r.ts, r.value, 0); ierr != nil {
			if IsDuplicateKeyError(ierr) {
				// Гонка: кто-то вставил её между UpdatePoint и
				// InsertPoint — не беда, пробуем перезаписать ещё раз.
				if _, uerr2 := writer.UpdatePoint(ctx, r.mapping.PointID, r.ts, r.value, 0); uerr2 == nil {
					updated++
					health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
					continue
				}
			}
			log.Printf("[es-sync] принудительная запись (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), ierr)
			failed++
			continue
		}
		inserted++
		health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
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
