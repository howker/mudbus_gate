// energosphere_sync.go pushes collected meter values (ВКМ-360: heat
// energy, mass, temperature, pressure; Akron: volume; IVK-TER: hourly archive fields) from mbgw's own
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
// Direct DB delivery is the confirmed-working path for these
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
	"sort"
	"strconv"
	"strings"
	"time"

	"mbgw/internal/health"
	"mbgw/internal/interval"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/vkmraw"
)

// PointMapping — одна точка ЭС (ID_PP в PointMains), в которую пишем
// один параметр прибора с заданным множителем. У ВКМ таких точек обычно
// 4 на прибор (масса/тепло/температура/давление, теги "S"/"ST"/"T"/"Pi"),
// у Akron — одна (объём, тег "V"); у ИВК-ТЭР могут быть настроены любые
// нужные поля его часового archive_hourly (v_plus, v_minus, q_avg и т.д.).
type PointMapping struct {
	Tag     string // тег параметра прибора — см. collectVKMReadings/collectAkronReadings
	PointID int    // ID_PP в PointMains
	Pipe    int    // ВКМ: трубопровод 1..10; 0 = использовать legacy Config.Pipe
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
	Pipe     int    // используется только при Kind="vkm360" (номер трубопровода в архиве); остальные типы игнорируют
	Kind     string // "vkm360" | "akron" | "ivk-ter" — источник данных; см. collect*Readings

	Points []PointMapping

	IntervalSec   int
	BackfillHours int
	DryRun        bool

	// TimeShiftMinutes — общий сдвиг метки времени перед записью в
	// PointMains. Применяется одинаково ко всем поддержанным типам.
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

// The durable dirty queue (B1) is the primary repair mechanism. This recent
// reconciliation is a second, deliberately bounded safety net for gaps that
// pre-date the queue or were created outside mbgw. It runs at worker startup
// and then hourly, checking only the latest seven days. That window matches
// the historical default ES backfill depth while keeping SQL Server work
// small: one timestamp-range SELECT per configured ID_PP, not one query per
// reading.
const (
	recentESReconcileWindow   = 7 * 24 * time.Hour
	recentESReconcileInterval = time.Hour
)

// RunEnergosphereSync opens both databases and loops every interval,
// syncing the backfill window, until ctx is cancelled.
// trigger, если не nil, позволяет вызывающему коду попросить сделать
// ВНЕПЛАНОВЫЙ проход прямо сейчас, не дожидаясь обычного часового тикера
// — используется только явной операцией синхронизации с ЭС.
// Принудительный переопрос архива больше этот trigger не вызывает: он
// работает только по цепочке «прибор -> локальная БД МодбасШлюза».
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
		return fmt.Errorf("не удалось открыть локальную БД (%s): %w", sqlitePath, err)
	}
	defer repo.Close()

	return RunEnergosphereSyncWithRepo(ctx, repo, cfg, trigger)
}

// RunEnergosphereSyncWithRepo запускает тот же цикл, но использует уже
// открытый общий Repo. Production-режим `mbgw server` обязан использовать
// именно этот вариант: отдельный sqliterepo.New на каждый прибор создаёт
// несколько независимых пулов соединений к одному SQLite-файлу и может
// привести к SQLITE_BUSY при параллельной записи.
func RunEnergosphereSyncWithRepo(ctx context.Context, repo *sqliterepo.Repo, cfg Config, trigger <-chan struct{}) error {
	if repo == nil {
		return fmt.Errorf("локальная БД не подключена")
	}
	if err := repo.InitESSyncCursorSchema(ctx); err != nil {
		return fmt.Errorf("не удалось подготовить курсор синхронизации с ЭС: %w", err)
	}

	writer, err := OpenPointMainsWriter(SQLServerConfig{
		Server:   cfg.SQLServer,
		Database: cfg.SQLDatabase,
		User:     cfg.SQLUser,
		Password: cfg.SQLPassword,
		Port:     cfg.SQLPort,
	})
	if err != nil {
		return fmt.Errorf("не удалось открыть подключение к SQL Server ЭС: %w", err)
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
			log.Printf("[синхронизация с ЭС] подключение к БД ЭС успешно: сервер=%s база=%s логин=%s\n", cfg.SQLServer, cfg.SQLDatabase, cfg.SQLUser)
			break
		}

		if ctx.Err() != nil {
			log.Println("[синхронизация с ЭС] остановлен до восстановления подключения к БД ЭС")
			return nil
		}

		log.Printf("[синхронизация с ЭС] БД ЭС недоступна: %v; повтор через %s\n", err, retryDelay)

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			log.Println("[синхронизация с ЭС] остановлен до восстановления подключения к БД ЭС")
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
		for _, pipe := range vkmMappedPipes(cfg) {
			if n, oldest, newest, found, err := repo.CountVKMRaw(ctx, cfg.DeviceID, pipe); err != nil {
				log.Printf("[синхронизация с ЭС] предупреждение: не смог опросить исходную БД ВКМ, трубопровод %d: %v\n", pipe, err)
			} else if !found {
				log.Printf("[синхронизация с ЭС] ВНИМАНИЕ: в локальной БД нет ни одной записи для прибор=%s труба=%d — опрос прибора собрал данные?\n", cfg.DeviceID, pipe)
			} else {
				log.Printf("[синхронизация с ЭС] исходная БД: прибор=%s труба=%d, %d записей ВКМ, период с %s по %s\n",
					cfg.DeviceID, pipe, n, oldest.Format("02.01.2006 15:04"), newest.Format("02.01.2006 15:04"))
			}
		}
	}

	if cfg.DryRun {
		log.Println("[синхронизация с ЭС] РЕЖИМ ПРОВЕРКИ БЕЗ ЗАПИСИ: в БД ЭС ничего не пишется, только лог того, что было бы записано.")
	}

	runPointSyncOnce(ctx, repo, writer, cfg)
	ticker := time.NewTicker(cfg.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[синхронизация с ЭС] остановлен")
			return nil
		case <-ticker.C:
			runPointSyncOnce(ctx, repo, writer, cfg)
		case <-trigger:
			log.Println("[синхронизация с ЭС] внеплановая синхронизация по запросу")
			runPointSyncOnce(ctx, repo, writer, cfg)
		}
	}
}

