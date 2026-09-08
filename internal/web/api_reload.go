package web

import (
	"context"
	"encoding/json"
	"errors"
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
// много минут (каждый период — отдельное полное обращение к прибору), и
// раньше HTTP-запрос от браузера просто висел всё это время без единого
// признака происходящего — оператор не понимал, работает ли вообще
// что-то, или всё зависло. Теперь этот обработчик запускает переопрос в
// фоновой горутине и СРАЗУ отвечает {"started": true}, а реальный
// прогресс браузер узнаёт отдельными опросами
// GET /api/devices/reload-progress (см. handleReloadProgress ниже).
//
// Принудительный переопрос работает только по цепочке
// «прибор -> локальная БД МодбасШлюза». Он намеренно не запускает
// синхронизацию с ЭС ни по ходу работы, ни после завершения. Для ЭС есть
// отдельная явная операция «Принудительная пересинхронизация с ЭС».

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

	// Защита от повторного запуска переопроса ТОГО ЖЕ прибора, пока
	// предыдущий ещё не завершился — оба физически дёргали бы одну и ту
	// же линию связи, мешая друг другу, а отслеживание прогресса сбилось
	// бы (новая задача тихо заменила бы старую в reloadJobs). Разные
	// приборы друг другу не мешают (у каждого своя линия связи, своя
	// запись в reloadJobs) — это ограничение только на ПОВТОРНЫЙ запуск
	// ОДНОГО И ТОГО ЖЕ прибора (добавлено 2026-08-27, прямой вопрос).
	//
	// Отдельный, ОТМЕНЯЕМЫЙ контекст на этот конкретный запуск — выводим
	// от s.baseCtx (живёт весь процесс), НЕ от r.Context() (обрывается
	// сразу после того, как обработчик вернёт управление, а мы возвращаем
	// его немедленно, запустив работу в фоне). Сохраняем cancel в самой
	// задаче — handleReloadCancel вызовет его по запросу оператора.
	//
	// Проверка и создание задачи — под ОДНОЙ блокировкой (не двумя
	// последовательными), чтобы исключить гонку между "проверили, что
	// свободно" и "записали новую задачу", если два запроса для одного
	// прибора пришли почти одновременно.
	s.reloadJobsMu.Lock()
	if existing, ok := s.reloadJobs[body.DeviceID]; ok && !existing.Finished {
		s.reloadJobsMu.Unlock()
		writeError(w, http.StatusConflict, "для этого прибора уже идёт переопрос — дождитесь завершения или нажмите «Отменить»")
		return
	}
	if !s.beginPhysicalOperation() {
		s.reloadJobsMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "МодбасШлюз останавливается — новый переопрос не запускается")
		return
	}
	jobCtx, cancel := context.WithCancel(s.baseCtx)
	job := &reloadJob{Done: 0, Total: 0, Saved: 0, Finished: false, StartedAt: time.Now(), cancel: cancel}
	s.reloadJobs[body.DeviceID] = job
	s.reloadJobsMu.Unlock()

	go func() {
		defer s.endPhysicalOperation()
		defer cancel() // освобождаем ресурсы контекста, даже если переопрос завершился сам, без отмены

		saved, err := s.onForceReload(jobCtx, body.DeviceID, from, to, func(done, total int) {
			s.reloadJobsMu.Lock()
			job.Done = done
			job.Total = total
			s.reloadJobsMu.Unlock()
		})

		s.reloadJobsMu.Lock()
		job.Saved = saved
		job.Finished = true
		job.FinishedAt = time.Now()
		if errors.Is(err, context.Canceled) {
			job.Error = "Отменено оператором"
		} else if err != nil {
			job.Error = err.Error()
		}
		s.reloadJobsMu.Unlock()
	}()

	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

// handleReloadCancel — POST /api/devices/reload-cancel: прерывает
// переопрос, запущенный для указанного прибора, если он ещё не завершён
// (добавлено 2026-08-27, прямой запрос — оператор должен иметь
// возможность остановить долгую операцию в любой момент, а не только
// дождаться конца). found=false, если для этого прибора сейчас ничего
// не выполняется — не ошибка, просто нечего отменять.
func (s *Server) handleReloadCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	var body struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}

	s.reloadJobsMu.Lock()
	job, found := s.reloadJobs[body.DeviceID]
	var alreadyFinished bool
	if found {
		alreadyFinished = job.Finished
	}
	s.reloadJobsMu.Unlock()

	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	if alreadyFinished {
		writeJSON(w, http.StatusOK, map[string]any{"found": true, "already_finished": true})
		return
	}
	job.cancel()
	writeJSON(w, http.StatusOK, map[string]any{"found": true, "cancelling": true})
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

// handleSyncNow — POST /api/es-sync/trigger: кнопка «Синхронизировать
// сейчас» на вкладке «Подключение к ЭС» — просит уже работающий цикл
// es-sync конкретного прибора сделать внеплановый проход немедленно, не
// дожидаясь часового тикера (добавлено 2026-08-27).
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	if s.onSyncNow == nil {
		writeError(w, http.StatusServiceUnavailable, "синхронизация недоступна в этом режиме запуска")
		return
	}
	var body struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if err := s.onSyncNow(body.DeviceID); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
