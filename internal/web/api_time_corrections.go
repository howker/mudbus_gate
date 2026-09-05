package web

import (
	"net/http"
	"strconv"
)

type timeCorrectionAPIRecord struct {
	DeviceID          string `json:"device_id"`
	DeviceName        string `json:"device_name"`
	CorrectedAt       string `json:"corrected_at"`
	CorrectionSeconds int    `json:"correction_seconds"`
}

// handleTimeCorrections exposes the durable VKM clock-correction journal
// stored in device_time_corrections. It is read-only and intended for the
// operator's "when / which meter / how many seconds" audit table.
func (s *Server) handleTimeCorrections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	rows, err := s.repo.ListTimeCorrections(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ошибка чтения истории коррекции времени: "+err.Error())
		return
	}

	devices, err := s.repo.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ошибка чтения приборов: "+err.Error())
		return
	}
	names := make(map[string]string, len(devices))
	for _, d := range devices {
		names[d.ID] = d.Name
	}

	out := make([]timeCorrectionAPIRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, timeCorrectionAPIRecord{
			DeviceID:          row.DeviceID,
			DeviceName:        names[row.DeviceID],
			CorrectedAt:       row.CorrectedAt.Format("02.01.2006 15:04:05"),
			CorrectionSeconds: row.CorrectionSeconds,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