// ConfigLoader returns the current ЭС configuration for one device.
// found=false is not an error: it means the operator has not configured
// the connection and/or any PointMains mappings yet. A long-lived worker
// must stay alive in that state so settings saved later are picked up
// without restarting mbgw.
type ConfigLoader func(context.Context) (cfg Config, found bool, err error)

type sqlConnectionKey struct {
	server   string
	database string
	user     string
	password string
	port     int
}

func connectionKey(cfg Config) sqlConnectionKey {
	return sqlConnectionKey{
		server:   cfg.SQLServer,
		database: cfg.SQLDatabase,
		user:     cfg.SQLUser,
		password: cfg.SQLPassword,
		port:     cfg.SQLPort,
	}
}

// RunEnergosphereSyncReloadingWithRepo is the production server worker.
// Unlike RunEnergosphereSyncWithRepo, it reloads the device's mappings and
// multipliers before EVERY pass. If SQL connection settings change, the old
// PointMainsWriter is closed and a new one is opened automatically.
//
// This deliberately survives an initially incomplete configuration: an
// enabled device may be physically unavailable at mbgw startup, or its
// ID_PP mappings may be saved later from /admin. Neither situation should
// require a service restart merely to start local-DB -> ЭС delivery.
func RunEnergosphereSyncReloadingWithRepo(ctx context.Context, repo *sqliterepo.Repo, load ConfigLoader, trigger <-chan struct{}) error {
	if repo == nil {
		return fmt.Errorf("локальная БД не подключена")
	}
	if load == nil {
		return fmt.Errorf("не задан загрузчик конфигурации синхронизации с ЭС")
	}
	if err := repo.InitESSyncCursorSchema(ctx); err != nil {
		return fmt.Errorf("не удалось подготовить курсор синхронизации с ЭС: %w", err)
	}
	if err := repo.InitESDirtyRangeSchema(ctx); err != nil {
		return fmt.Errorf("не удалось подготовить очередь восстановления пропусков ЭС: %w", err)
	}

	var writer *PointMainsWriter
	var writerKey sqlConnectionKey
	writerConfigured := false
	defer func() {
		if writer != nil {
			_ = writer.Close()
		}
	}()

	closeWriter := func() {
		if writer != nil {
			_ = writer.Close()
			writer = nil
		}
		writerConfigured = false
	}

	retryDelay := 5 * time.Second
	waitFor := time.Duration(0)
	loggedNotConfigured := false
	var dirtyTrigger <-chan struct{}
	wakeOnDirty := false
	var nextRecentReconcile time.Time

	for {
		if waitFor > 0 {
			timer := time.NewTimer(waitFor)
			dirtyWake := dirtyTrigger
			if !wakeOnDirty {
				dirtyWake = nil
			}
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				log.Println("[синхронизация с ЭС] остановлен")
				return nil
			case <-timer.C:
			case <-trigger:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				log.Println("[синхронизация с ЭС] внеплановая синхронизация по запросу")
			case <-dirtyWake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				log.Println("[синхронизация с ЭС] локальный архив обновлён — внеплановая проверка пропусков ЭС")
			}
		} else {
			select {
			case <-ctx.Done():
				log.Println("[синхронизация с ЭС] остановлен")
				return nil
			default:
			}
		}

		cfg, found, err := load(ctx)
		if err != nil {
			closeWriter()
			wakeOnDirty = false
			log.Printf("[синхронизация с ЭС] не удалось перечитать настройки: %v; повтор через %s\n", err, retryDelay)
			waitFor = retryDelay
			if retryDelay < 30*time.Second {
				retryDelay *= 2
				if retryDelay > 30*time.Second {
					retryDelay = 30 * time.Second
				}
			}
			continue
		}
		if !found {
			closeWriter()
			wakeOnDirty = false
			if !loggedNotConfigured {
				log.Println("[синхронизация с ЭС] подключение к БД ЭС или точки не настроены; worker остаётся запущен и подхватит настройки без перезапуска службы")
				loggedNotConfigured = true
			}
			retryDelay = 5 * time.Second
			waitFor = time.Minute
			continue
		}
		loggedNotConfigured = false
		if dirtyTrigger == nil {
			dirtyTrigger = repo.ESDirtySignal(cfg.DeviceID)
		}

		key := connectionKey(cfg)
		openedNow := false
		if writer == nil || !writerConfigured || key != writerKey {
			closeWriter()
			writer, err = OpenPointMainsWriter(SQLServerConfig{
				Server:   cfg.SQLServer,
				Database: cfg.SQLDatabase,
				User:     cfg.SQLUser,
				Password: cfg.SQLPassword,
				Port:     cfg.SQLPort,
			})
			if err != nil {
				wakeOnDirty = false
				log.Printf("[синхронизация с ЭС] не удалось открыть подключение к SQL Server ЭС: %v; повтор через %s\n", err, retryDelay)
				waitFor = retryDelay
				if retryDelay < 30*time.Second {
					retryDelay *= 2
					if retryDelay > 30*time.Second {
						retryDelay = 30 * time.Second
					}
				}
				continue
			}
			writerKey = key
			writerConfigured = true
			openedNow = true
		}

		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pingErr := writer.Ping(pingCtx)
		cancel()
		if pingErr != nil {
			closeWriter()
			wakeOnDirty = false
			log.Printf("[синхронизация с ЭС] БД ЭС недоступна: %v; повтор через %s\n", pingErr, retryDelay)
			waitFor = retryDelay
			if retryDelay < 30*time.Second {
				retryDelay *= 2
				if retryDelay > 30*time.Second {
					retryDelay = 30 * time.Second
				}
			}
			continue
		}

		retryDelay = 5 * time.Second
		if openedNow {
			log.Printf("[синхронизация с ЭС] подключение к БД ЭС успешно: прибор=%s, сервер=%s, база=%s, точек=%d\n",
				cfg.DeviceID, cfg.SQLServer, cfg.SQLDatabase, len(cfg.Points))
		}
		runPointSyncOnce(ctx, repo, writer, cfg)
		if stats, healErr := healESDirtyRanges(ctx, repo, writer, cfg, 256); healErr != nil {
			log.Printf("[синхронизация с ЭС] восстановление пропусков по изменённому локальному архиву: %v\n", healErr)
		} else if stats.RangesCompleted > 0 || stats.Inserted > 0 || stats.Blocked > 0 {
			log.Printf("[синхронизация с ЭС] восстановление пропусков: диапазонов проверено %d, закрыто %d, добавлено %d, уже было %d, заблокировано проверкой %d, ошибок %d\n",
				stats.RangesChecked, stats.RangesCompleted, stats.Inserted, stats.Existing, stats.Blocked, stats.Failed)
		}

		now := time.Now()
		if nextRecentReconcile.IsZero() || !now.Before(nextRecentReconcile) {
			from := now.Add(-recentESReconcileWindow)
			stats, recErr := reconcileRecentESWindow(ctx, repo, writer, cfg, from, now)
			nextRecentReconcile = now.Add(recentESReconcileInterval)
			if recErr != nil {
				log.Printf("[синхронизация с ЭС] периодическая проверка недавних пропусков: %v\n", recErr)
			} else if stats.Inserted > 0 || stats.Blocked > 0 || stats.Failed > 0 {
				log.Printf("[синхронизация с ЭС] периодическая проверка %s..%s: локальных точек %d, запросов к ЭС %d, добавлено %d, уже было %d, заблокировано %d, ошибок %d\n",
					from.Format("02.01 15:04"), now.Format("02.01 15:04"),
					stats.LocalReadings, stats.BatchQueries, stats.Inserted, stats.Existing, stats.Blocked, stats.Failed)
			}
		}

		wakeOnDirty = true
		waitFor = cfg.interval()
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
	// Historical min/max channel limits are intentionally ignored. They were
	// a UI-added safety layer that could permanently block a valid commercial
	// point and stall cursor progress. NaN/Inf remains the hard safety barrier.
	return nil
}

