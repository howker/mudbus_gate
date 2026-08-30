// Package integration implements delivery of collected data to external
// (upstream) systems, as anticipated by LLD.md's internal/integration
// package ("export.go, rest_out.go, mqtt.go [post-MVP], extdb.go
// [post-MVP]") and explicitly permitted by FINAL_TRD's integration options
// for Энергосфера ("через сетевой доступ к БД, API, экспорт/импорт или
// linked server/ODBC-схемы, если это допустимо на стороне заказчика").
//
// extdb.go is that "extdb" piece: a direct network write into an external
// SQL Server database's own table (Энергосфера's Mains), bypassing that
// system's own device drivers. This exists because the ЭС's УВП280 driver
// rejects heat/pressure values outright regardless of content (see
// energosphere_sync.go's package doc for the full history), and a full day
// spent emulating a ВЗЛЕТ ТСРВ-024 device over TCP hit undiagnosable
// driver-side behavior — direct DB delivery is the one confirmed-working
// path for these two channels.
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

// MainsWriter is a thin, schema-aware client for one specific external
// table shape: (ID_Channel int, MeasureDate datetime, Value float,
// State int) — Энергосфера's Mains table. It is not a general-purpose SQL
// client; it only knows how to check for and insert ONE point.
type MainsWriter struct {
	db       *sql.DB
	database string
}

// OpenMainsWriter opens the connection (does not verify it — call Ping
// separately so callers can produce a clear startup error).
//
// encrypt=disable is required: this targets SQL Server 2008 R2, which
// predates the driver's default TLS expectations. Safe here because the
// connection is expected to stay on localhost (see FINAL_TRD's platform
// notes on this specific deployment).
func OpenMainsWriter(cfg SQLServerConfig) (*MainsWriter, error) {
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
	return &MainsWriter{db: db, database: cfg.Database}, nil
}

func (w *MainsWriter) Close() error { return w.db.Close() }

func (w *MainsWriter) Ping(ctx context.Context) error {
	return w.db.PingContext(ctx)
}

// CheckChannelHistory проверяет, есть ли УЖЕ данные в указанном канале
// Mains — используется как защита от случайного назначения канала,
// который на самом деле принадлежит СОВСЕМ ДРУГОЙ, посторонней точке ЭС
// (не одному из наших приборов). В отличие от FindChannelConflicts
// (repo_device_config.go), которая знает только про каналы, уже
// настроенные У НАС САМИХ, — это спрашивает напрямую саму ЭС: если в
// канале уже есть история (особенно давняя, старше того, как мы вообще
// начали писать в эту базу) — почти наверняка канал занят чем-то чужим,
// и наша запись туда испортит данные постороннего прибора. found=false
// означает канал совершенно пустой, безопасен для использования.
func (w *MainsWriter) CheckChannelHistory(ctx context.Context, channel int) (rowCount int, oldest, newest time.Time, found bool, err error) {
	var minTS, maxTS sql.NullTime
	err = w.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT COUNT(*), MIN(MeasureDate), MAX(MeasureDate)
FROM [%s].dbo.Mains WITH (NOLOCK)
WHERE ID_Channel = @p1
`, w.database), channel).Scan(&rowCount, &minTS, &maxTS)
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
func (w *MainsWriter) ListDatabases(ctx context.Context) ([]string, error) {
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

// PointExists checks whether Mains already holds a row for (channel, ts).
// WITH (NOLOCK) and an exact-key probe keep this cheap and off the ЭС DB's
// back — never a scan.
//
// CAST(@p2 AS datetime) is required, not cosmetic: the mssql driver sends a
// Go time.Time parameter as the higher-precision "datetime2" SQL type by
// default, while Mains.MeasureDate is plain "datetime". Comparing
// datetime2 against datetime lets SQL Server's implicit conversion round
// the two sides slightly differently, so an existing row (written via
// InsertPoint, same CAST) could fail to match here — silently defeating
// this check and causing a duplicate-key error on the subsequent insert.
// Confirmed live (2026-08-21): every row from a prior successful run
// showed up as "not found" here before this cast was added, spamming
// duplicate-key errors on a retry loop every sync interval. Casting both
// sides to the SAME datetime precision makes the comparison exact.
func (w *MainsWriter) PointExists(ctx context.Context, channel int, ts time.Time) (bool, error) {
	q := fmt.Sprintf(
		"SELECT TOP 1 1 FROM [%s].dbo.Mains WITH (NOLOCK) WHERE ID_Channel = @p1 AND MeasureDate = CAST(@p2 AS datetime)",
		w.database)
	var one int
	err := w.db.QueryRowContext(ctx, q, channel, ts).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// InsertPoint writes one row into Mains. state follows Mains' own
// convention: 0 = достоверно, 1 = недостоверно/брак — callers should
// derive this from internal/quality's Tag once quality integration lands
// (see energosphere_sync.go's TODO); for now callers pass 0 directly.
// See PointExists's doc comment for why the CAST matters here too.
func (w *MainsWriter) InsertPoint(ctx context.Context, channel int, ts time.Time, value float64, state int) error {
	q := fmt.Sprintf(
		"INSERT INTO [%s].dbo.Mains (ID_Channel, MeasureDate, Value, State) VALUES (@p1, CAST(@p2 AS datetime), @p3, @p4)",
		w.database)
	_, err := w.db.ExecContext(ctx, q, channel, ts, value, state)
	return err
}

// UpdatePoint overwrites Value/State for an EXISTING row in Mains — used
// by the «Принудительная пересинхронизация с ЭС» operator action
// (добавлено 2026-08-30, прямой запрос оператора: "бывает что с прибора
// попали искажённые данные и нужно переопросить прибор и чтобы новые
// данные попали в эс"). The ordinary sync loop
// (energosphere_sync.go's runEnergosphereSyncOnce) NEVER calls this —
// it only inserts new points and skips existing ones, deliberately, to
// avoid touching every already-synced point on every routine hourly
// pass. This is a separate, explicitly operator-triggered path (see
// ForceResyncRange) for the case where a point already in Mains needs
// correcting — either the source data was garbled and has since been
// re-collected correctly (see internal/device/vkm_reload.go), or the
// channel's unit conversion factor changed after the point was already
// sent in the wrong unit (see api_admin_ui.go's «Каналы ЭС» tab).
//
// Returns the number of rows actually changed (0 if the point didn't
// exist at all yet — ForceResyncRange falls back to InsertPoint in that
// case, giving "insert if missing, overwrite if present" semantics
// overall).
func (w *MainsWriter) UpdatePoint(ctx context.Context, channel int, ts time.Time, value float64, state int) (int64, error) {
	q := fmt.Sprintf(
		"UPDATE [%s].dbo.Mains SET Value = @p3, State = @p4 WHERE ID_Channel = @p1 AND MeasureDate = CAST(@p2 AS datetime)",
		w.database)
	res, err := w.db.ExecContext(ctx, q, channel, ts, value, state)
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
