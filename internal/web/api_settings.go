package web

import (
	"encoding/json"
	"log"
	"net/http"

	"mbgw/internal/dbg"
)

type settingsJSON struct {
	ConfiguredPort         int  `json:"configured_port"`
	ActualPort             int  `json:"actual_port"`
	DebugLogEnabled        bool `json:"debug_log_enabled"`
	WatchdogTimeoutMinutes int  `json:"watchdog_timeout_minutes"`
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
			writeJSON(w, http.StatusOK, settingsJSON{ConfiguredPort: 8080, WatchdogTimeoutMinutes: 10})
			return
		}
		writeJSON(w, http.StatusOK, settingsJSON{
			ConfiguredPort: settings.ConfiguredPort, ActualPort: settings.ActualPort,
			DebugLogEnabled: settings.DebugLogEnabled, WatchdogTimeoutMinutes: settings.WatchdogTimeoutMinutes,
		})

	case http.MethodPost:
		var body struct {
			ConfiguredPort         *int  `json:"configured_port"`
			DebugLogEnabled        *bool `json:"debug_log_enabled"`
			WatchdogTimeoutMinutes *int  `json:"watchdog_timeout_minutes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		portChanged := false
		if body.ConfiguredPort != nil {
			if *body.ConfiguredPort <= 0 || *body.ConfiguredPort > 65535 {
				writeError(w, http.StatusBadRequest, "порт должен быть в диапазоне 1-65535")
				return
			}
			if err := s.repo.SetConfiguredPort(r.Context(), *body.ConfiguredPort); err != nil {
				writeError(w, http.StatusInternalServerError, "не удалось сохранить порт: "+err.Error())
				return
			}
			portChanged = *body.ConfiguredPort != s.port
		}
		if body.DebugLogEnabled != nil {
			if err := s.repo.SetDebugLogEnabled(r.Context(), *body.DebugLogEnabled); err != nil {
				writeError(w, http.StatusInternalServerError, "не удалось сохранить настройку лога: "+err.Error())
				return
			}
			dbg.Enabled = *body.DebugLogEnabled
		}
		if body.WatchdogTimeoutMinutes != nil {
			if *body.WatchdogTimeoutMinutes < 2 || *body.WatchdogTimeoutMinutes > 120 {
				writeError(w, http.StatusBadRequest, "таймаут контроля зависания должен быть от 2 до 120 минут")
				return
			}
			if err := s.repo.SetWatchdogTimeoutMinutes(r.Context(), *body.WatchdogTimeoutMinutes); err != nil {
				writeError(w, http.StatusInternalServerError, "не удалось сохранить таймаут контроля зависания: "+err.Error())
				return
			}
			if s.onWatchdogTimeout != nil {
				s.onWatchdogTimeout(*body.WatchdogTimeoutMinutes)
			}
		}

		note := "Сохранено и применено."
		if portChanged {
			if err := s.Rebind(*body.ConfiguredPort); err != nil {
				writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "note": "Порт сохранён, но переключиться на него сейчас не удалось: " + err.Error() + ". Перезапустите МодбасШлюз."})
				return
			}
			if err := s.repo.SetActualPort(r.Context(), *body.ConfiguredPort); err != nil {
				log.Printf("[ВЕБ] не удалось обновить фактический порт после переключения: %v\n", err)
			}
			note = "Сохранено и применено — сервер уже работает на новом порту, откройте эту страницу заново по новому адресу."
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "note": note})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}