func dirtyFailureSourcePipe(cfg Config, m PointMapping) int {
	pipe := m.Pipe
	if cfg.Kind == "" || cfg.Kind == "vkm360" {
		if pipe == 0 {
			pipe = cfg.Pipe
		}
		if pipe == 0 {
			pipe = 1
		}
	}
	return pipe
}

// resolveDirtyFailureReading reconstructs a failed point from the CURRENT
// mapping and CURRENT local archive. The stored point ID/value in the retry
// row are diagnostic snapshots only and are never trusted for a retry write.
// This prevents an operator mapping/factor correction from causing stale data
// to be inserted into a now unrelated Energosphere ID_PP.
func resolveDirtyFailureReading(ctx context.Context, repo *sqliterepo.Repo, cfg Config, f sqliterepo.ESDirtyPointFailure) (pointReading, bool, error) {
	if strings.TrimSpace(f.SourceTag) == "" {
		return pointReading{}, false, nil
	}

	var current *PointMapping
	for i := range cfg.Points {
		m := &cfg.Points[i]
		if m.Tag != f.SourceTag || dirtyFailureSourcePipe(cfg, *m) != f.SourcePipe {
			continue
		}
		if current != nil {
			return pointReading{}, false, fmt.Errorf("неоднозначная текущая привязка источника pipe=%d tag=%q", f.SourcePipe, f.SourceTag)
		}
		current = m
	}
	if current == nil {
		return pointReading{}, false, nil
	}

	retryCfg := cfg
	retryCfg.Points = []PointMapping{*current}
	readings, err := collectReadingsForESRange(ctx, repo, retryCfg, f.Ts, f.Ts)
	if err != nil {
		return pointReading{}, false, err
	}
	for _, r := range readings {
		if r.mapping.PointID == current.PointID && r.ts.Equal(f.Ts) {
			return r, true, nil
		}
	}
	return pointReading{}, false, nil
}

