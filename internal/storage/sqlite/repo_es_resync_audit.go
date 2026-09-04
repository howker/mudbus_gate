package sqlite

import (
	"context"
	"fmt"
	"time"
)

// ESForceResyncAudit is one durable audit event for the operator-triggered
// ForceResync workflow. Action is "preview" or "execute".
type ESForceResyncAudit struct {
	DeviceID string
	From     time.Time
	To       time.Time
	Action   string

	Updated  int
	Inserted int
	Failed   int

	OK    bool
	Error string
}

// InitESForceResyncAuditSchema creates a durable audit table. It is kept
// separate from application logs so the history survives log rotation.
func (r *Repo) InitESForceResyncAuditSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS es_force_resync_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT NOT NULL,
    range_from  DATETIME NOT NULL,
    range_to    DATETIME NOT NULL,
    action      TEXT NOT NULL,              -- 'preview' | 'execute'
    updated     INTEGER NOT NULL DEFAULT 0, -- preview: would update
    inserted    INTEGER NOT NULL DEFAULT 0, -- preview: would insert
    failed      INTEGER NOT NULL DEFAULT 0, -- preview: blocked/failed
    ok          INTEGER NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_es_force_resync_audit_device_created
    ON es_force_resync_audit(device_id, created_at DESC);
`)
	if err != nil {
		return fmt.Errorf("init ES force-resync audit schema: %w", err)
	}
	return nil
}

// AddESForceResyncAudit appends one immutable audit row.
func (r *Repo) AddESForceResyncAudit(ctx context.Context, a ESForceResyncAudit) error {
	ok := 0
	if a.OK {
		ok = 1
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO es_force_resync_audit
    (device_id, range_from, range_to, action, updated, inserted, failed, ok, error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, a.DeviceID, a.From, a.To, a.Action, a.Updated, a.Inserted, a.Failed, ok, a.Error, time.Now())
	if err != nil {
		return fmt.Errorf("add ES force-resync audit: %w", err)
	}
	return nil
}
