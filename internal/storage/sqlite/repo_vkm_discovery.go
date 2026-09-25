package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	VKMDiscoveryAvailable = "available"
	VKMDiscoveryAbsent    = "absent"
	VKMDiscoveryUncertain = "uncertain"
	VKMDiscoveryError     = "error"
)

// VKMPipeDiscoveryRecord is the last physical discovery observation for one
// VKM pipe. It is intentionally separate from vkm_active_pipes: an empty
// period or a temporary line error must never silently change poll settings.
type VKMPipeDiscoveryRecord struct {
	DeviceID  string    `json:"device_id,omitempty"`
	PipeNo    int       `json:"pipe_no"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	ScannedAt time.Time `json:"scanned_at"`
}

func validVKMDiscoveryStatus(status string) bool {
	switch status {
	case VKMDiscoveryAvailable, VKMDiscoveryAbsent, VKMDiscoveryUncertain, VKMDiscoveryError:
		return true
	default:
		return false
	}
}

// ReplaceVKMPipeDiscovery atomically replaces the saved discovery snapshot for
// one device. Callers pass a complete scan (normally pipes 1..10); active-pipe
// configuration is not touched here.
func (r *Repo) ReplaceVKMPipeDiscovery(ctx context.Context, deviceID string, rows []VKMPipeDiscoveryRecord) error {
	if deviceID == "" {
		return fmt.Errorf("device id is empty")
	}
	seen := make(map[int]bool, len(rows))
	for _, row := range rows {
		if row.PipeNo < vkmMinPipe || row.PipeNo > vkmMaxPipe {
			return fmt.Errorf("VKM pipe %d is outside %d..%d", row.PipeNo, vkmMinPipe, vkmMaxPipe)
		}
		if seen[row.PipeNo] {
			return fmt.Errorf("VKM pipe %d discovery result is duplicated", row.PipeNo)
		}
		if !validVKMDiscoveryStatus(row.Status) {
			return fmt.Errorf("VKM pipe %d has invalid discovery status %q", row.PipeNo, row.Status)
		}
		seen[row.PipeNo] = true
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace vkm pipe discovery begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM vkm_pipe_discovery WHERE device_id = ?`, deviceID); err != nil {
		return fmt.Errorf("clear vkm pipe discovery: %w", err)
	}
	for _, row := range rows {
		scannedAt := row.ScannedAt
		if scannedAt.IsZero() {
			scannedAt = time.Now()
		}
		tags := row.Tags
		if tags == nil {
			tags = []string{}
		}
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			return fmt.Errorf("encode vkm pipe %d discovery tags: %w", row.PipeNo, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO vkm_pipe_discovery (device_id, pipe, status, detail, tags_json, scanned_at)
VALUES (?, ?, ?, ?, ?, ?)
`, deviceID, row.PipeNo, row.Status, row.Detail, string(tagsJSON), scannedAt); err != nil {
			return fmt.Errorf("insert vkm pipe %d discovery: %w", row.PipeNo, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace vkm pipe discovery commit: %w", err)
	}
	return nil
}

func (r *Repo) GetVKMPipeDiscovery(ctx context.Context, deviceID string) ([]VKMPipeDiscoveryRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT pipe, status, detail, tags_json, scanned_at
FROM vkm_pipe_discovery
WHERE device_id = ?
ORDER BY pipe ASC
`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("get vkm pipe discovery: %w", err)
	}
	defer rows.Close()

	out := make([]VKMPipeDiscoveryRecord, 0, 10)
	for rows.Next() {
		var row VKMPipeDiscoveryRecord
		var tagsJSON string
		row.DeviceID = deviceID
		if err := rows.Scan(&row.PipeNo, &row.Status, &row.Detail, &tagsJSON, &row.ScannedAt); err != nil {
			return nil, fmt.Errorf("scan vkm pipe discovery: %w", err)
		}
		if err := json.Unmarshal([]byte(tagsJSON), &row.Tags); err != nil {
			return nil, fmt.Errorf("decode vkm pipe %d discovery tags: %w", row.PipeNo, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get vkm pipe discovery rows: %w", err)
	}
	return out, nil
}
