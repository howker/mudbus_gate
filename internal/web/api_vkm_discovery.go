package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	sqliterepo "mbgw/internal/storage/sqlite"
)

const vkmPipeDiscoveryTimeout = 3 * time.Minute

// handleVKMPipeDiscovery exposes the last saved discovery snapshot (GET) and
// performs a fresh physical scan (POST). A scan never edits vkm_active_pipes:
// "available/absent/uncertain" describes what the meter answered, while the
// operator still decides which pipes the normal poller should read.
func (s *Server) handleVKMPipeDiscovery(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" && r.Method == http.MethodPost {
		var body struct {
			DeviceID string `json:"device_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		deviceID = body.DeviceID
	}
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "параметр device_id обязателен")
		return
	}

	devRec, found, err := s.repo.GetDevice(r.Context(), deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать настройки прибора: "+err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("прибор %q не найден", deviceID))
		return
	}
	if devRec.Kind != "vkm360" {
		writeError(w, http.StatusBadRequest, "сканирование трубопроводов доступно только для ВКМ-360")
		return
	}

	switch r.Method {
	case http.MethodGet:
		rows, err := s.repo.GetVKMPipeDiscovery(r.Context(), deviceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать результаты сканирования: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rows)

	case http.MethodPost:
		if s.onVKMPipeScan == nil {
			writeError(w, http.StatusNotImplemented, "сканирование трубопроводов не подключено в этом режиме запуска")
			return
		}
		if !s.beginPhysicalOperation() {
			writeError(w, http.StatusServiceUnavailable, "служба завершает работу; новое сканирование не запускается")
			return
		}
		defer s.endPhysicalOperation()

		parent := r.Context()
		if s.baseCtx != nil {
			parent = s.baseCtx
		}
		scanCtx, cancel := context.WithTimeout(parent, vkmPipeDiscoveryTimeout)
		defer cancel()
		if s.baseCtx != nil {
			go func() {
				select {
				case <-r.Context().Done():
					cancel()
				case <-scanCtx.Done():
				}
			}()
		}
		results, err := s.onVKMPipeScan(scanCtx, deviceID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "не удалось пересканировать трубопроводы: "+err.Error())
			return
		}
		if len(results) != 10 {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("сканирование вернуло %d результатов вместо 10", len(results)))
			return
		}

		scannedAt := time.Now()
		rows := make([]sqliterepo.VKMPipeDiscoveryRecord, 0, len(results))
		for _, result := range results {
			var tags []string
			if result.Status == sqliterepo.VKMDiscoveryAvailable && result.Raw != "" {
				tags = extractVKMNumericTags(result.Raw)
			}
			rows = append(rows, sqliterepo.VKMPipeDiscoveryRecord{
				DeviceID:  deviceID,
				PipeNo:    result.PipeNo,
				Status:    result.Status,
				Detail:    result.Detail,
				Tags:      tags,
				ScannedAt: scannedAt,
			})
		}
		if err := s.repo.ReplaceVKMPipeDiscovery(scanCtx, deviceID, rows); err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить результаты сканирования: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rows)

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}