func vkmMappedPipes(cfg Config) []int {
	seen := make(map[int]bool)
	for _, m := range cfg.Points {
		pipe := m.Pipe
		if pipe == 0 {
			pipe = cfg.Pipe
		}
		if pipe == 0 {
			pipe = 1
		}
		if pipe >= 1 && pipe <= 10 {
			seen[pipe] = true
		}
	}
	if len(seen) == 0 {
		pipe := cfg.Pipe
		if pipe < 1 || pipe > 10 {
			pipe = 1
		}
		seen[pipe] = true
	}
	out := make([]int, 0, len(seen))
	for pipe := range seen {
		out = append(out, pipe)
	}
	sort.Ints(out)
	return out
}

// collectVKMReadings читает диапазон сырых строк архива ВКМ и извлекает
// из каждой все теги, перечисленные в cfg.Points — тот же путь, что был
// и раньше, просто вынесен в отдельную функцию, чтобы runPointSyncOnce
// мог одинаково работать что с этим источником, что с Akron'овским.
func collectVKMReadings(ctx context.Context, repo *sqliterepo.Repo, cfg Config, from, now time.Time) ([]pointReading, error) {
	// A single device may map points from several VKM pipes. Group mappings
	// first so each physical/raw pipe range is read once per sync pass.
	byPipe := make(map[int][]PointMapping)
	for _, m := range cfg.Points {
		pipe := m.Pipe
		if pipe == 0 {
			pipe = cfg.Pipe
		}
		if pipe == 0 {
			pipe = 1
		}
		if pipe < 1 || pipe > 10 {
			return nil, fmt.Errorf("ВКМ: точка ID_PP=%d настроена на недопустимый трубопровод %d", m.PointID, pipe)
		}
		byPipe[pipe] = append(byPipe[pipe], m)
	}

	pipes := make([]int, 0, len(byPipe))
	for pipe := range byPipe {
		pipes = append(pipes, pipe)
	}
	sort.Ints(pipes)

	var out []pointReading
	missingWarned := make(map[string]bool)
	for _, pipe := range pipes {
		rows, err := repo.GetVKMRawStringsRange(ctx, cfg.DeviceID, pipe, from, now)
		if err != nil {
			return nil, fmt.Errorf("чтение сырых строк ВКМ, трубопровод %d: %w", pipe, err)
		}

		for _, row := range rows {
			// Apply the configured ES time shift exactly once, preserving the
			// historical single-pipe semantics for every pipe.
			esTime := row.TsHour.Add(time.Duration(cfg.TimeShiftMinutes) * time.Minute)
			for _, m := range byPipe[pipe] {
				rawVal, ok := vkmraw.Float(row.RawString, m.Tag)
				if !ok {
					key := fmt.Sprintf("%d/%s", pipe, m.Tag)
					if !missingWarned[key] {
						log.Printf("[синхронизация с ЭС] ВНИМАНИЕ: прибор=%s трубопровод=%d: настроенный источник %q отсутствует в сырой строке; ID_PP=%d пока не обновляется\n",
							cfg.DeviceID, pipe, m.Tag, m.PointID)
						missingWarned[key] = true
					}
					continue
				}
				value := rawVal * m.Factor
				out = append(out, pointReading{
					mapping: m,
					ts:      esTime,
					value:   value,
				})
			}
		}
	}
	return out, nil
}

// hourlyHalfHourMode describes how one HOURLY source value is projected
// onto Energosphere's fixed 30-minute PointMains grid.
//
// This is deliberately a value-semantic decision, not a device-name rule:
//   - interval total/duration: split 50/50, preserving the hourly sum;
//   - average/state: repeat the same value for both half-hours.
//
// A cumulative counter is converted to an hourly interval value BEFORE this
// helper is called (Akron V is the current example). Unknown semantics must not
// be silently guessed: the caller has to classify the parameter explicitly.
type hourlyHalfHourMode uint8

const (
	hourlySplitTotal hourlyHalfHourMode = iota
	hourlyRepeatValue
)

// projectHourlyToHalfHours converts one hourly value whose timestamp marks the
// END of the hour into two PointMains values whose timestamps mark the END of
// each half-hour. TimeShiftMinutes is applied only after those logical
// half-hour boundaries have been formed.
func projectHourlyToHalfHours(m PointMapping, hourEnd time.Time, value float64, shift time.Duration, mode hourlyHalfHourMode) []pointReading {
	finalValue := value * m.Factor
	firstValue := finalValue
	secondValue := finalValue
	if mode == hourlySplitTotal {
		firstValue /= 2
		secondValue /= 2
	}

	return []pointReading{
		{
			mapping: m,
			ts:      hourEnd.Add(-30 * time.Minute).Add(shift),
			value:   firstValue,
		},
		{
			mapping: m,
			ts:      hourEnd.Add(shift),
			value:   secondValue,
		},
	}
}

