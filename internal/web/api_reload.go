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
//
// ЗАПУСКАЕТСЯ В ФОНЕ (добавлено 2026-08-27): переопрос может занимать
// много минут (каждый период — отдельное полное обращение к прибору),
// и раньше HTTP-запрос от браузера просто висел всё это время без
// единого признака происходящего — оператор не понимал, работает ли
// вообще что-то, или всё зависло. Теперь этот обработчик запускает
// переопрос в фоновой горутине и СРАЗУ отвечает {"started": true}, а
// реальный прогресс браузер узнаёт отдельными опросами
// GET /api/devices/reload-progress (см. handleReloadProgress ниже).
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

	// Заводим/перезаписываем запись о задаче ДО запуска горутины — чтобы
	// первый же опрос прогресса браузером (который может прилететь очень
	// быстро после ответа на этот POST) сразу нашёл что-то осмысленное,
	// а не "нет такой задачи".
	job := &reloadJob{Done: 0, Total: 0, Saved: 0, Finished: false}
	s.reloadJobsMu.Lock()
	s.reloadJobs[body.DeviceID] = job
	s.reloadJobsMu.Unlock()

	go func() {
		saved, err := s.onForceReload(body.DeviceID, from, to, func(done, total int) {
			s.reloadJobsMu.Lock()
			job.Done = done
			job.Total = total
			s.reloadJobsMu.Unlock()
		})

		s.reloadJobsMu.Lock()
		job.Saved = saved
		job.Finished = true
		if err != nil {
			job.Error = err.Error()
		}
		s.reloadJobsMu.Unlock()
	}()

	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

// handleReloadProgress — GET /api/devices/reload-progress?device_id=X:
// возвращает текущее состояние фонового переопроса для прибора (см.
// handleForceReload). found=false, если для этого прибора переопрос
// никогда не запускался (или сервер был перезапущен с тех пор — прогресс
// хранится только в памяти процесса, не в базе, это чисто для UI одной
// сессии наблюдения, переживать перезапуск ему не нужно).
func (s *Server) handleReloadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "параметр device_id обязателен")
		return
	}

	s.reloadJobsMu.Lock()
	job, found := s.reloadJobs[deviceID]
	var snapshot reloadJob
	if found {
		snapshot = *job // копия под замком — дальше отдаём уже без блокировки
	}
	s.reloadJobsMu.Unlock()

	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"found":    true,
		"done":     snapshot.Done,
		"total":    snapshot.Total,
		"saved":    snapshot.Saved,
		"finished": snapshot.Finished,
		"error":    snapshot.Error,
	})
}
