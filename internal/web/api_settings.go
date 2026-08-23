package web

import (
	"encoding/json"
	"log"
	"net/http"

	"mbgw/internal/dbg"
)

// api_settings.go implements GET/POST /api/settings — process-wide
// operator settings (web port, debug logging) backed by the
// app_settings table (see repo_app_settings.go). Порт переключается
// живьём (Server.Rebind), без перезапуска процесса — исправлено
// 2026-08-23 после жалобы, что оператор не может/не должен знать
// команду перезапуска mbgw server вручную. debug_log_enabled применяется
// сразу же (dbg.Enabled — простой пакетный флаг, без гонок при обычном
// использовании из одного HTTP-запроса).

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

		// Порт переключается ЖИВЬЁМ, без перезапуска процесса (см.
		// Server.Rebind в server.go) — фикс на 2026-08-23: раньше UI
		// требовал от оператора вручную перезапускать mbgw server,
		// хотя штатный способ запуска — служба Windows, и оператор
		// физически не может (и не должен) знать команду перезапуска.
		note := "Сохранено."
		if portChanged {
			if err := s.Rebind(*body.ConfiguredPort); err != nil {
				writeJSON(w, http.StatusOK, map[string]string{
					"status": "ok",
					"note":   "Порт сохранён, но переключиться на него сейчас не удалось: " + err.Error() + ". Перезапустите mbgw server вручную.",
				})
				return
			}
			if err := s.repo.SetActualPort(r.Context(), *body.ConfiguredPort); err != nil {
				log.Printf("[WEB] не удалось обновить фактический порт после переключения: %v\n", err)
			}
			note = "Сохранено и применено — сервер уже работает на новом порту, откройте эту страницу заново по новому адресу."
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "note": note})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}
