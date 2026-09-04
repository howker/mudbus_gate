package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// api_es_resync.go implements POST /api/es-sync/force-resync —
// принудительно ПЕРЕЗАПИСЫВАЕТ точки Mains за указанный диапазон, а не
// только вставляет новое (добавлено 2026-08-30, прямой запрос
// оператора: "бывает что с прибора попали искажённые данные и нужно
// переопросить прибор и чтобы новые данные попали в эс"). Отдельно от
// «Синхронизировать сейчас» (POST /api/es-sync/trigger — просто просит
// обычный цикл сделать внеплановый проход «только новое» раньше
// положенного): здесь явно указывается диапазон и явно перезаписывается
// уже существующее.
type forceResyncRequest struct {
	DeviceID string `json:"device_id"`
	From     string `json:"from"`   // формат ГГГГ-ММ-ДДTЧЧ:ММ, локальное время сервера
	To       string `json:"to"`     // тот же формат; пусто = до "сейчас"
	Action   string `json:"action"` // "preview" | "execute"; обязательно явно
}

func (s *Server) handleForceResyncES(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	if s.onForceResyncES == nil {
		writeError(w, http.StatusServiceUnavailable, "принудительная пересинхронизация с ЭС не подключена в этом режиме запуска")
		return
	}

	var req forceResyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "поле device_id обязательно")
		return
	}
	if req.Action != "preview" && req.Action != "execute" {
		writeError(w, http.StatusBadRequest, `поле action должно быть "preview" или "execute"`)
		return
	}
	from, err := time.ParseInLocation("2006-01-02T15:04", req.From, time.Local)
	if err != nil {
		writeError(w, http.StatusBadRequest, "поле from должно быть в формате ГГГГ-ММ-ДДЧЧ:ММ")
		return
	}
	to := time.Now()
	if req.To != "" {
		to, err = time.ParseInLocation("2006-01-02T15:04", req.To, time.Local)
		if err != nil {
			writeError(w, http.StatusBadRequest, "поле to должно быть в формате ГГГГ-ММ-ДДЧЧ:ММ")
			return
		}
	}
	if to.Before(from) {
		writeError(w, http.StatusBadRequest, "поле to не может быть раньше from")
		return
	}

	updated, inserted, failed, err := s.onForceResyncES(r.Context(), req.DeviceID, from, to, req.Action)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"action":   req.Action,
		"updated":  updated,
		"inserted": inserted,
		"failed":   failed,
	})
}

// ForceResyncESFunc matches SetForceResyncES's parameter — separated as
// a named type only so server.go's field declaration and this file's
// usage stay readable.
type ForceResyncESFunc func(ctx context.Context, deviceID string, from, to time.Time, action string) (updated, inserted, failed int, err error)
