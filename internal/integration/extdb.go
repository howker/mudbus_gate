// Package integration implements delivery of collected data to external
// (upstream) systems, as anticipated by LLD.md's internal/integration
// package ("export.go, rest_out.go, mqtt.go [post-MVP], extdb.go
// [post-MVP]") and explicitly permitted by FINAL_TRD's integration options
// for Энергосфера ("через сетевой доступ к БД, API, экспорт/импорт или
// linked server/ODBC-схемы, если это допустимо на стороне заказчика").
//
// extdb.go is that "extdb" piece: a direct network write into an external
// SQL Server database's own table (Энергосфера's PointMains), bypassing
// that system's own device drivers. This exists because the ЭС's УВП280
// driver rejects heat/pressure values outright regardless of content (see
// energosphere_sync.go's package doc for the full history), and a full day
// spent emulating a ВЗЛЕТ ТСРВ-024 device over TCP hit undiagnosable
// driver-side behavior — direct DB delivery is the one confirmed-working
// path for these two channels.
//
// ИЗМЕНЕНО (2026-08-31, прямой запрос оператора): раньше писали в таблицу
// Mains (ID_Channel/MeasureDate/Value/State). Теперь пишем в PointMains
// (ID_PP/DT/Val/State) — та же самая идея (одна точка = одна строка на
// момент времени), но другая таблица и другие названия колонок в самой
// ЭС. Разведано напрямую в базе (SSMS, dbo.PointMains, 2026-08-31) —
// см. историю в docs/, при необходимости свериться заново с реальной
// схемой перед следующим подобным переносом.
//
// Раньше эта запись была ИСКЛЮЧИТЕЛЬНО для приборов ВКМ (4 величины —
// масса/тепло/температура/давление); Akron получал данные в ЭС другим
// путём — эмуляцией физического прибора для родного драйвера ЭС (см.
// internal/northbound/akron_live.go, теперь закомментирован в
// cmd/mbgw/server.go). Теперь оба типа приборов пишут сюда же, одним и
// тем же путём — просто у Akron всего одна величина (объём), а не
// четыре.
//
// This file is deliberately the ONLY place in the codebase that imports
// the mssql driver — mirrors LLD.md's global dependency rule ("импорт
// СУБД-драйверов только в storage/sqlite и storage/postgres") applied to
// the external-DB case: any SQL-Server-specific code belongs here, not
// scattered into the sync orchestration logic in energosphere_sync.go.
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

// SQLServerConfig holds a SQL Server connection target. The password is
// expected to come from a config file on the server (see
// energosphere_sync.go's Config/LoadConfig), never hardcoded or logged.
type SQLServerConfig struct {
	Server   string // e.g. "localhost" (SSMS's "(local)" must be translated by the caller)
	Database string
	User     string
	Password string
	Port     int // 0 -> default 1433
}

// PointMainsWriter — тонкий, специализированный клиент ровно для одной
// внешней таблицы: (ID_PP int, DT datetime, Val float, State int) —
// таблицы PointMains Энергосферы. Не универсальный SQL-клиент — умеет
// только проверять и записывать ОДНУ точку.
//
// ПЕРЕИМЕНОВАНО (2026-08-31): раньше называлось MainsWriter, писало в
// таблицу Mains (ID_Channel/MeasureDate/Value/State) — переименовано
// вместе с переносом записи на PointMains, чтобы имя типа не расходилось
// с тем, во что он реально пишет.
type PointMainsWriter struct {
	db       *sql.DB
	database string
}

// OpenPointMainsWriter opens the connection (does not verify it — call
// Ping separately so callers can produce a clear startup error).
//
// encrypt=disable is required: this targets SQL Server 2008 R2, which
// predates the driver's default TLS expectations. Safe here because the
// connection is expected to stay on localhost (see FINAL_TRD's platform
// notes on this specific deployment).
func OpenPointMainsWriter(cfg SQLServerConfig) (*PointMainsWriter, error) {
	port := cfg.Port
	if port == 0 {
		port = 1433
	}
	u := &url.URL{
		Scheme: "sqlserver",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   fmt.Sprintf("%s:%d", cfg.Server, port),
	}
	q := url.Values{}
	q.Set("database", cfg.Database)
	q.Set("encrypt", "disable")
	q.Set("connection timeout", "15")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &PointMainsWriter{db: db, database: cfg.Database}, nil
}

func (w *PointMainsWriter) Close() error { return w.db.Close() }

func (w *PointMainsWriter) Ping(ctx context.Context) error {
	return w.db.PingContext(ctx)
}

