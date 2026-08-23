package web

import (
	"encoding/json"
	"net/http"
	"time"
)

// api_reload.go — POST /api/devices/reload-archive: принудительно
// перечитывает архив у прибора за указанный период и перезаписывает уже
// сохранённые записи. Работает для ОБОИХ типов приборов (Akron и ВКМ) —
// см. подробное объяснение в internal/device/akron_reload.go и
// internal/device/vkm_reload.go. Нужен, чтобы исправлять уже испорченные
// данные (например, от одноразовой помехи на линии связи).
func (s *Server) handleForceReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	if s.onForceReload == nil {
		writeError(w, http.StatusServiceUnavailable, "принудительный переопрос не подключён в этом режиме запуска")
		return
	}

	var body struct {
		DeviceID string `json:"device_id"`
		From     string `json:"from"` // формат ГГГГ-ММ-ДДTЧЧ:ММ, локальное время сервера
		To       string `json:"to"`   // тот же формат; для Akron игнорируется (см. ForceReloadAkronHourly — читает до "сейчас")
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if body.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "поле device_id обязательно")
		return
	}
	from, err := time.ParseInLocation("2006-01-02T15:04", body.From, time.Local)
	if err != nil {
		writeError(w, http.StatusBadRequest, "поле from должно быть в формате ГГГГ-ММ-ДДЧЧ:ММ")
		return
	}
	to := time.Now()
	if body.To != "" {
		to, err = time.ParseInLocation("2006-01-02T15:04", body.To, time.Local)
		if err != nil {
			writeError(w, http.StatusBadRequest, "поле to должно быть в формате ГГГГ-ММ-ДДЧЧ:ММ")
			return
		}
	}

	saved, err := s.onForceReload(body.DeviceID, from, to)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "saved": saved})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": saved})
}
