package servicelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Entry — одна запись отдельного диагностического журнала службы.
// Этот журнал живёт в mbgw_service_log.db и не зависит от основной БД
// измерений: при проблеме mbgw_server.db причина перезапуска остаётся
// доступной оператору.
type Entry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"-"`
	Time     string    `json:"time"`
	Level    string    `json:"level"`
	Category string    `json:"category"`
	Message  string    `json:"message"`
}

type Log struct {
	db *sql.DB
}

func Open(path string) (*Log, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть БД журнала службы: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=1500;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("не удалось настроить БД журнала службы: %w", err)
	}
	l := &Log{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Log) init(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS service_events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       DATETIME NOT NULL,
    level    TEXT NOT NULL,
    category TEXT NOT NULL,
    message  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_service_events_ts ON service_events(ts DESC);
`)
	if err != nil {
		return fmt.Errorf("не удалось создать таблицу журнала службы: %w", err)
	}
	return nil
}

func (l *Log) Close() error { return l.db.Close() }

func (l *Log) Append(ctx context.Context, level, category, message string) error {
	if level == "" {
		level = "информация"
	}
	if category == "" {
		category = "служба"
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO service_events(ts, level, category, message) VALUES (?, ?, ?, ?)`,
		time.Now(), level, category, message)
	if err != nil {
		return fmt.Errorf("не удалось записать журнал службы: %w", err)
	}
	return nil
}

func (l *Log) List(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, ts, level, category, message FROM service_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать журнал службы: %w", err)
	}
	defer rows.Close()
	out := make([]Entry, 0, limit)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.At, &e.Level, &e.Category, &e.Message); err != nil {
			return nil, fmt.Errorf("не удалось разобрать запись журнала службы: %w", err)
		}
		e.Time = e.At.Local().Format("02.01.2006 15:04:05")
		out = append(out, e)
	}
	return out, rows.Err()
}

// Cleanup удаляет старые записи. Вызывается при старте и затем раз в сутки.
func (l *Log) Cleanup(ctx context.Context, before time.Time) error {
	_, err := l.db.ExecContext(ctx, `DELETE FROM service_events WHERE ts < ?`, before)
	if err != nil {
		return fmt.Errorf("не удалось очистить старые записи журнала службы: %w", err)
	}
	return nil
}