// wallClockInLocation keeps the visible calendar fields and only changes
// the Location. This is NOT an instant conversion. IVK-TER archive_time is
// decoded/stored as UTC-located time.Time even though the device value is a
// local wall-clock calendar. Comparing it as an absolute instant against
// operator ranges parsed in time.Local shifts the effective boundary by the
// server UTC offset.
func wallClockInLocation(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	return time.Date(
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc,
	)
}

// normalizeESSyncCursorTime keeps IVK-TER cursors on the same neutral
// wall-clock scale as archive_hourly. Existing IVK cursors were written from
// UTC-located archive timestamps, while the wall-clock fix rebuilds new
// PointMains timestamps in the caller's local Location. Comparing those values
// directly as absolute instants would make the same visible calendar timestamp
// differ by the server UTC offset.
//
// IVK cursors are therefore stored and compared as UTC-located wall-clock
// calendar fields. Other device kinds keep their existing instant semantics.
func normalizeESSyncCursorTime(cfg Config, t time.Time) time.Time {
	switch cfg.Kind {
	case "ivk-ter", "ivk_ter":
		return wallClockInLocation(t, time.UTC)
	default:
		return t
	}
}

// ivkHalfHourMode fixes the semantics of every field that the current
// IVK-TER hourly profile exposes. This table is intentionally explicit: when a
// new field/device is added, its meaning must be decided instead of falling
// back to an accidental "write the hourly point as-is" path.
func ivkHalfHourMode(tag string) (hourlyHalfHourMode, bool) {
	switch tag {
	case "v_plus", "v_minus", "comm_fail_time", "downtime", "power_loss_time":
		// Totals/durations accumulated over the hour. There is no finer source
		// profile, so the documented approximation is an even 50/50 split.
		return hourlySplitTotal, true
	case "q_avg", "resistance", "errors", "flowmeter_type":
		// Average/state/configuration values describe the hourly period as a
		// whole; preserve the value in both half-hour slots.
		return hourlyRepeatValue, true
	default:
		return 0, false
	}
}