// CheckPointHistory проверяет, есть ли УЖЕ данные у указанной точки
// (ID_PP) в PointMains — используется как защита от случайного
// назначения точки, которая на самом деле принадлежит СОВСЕМ ДРУГОМУ,
// постороннему объекту учёта ЭС (не одному из наших приборов). В отличие
// от FindPointConflicts (repo_device_config.go), которая знает только
// про точки, уже настроенные У НАС САМИХ, — это спрашивает напрямую саму
// ЭС: если у точки уже есть история (особенно давняя, старше того, как
// мы вообще начали писать в эту базу) — почти наверняка точка занята
// чем-то чужим, и наша запись туда испортит данные постороннего объекта.
// found=false означает точку совершенно пустой, безопасна для
// использования.
//
// ПЕРЕИМЕНОВАНО (2026-08-31): раньше CheckChannelHistory, читала Mains/
// ID_Channel/MeasureDate — теперь PointMains/ID_PP/DT.
func (w *PointMainsWriter) CheckPointHistory(ctx context.Context, pointID int) (rowCount int, oldest, newest time.Time, found bool, err error) {
	var minTS, maxTS sql.NullTime
	err = w.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT COUNT(*), MIN(DT), MAX(DT)
FROM [%s].dbo.PointMains WITH (NOLOCK)
WHERE ID_PP = @p1
`, w.database), pointID).Scan(&rowCount, &minTS, &maxTS)
	if err != nil {
		return 0, time.Time{}, time.Time{}, false, err
	}
	if rowCount == 0 {
		return 0, time.Time{}, time.Time{}, false, nil
	}
	if minTS.Valid {
		oldest = minTS.Time
	}
	if maxTS.Valid {
		newest = maxTS.Time
	}
	return rowCount, oldest, newest, true, nil
}

// ListDatabases returns the non-system database names visible to this
// connection — used by the Web UI's "Подключение к ЭС" test button to
// offer a dropdown of real databases instead of the operator typing a
// name blind (fix for 2026-08-23 feedback: "данные должны идти из
// выпадающего списка автоматом если это возможно"). System databases
// (master/tempdb/model/msdb) are excluded — matches the manual
// sys.databases query already used to find CSD_Astrakhan earlier in this
// project's SSMS sessions.
func (w *PointMainsWriter) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := w.db.QueryContext(ctx, `
SELECT name FROM sys.databases
WHERE name NOT IN ('master', 'tempdb', 'model', 'msdb')
ORDER BY name
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// PointExists checks whether PointMains already holds a row for
// (pointID, ts). WITH (NOLOCK) and an exact-key probe keep this cheap and
// off the ЭС DB's back — never a scan.
//
// CAST(@p2 AS datetime) is required, not cosmetic: the mssql driver sends
// a Go time.Time parameter as the higher-precision "datetime2" SQL type
// by default, while PointMains.DT is plain "datetime". Comparing
// datetime2 against datetime lets SQL Server's implicit conversion round
// the two sides slightly differently, so an existing row (written via
// InsertPoint, same CAST) could fail to match here — silently defeating
// this check and causing a duplicate-key error on the subsequent insert.
// Confirmed live (2026-08-21, тогда ещё для таблицы Mains — тот же
// принцип верен и для PointMains): every row from a prior successful run
// showed up as "not found" here before this cast was added, spamming
// duplicate-key errors on a retry loop every sync interval. Casting both
// sides to the SAME datetime precision makes the comparison exact.
func (w *PointMainsWriter) PointExists(ctx context.Context, pointID int, ts time.Time) (bool, error) {
	q := fmt.Sprintf(
		"SELECT TOP 1 1 FROM [%s].dbo.PointMains WITH (NOLOCK) WHERE ID_PP = @p1 AND DT = CAST(@p2 AS datetime)",
		w.database)
	var one int
	err := w.db.QueryRowContext(ctx, q, pointID, ts).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// InsertPoint writes one row into PointMains. state follows PointMains'
// own convention: 0 = достоверно, 1 = недостоверно/брак — callers should
// derive this from internal/quality's Tag once quality integration lands
// (see energosphere_sync.go's TODO); for now callers pass 0 directly.
// See PointExists's doc comment for why the CAST matters here too.
func (w *PointMainsWriter) InsertPoint(ctx context.Context, pointID int, ts time.Time, value float64, state int) error {
	q := fmt.Sprintf(
		"INSERT INTO [%s].dbo.PointMains (ID_PP, DT, Val, State) VALUES (@p1, CAST(@p2 AS datetime), @p3, @p4)",
		w.database)
	_, err := w.db.ExecContext(ctx, q, pointID, ts, value, state)
	return err
}

// UpdatePoint overwrites Val/State for an EXISTING row in PointMains —
// used by the «Принудительная пересинхронизация с ЭС» operator action
// (добавлено 2026-08-30, прямой запрос оператора: "бывает что с прибора
// попали искажённые данные и нужно переопросить прибор и чтобы новые
// данные попали в эс"). The ordinary sync loop
// (energosphere_sync.go's runEnergosphereSyncOnce) NEVER calls this —
// it only inserts new points and skips existing ones, deliberately, to
// avoid touching every already-synced point on every routine hourly
// pass. This is a separate, explicitly operator-triggered path (see
// ForceResyncRange) for the case where a point already in PointMains
// needs correcting — either the source data was garbled and has since
// been re-collected correctly (see internal/device/vkm_reload.go), or
// the point's unit conversion factor changed after the point was already
// sent in the wrong unit (see api_admin_ui.go's «Точки ЭС» tab).
//
// Returns the number of rows actually changed (0 if the point didn't
// exist at all yet — ForceResyncRange falls back to InsertPoint in that
// case, giving "insert if missing, overwrite if present" semantics
// overall).
func (w *PointMainsWriter) UpdatePoint(ctx context.Context, pointID int, ts time.Time, value float64, state int) (int64, error) {
	q := fmt.Sprintf(
		"UPDATE [%s].dbo.PointMains SET Val = @p3, State = @p4 WHERE ID_PP = @p1 AND DT = CAST(@p2 AS datetime)",
		w.database)
	res, err := w.db.ExecContext(ctx, q, pointID, ts, value, state)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IsDuplicateKeyError reports whether err is a SQL Server unique-constraint
// violation — used by the sync loop to treat "someone else already wrote
// this point" as a benign skip rather than a hard failure (can happen if
// PointExists briefly disagrees with the server's own index, e.g. under a
// concurrent writer; never expected in normal single-writer operation, but
// safe to tolerate rather than crash-loop on).
func IsDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "PRIMARY KEY") || strings.Contains(msg, "UNIQUE")
}
