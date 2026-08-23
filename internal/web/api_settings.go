package web

import (
	"encoding/json"
	"net/http"
)

// api_settings.go implements GET/POST /api/settings — process-wide
// operator settings (web port, debug logging) backed by the
// app_settings table (see repo_app_settings.go). Port changes here take
// effect on the NEXT restart of `mbgw server` (same "no hot-reload"
// principle already established for devices — see repo_device_config.go's
// doc comment), not immediately: rebinding a live HTTP listener mid-
// process is more complexity than this minimal slice needs.

type settingsJSON struct {
	ConfiguredPort  int  `json:"configured_port"`
	ActualPort      int  `json:"actual_port"`
	DebugLogEnabled bool `json:"debug_log_enabled"`
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings, found, err := s.repo.GetAppSettings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать настройки: "+err.Error())
			return
		}
		if !found {
			writeJSON(w, http.StatusOK, settingsJSON{ConfiguredPort: 8080})
			return
		}
		writeJSON(w, http.StatusOK, settingsJSON{
			ConfiguredPort:  settings.ConfiguredPort,
			ActualPort:      settings.ActualPort,
			DebugLogEnabled: settings.DebugLogEnabled,
		})

	case http.MethodPost:
		var body struct {
			ConfiguredPort  *int  `json:"configured_port"`
			DebugLogEnabled *bool `json:"debug_log_enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		if body.ConfiguredPort != nil {
			if *body.ConfiguredPort <= 0 || *body.ConfiguredPort > 65535 {
				writeError(w, http.StatusBadRequest, "порт должен быть в диапазоне 1-65535")
				return
			}
			if err := s.repo.SetConfiguredPort(r.Context(), *body.ConfiguredPort); err != nil {
				writeError(w, http.StatusInternalServerError, "не удалось сохранить порт: "+err.Error())
				return
			}
		}
		if body.DebugLogEnabled != nil {
			if err := s.repo.SetDebugLogEnabled(r.Context(), *body.DebugLogEnabled); err != nil {
				writeError(w, http.StatusInternalServerError, "не удалось сохранить настройку лога: "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "note": "изменения вступят в силу после перезапуска mbgw server"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}