// collectIVKReadings читает уже декодированные часовые поля ИВК-ТЭР
// из общей archive_hourly и приводит их к обязательной получасовой сетке ЭС.
//
// v_plus/v_minus являются готовыми объёмами ЗА ЧАС (live-сверка: V+ около
// 74.6 м3 при Qср около 1243 л/мин за 60 минут), поэтому их не нужно
// превращать в дельту, но нужно разделить 50/50 между двумя получасовками.
// Средние значения/состояния повторяются в обеих получасовках. Метка
// row.TsHour означает конец часового периода; TimeShiftMinutes применяется
// после формирования меток HH:30 и HH+1:00.
func collectIVKReadings(ctx context.Context, repo *sqliterepo.Repo, cfg Config, from, now time.Time) ([]pointReading, error) {
	var out []pointReading
	shift := time.Duration(cfg.TimeShiftMinutes) * time.Minute

	// IVK-TER archive timestamps are stored with Location=UTC, but their
	// calendar fields are device LOCAL wall-clock, not UTC instants. Query the
	// SQLite rows in that same neutral wall-clock representation, then rebuild
	// the resulting PointMains timestamps in the caller's wall-clock location
	// (normally time.Local). This keeps range comparisons and TimeShiftMinutes
	// on one calendar scale without changing the stored archive schema.
	wallLoc := now.Location()
	queryFrom := wallClockInLocation(from, time.UTC)
	queryTo := wallClockInLocation(now, time.UTC)

	for _, m := range cfg.Points {
		mode, ok := ivkHalfHourMode(m.Tag)
		if !ok {
			return nil, fmt.Errorf("ИВК-ТЭР: для параметра %q не задано правило преобразования часового значения в получасовые точки ЭС", m.Tag)
		}

		rows, err := repo.GetHourlyArchiveRange(ctx, cfg.DeviceID, "", m.Tag, queryFrom, queryTo)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			hourEnd := wallClockInLocation(row.TsHour, wallLoc)
			out = append(out, projectHourlyToHalfHours(m, hourEnd, row.Value, shift, mode)...)
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

		// Для Akron сейчас подтверждён только V. Любой новый часовой тег
		// сначала должен получить явную семантику (total/average/state),
		// иначе нельзя корректно положить его в получасовую сетку ЭС.
		if m.Tag != "V" {
			return nil, fmt.Errorf("Akron: для параметра %q не задано правило преобразования часового значения в получасовые точки ЭС", m.Tag)
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
					// CounterDelta уже превратил накопительный счётчик в
					// часовую величину. Дальше действует общий контракт ЭС:
					// interval total -> две равные получасовки.
					out = append(out, projectHourlyToHalfHours(m, row.TsHour, delta, shift, hourlySplitTotal)...)
				} else if row.TsHour.Sub(prevTS) != time.Hour {
					log.Printf(
						"[синхронизация с ЭС] Akron %s: пропуск %s — нет соседнего часового снимка перед ним (предыдущий %s)\n",
						cfg.DeviceID,
						row.TsHour.Format("02.01.2006 15:04"),
						prevTS.Format("02.01.2006 15:04"),
					)
				} else {
					log.Printf(
						"[синхронизация с ЭС] Akron %s: пропуск %s — накопительный V уменьшился: %g -> %g\n",
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
	case "ivk-ter", "ivk_ter":
		return collectIVKReadings(ctx, repo, cfg, from, now)
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

	// Hourly sources create a point at hourEnd-30m as well as at hourEnd.
	// If the requested ES range ends exactly on that FIRST half-hour point,
	// the source row we need is 30 minutes later than sourceTo. Read one
	// half-hour ahead, then keep the existing final ES-range filter below.
	switch cfg.Kind {
	case "akron", "ivk-ter", "ivk_ter":
		sourceTo = sourceTo.Add(30 * time.Minute)
	}

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
			log.Printf("[синхронизация с ЭС] чтение курсора (%s ID_PP=%d): %v\n", m.Label, m.PointID, err)
			allHaveCursor = false
			continue
		}
		if !found {
			allHaveCursor = false
			continue
		}

		cur = normalizeESSyncCursorTime(cfg, cur)
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
		log.Printf("[синхронизация с ЭС] чтение исходной БД: %v\n", err)
		return
	}
	if len(readings) == 0 {
		return
	}

	var inserted, skipped, skippedByCursor, failed int
	blocked := make(map[int]bool)

	advanceCursor := func(r pointReading) bool {
		cursorTS := normalizeESSyncCursorTime(cfg, r.ts)
		if err := repo.SetESSyncCursor(ctx, cfg.DeviceID, r.mapping.PointID, cursorTS); err != nil {
			log.Printf("[синхронизация с ЭС] запись курсора (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), err)
			failed++
			blocked[r.mapping.PointID] = true
			return false
		}
		cursors[r.mapping.PointID] = cursorTS
		return true
	}

	for _, r := range readings {
		pointID := r.mapping.PointID

		cursorTS := normalizeESSyncCursorTime(cfg, r.ts)
		if cur, ok := cursors[pointID]; ok && !cursorTS.After(cur) {
			skippedByCursor++
			continue
		}

		// Не перескакиваем cursor через ошибку более раннего значения
		// этой же точки. Иначе дырка стала бы невидимой навсегда.
		if blocked[pointID] {
			continue
		}

		if err := validatePointReading(r); err != nil {
			log.Printf("[синхронизация с ЭС] БЛОКИРОВКА записи (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, pointID, r.ts.Format("02.01 15:04"), err)
			failed++
			blocked[pointID] = true
			continue
		}

		present, err := writer.PointExists(ctx, pointID, r.ts)
		if err != nil {
			log.Printf("[синхронизация с ЭС] проверка наличия точки (%s ID_PP=%d %s): %v\n",
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
			log.Printf("[синхронизация с ЭС] ПРОВЕРКА БЕЗ ЗАПИСИ — было бы записано: %s ID_PP=%d %s значение=%g\n",
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
			log.Printf("[синхронизация с ЭС] запись (%s ID_PP=%d %s): %v\n",
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
		log.Printf("[синхронизация с ЭС] проход завершён: записано %d, пропущено (уже есть) %d, пропущено по курсору %d, ошибок %d, окно %s..%s\n",
			inserted, skipped, skippedByCursor, failed,
			from.Format("02.01 15:04"), now.Format("02.01 15:04"))
	}
}

type insertOnlyPointWriter interface {
	PointExists(context.Context, int, time.Time) (bool, error)
	InsertPoint(context.Context, int, time.Time, float64, int) error
}

type recentReconcileWriter interface {
	insertOnlyPointWriter
	ExistingPointTimes(context.Context, int, time.Time, time.Time) (map[string]struct{}, error)
}

type recentReconcileStats struct {
	LocalReadings int
	BatchQueries  int
	Inserted      int
	Existing      int
	Blocked       int
	Failed        int
}

// reconcileRecentESWindow is the bounded, periodic second safety layer. It
// does not use or move the ordinary cursor and does not require a dirty-range
// marker. Existing PointMains rows are never overwritten.
func reconcileRecentESWindow(ctx context.Context, repo *sqliterepo.Repo, writer recentReconcileWriter, cfg Config, from, to time.Time) (recentReconcileStats, error) {
	var stats recentReconcileStats
	if repo == nil {
		return stats, fmt.Errorf("локальная БД не подключена")
	}
	if writer == nil {
		return stats, fmt.Errorf("запись в ЭС не подключена")
	}
	if to.Before(from) {
		from, to = to, from
	}

	readings, err := collectReadingsForESRange(ctx, repo, cfg, from, to)
	if err != nil {
		return stats, fmt.Errorf("чтение локального архива: %w", err)
	}
	stats.LocalReadings = len(readings)
	if len(readings) == 0 {
		return stats, nil
	}

	byPoint := make(map[int][]pointReading)
	for _, r := range readings {
		byPoint[r.mapping.PointID] = append(byPoint[r.mapping.PointID], r)
	}

	for pointID, pointReadings := range byPoint {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.BatchQueries++
		existing, err := writer.ExistingPointTimes(ctx, pointID, from, to)
		if err != nil {
			stats.Failed += len(pointReadings)
			log.Printf("[синхронизация с ЭС] периодическая проверка ID_PP=%d: чтение существующих меток: %v\n", pointID, err)
			continue
		}

		for _, r := range pointReadings {
			if err := validatePointReading(r); err != nil {
				stats.Blocked++
				log.Printf("[синхронизация с ЭС] периодическая проверка: БЛОКИРОВКА (%s ID_PP=%d %s): %v\n",
					r.mapping.Label, pointID, r.ts.Format("02.01 15:04"), err)
				continue
			}

			key := pointMainsTimeKey(r.ts)
			if _, ok := existing[key]; ok {
				stats.Existing++
				continue
			}
			if cfg.DryRun {
				log.Printf("[синхронизация с ЭС] периодическая ПРОВЕРКА БЕЗ ЗАПИСИ — было бы добавлено: %s ID_PP=%d %s значение=%g\n",
					r.mapping.Label, pointID, r.ts.Format("02.01.2006 15:04"), r.value)
				continue
			}

			if err := writer.InsertPoint(ctx, pointID, r.ts, r.value, 0); err != nil {
				if IsDuplicateKeyError(err) {
					existing[key] = struct{}{}
					stats.Existing++
					continue
				}
				stats.Failed++
				log.Printf("[синхронизация с ЭС] периодическая проверка: вставка (%s ID_PP=%d %s): %v\n",
					r.mapping.Label, pointID, r.ts.Format("02.01 15:04"), err)
				continue
			}

			existing[key] = struct{}{}
			stats.Inserted++
			health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
		}
	}

	return stats, nil
}

type dirtyHealStats struct {
	RangesChecked   int
	RangesCompleted int
	Inserted        int
	Existing        int
	Blocked         int
	Failed          int
}

// healESDirtyRanges reconciles source ranges that were changed locally after
// the ordinary per-point cursor may already have moved past them.
//
// SAFETY CONTRACT: this is INSERT-MISSING-ONLY. Existing PointMains rows are
// never updated here; overwriting existing ES data remains exclusively the
// explicit operator ForceResyncRange action.
func healESDirtyRanges(ctx context.Context, repo *sqliterepo.Repo, writer insertOnlyPointWriter, cfg Config, limit int) (dirtyHealStats, error) {
	var stats dirtyHealStats
	if repo == nil {
		return stats, fmt.Errorf("локальная БД не подключена")
	}
	if writer == nil {
		return stats, fmt.Errorf("запись в ЭС не подключена")
	}
	if limit <= 0 {
		limit = 256
	}

	// Hard per-pass budget: healing must never monopolize the ES sync loop.
	passDeadline := time.Now().Add(5 * time.Second)
	opsLeft := limit * 4
	if opsLeft < 64 {
		opsLeft = 64
	}

	// Retry isolated poison points first, but only after their persisted
	// backoff expires. Before touching ES, reconstruct every point from the
	// CURRENT source mapping and local archive; stored Value/PointID are never
	// replayed blindly after an operator changes ID_PP or Factor.
	failures, err := repo.ListESDirtyPointFailures(ctx, cfg.DeviceID, 64)
	if err != nil {
		return stats, err
	}
	retryFailed := 0
	retryDropped := 0
	for _, f := range failures {
		if time.Now().After(passDeadline) || opsLeft <= 0 {
			break
		}
		opsLeft--

		r, found, resolveErr := resolveDirtyFailureReading(ctx, repo, cfg, f)
		if resolveErr != nil {
			stats.Failed++
			retryFailed++
			f.LastError = resolveErr.Error()
			_ = repo.RecordESDirtyPointFailure(ctx, f)
			continue
		}
		if !found {
			// Mapping was removed/changed beyond recognition, or this is a
			// legacy first-P0 row with no source identity. There is no safe
			// target to retry, so retire it instead of writing stale data.
			_ = repo.CompleteESDirtyPointFailure(ctx, f)
			retryDropped++
			continue
		}
		if err := validatePointReading(r); err != nil {
			stats.Blocked++
			retryFailed++
			if r.mapping.PointID != f.PointID {
				_ = repo.CompleteESDirtyPointFailure(ctx, f)
			}
			_ = repo.RecordESDirtyPointFailure(ctx, sqliterepo.ESDirtyPointFailure{
				DeviceID: cfg.DeviceID, PointID: r.mapping.PointID, Ts: r.ts, Value: r.value,
				State: f.State, SourcePipe: dirtyFailureSourcePipe(cfg, r.mapping), SourceTag: r.mapping.Tag,
				LastError: err.Error(),
			})
			continue
		}

		present, writeErr := writer.PointExists(ctx, r.mapping.PointID, r.ts)
		if writeErr == nil && present {
			_ = repo.CompleteESDirtyPointFailure(ctx, f)
			stats.Existing++
			continue
		}
		if writeErr == nil && !cfg.DryRun {
			writeErr = writer.InsertPoint(ctx, r.mapping.PointID, r.ts, r.value, f.State)
			if writeErr == nil || IsDuplicateKeyError(writeErr) {
				_ = repo.CompleteESDirtyPointFailure(ctx, f)
				if writeErr == nil {
					stats.Inserted++
					health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
				} else {
					stats.Existing++
				}
				continue
			}
		}

		stats.Failed++
		retryFailed++
		msg := "dry-run: запись отложена"
		if writeErr != nil {
			msg = writeErr.Error()
		}
		if r.mapping.PointID != f.PointID {
			_ = repo.CompleteESDirtyPointFailure(ctx, f)
		}
		_ = repo.RecordESDirtyPointFailure(ctx, sqliterepo.ESDirtyPointFailure{
			DeviceID: cfg.DeviceID, PointID: r.mapping.PointID, Ts: r.ts, Value: r.value,
			State: f.State, SourcePipe: dirtyFailureSourcePipe(cfg, r.mapping), SourceTag: r.mapping.Tag,
			LastError: msg,
		})
	}
	if retryFailed > 0 {
		log.Printf("[синхронизация с ЭС] dirty-point АЛАРМ: %d изолированных точек всё ещё не восстановлены; повтор отложен по backoff\n", retryFailed)
	}
	if retryDropped > 0 {
		log.Printf("[синхронизация с ЭС] dirty-point: снято %d устаревших retry-записей без действующей текущей привязки\n", retryDropped)
	}
	if time.Now().After(passDeadline) || opsLeft <= 0 {
		return stats, nil
	}

	ranges, err := repo.ListESDirtyRanges(ctx, cfg.DeviceID, limit)
	if err != nil {
		return stats, err
	}

	for _, dr := range ranges {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if time.Now().After(passDeadline) || opsLeft <= 0 {
			break
		}
		stats.RangesChecked++

		sourceFrom := dr.From
		sourceTo := dr.To
		switch cfg.Kind {
		case "ivk-ter", "ivk_ter":
			sourceFrom = wallClockInLocation(sourceFrom, time.Local)
			sourceTo = wallClockInLocation(sourceTo, time.Local)
		}

		readings, readErr := collectReadings(ctx, repo, cfg, sourceFrom, sourceTo)
		if readErr != nil {
			stats.Failed++
			log.Printf("[синхронизация с ЭС] dirty-range %s..%s: чтение локального архива: %v\n",
				dr.From.Format("02.01 15:04"), dr.To.Format("02.01 15:04"), readErr)
			continue
		}

		rangeDBOK := true
		for _, r := range readings {
			if time.Now().After(passDeadline) || opsLeft <= 0 {
				return stats, nil
			}
			opsLeft--
			if err := validatePointReading(r); err != nil {
				stats.Blocked++
				log.Printf("[синхронизация с ЭС] dirty-range: БЛОКИРОВКА (%s ID_PP=%d %s): %v\n",
					r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), err)
				continue
			}

			present, err := writer.PointExists(ctx, r.mapping.PointID, r.ts)
			if err == nil && present {
				stats.Existing++
				continue
			}
			if err == nil && cfg.DryRun {
				stats.Failed++
				rangeDBOK = false
				continue
			}
			if err == nil {
				err = writer.InsertPoint(ctx, r.mapping.PointID, r.ts, r.value, 0)
				if err == nil {
					stats.Inserted++
					health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
					continue
				}
				if IsDuplicateKeyError(err) {
					stats.Existing++
					continue
				}
			}

			// Isolate a failing point. The source range can still close, while this
			// exact point remains in a durable bounded retry/alarm queue.
			stats.Failed++
			if recErr := repo.RecordESDirtyPointFailure(ctx, sqliterepo.ESDirtyPointFailure{
				DeviceID: cfg.DeviceID, PointID: r.mapping.PointID, Ts: r.ts,
				Value: r.value, State: 0, SourcePipe: dirtyFailureSourcePipe(cfg, r.mapping),
				SourceTag: r.mapping.Tag, LastError: err.Error(),
			}); recErr != nil {
				return stats, recErr
			}
			log.Printf("[синхронизация с ЭС] dirty-range: точка вынесена в отдельный retry/alarm (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), err)
		}

		if !rangeDBOK {
			continue
		}
		completed, err := repo.CompleteESDirtyRange(ctx, dr)
		if err != nil {
			stats.Failed++
			continue
		}
		if completed {
			stats.RangesCompleted++
		}
	}

	return stats, nil
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
			log.Printf("[синхронизация с ЭС] предпросмотр: ошибка проверки точки (%s ID_PP=%d %s): %v\n",
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

	log.Printf("[синхронизация с ЭС] предпросмотр принудительной пересинхронизации: будет переписано %d, вставлено %d, заблокировано/ошибок %d, окно %s..%s\n",
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
			log.Printf("[синхронизация с ЭС] БЛОКИРОВКА принудительной записи (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), verr)
			failed++
			continue
		}

		affected, uerr := writer.UpdatePoint(ctx, r.mapping.PointID, r.ts, r.value, 0)
		if uerr != nil {
			log.Printf("[синхронизация с ЭС] принудительная перезапись (%s ID_PP=%d %s): %v\n",
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
			log.Printf("[синхронизация с ЭС] принудительная запись (%s ID_PP=%d %s): %v\n",
				r.mapping.Label, r.mapping.PointID, r.ts.Format("02.01 15:04"), ierr)
			failed++
			continue
		}
		inserted++
		health.MarkESWriteSuccess(cfg.DeviceID, time.Now())
	}

	log.Printf("[синхронизация с ЭС] принудительная пересинхронизация завершена: переписано %d, вставлено новых %d, ошибок %d, окно %s..%s\n",
		updated, inserted, failed, from.Format("02.01 15:04"), to.Format("02.01 15:04"))
	return updated, inserted, failed, nil
}

// parseVKMTagFloat is kept as a narrow compatibility wrapper for older tests/callers.
// The actual raw VKM parser is shared with the archive UI in internal/vkmraw.
func parseVKMTagFloat(raw, tag string) (float64, bool) {
	return vkmraw.Float(raw, tag)
}
