package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"mbgw/internal/integration"
	sqliterepo "mbgw/internal/storage/sqlite"
)

// api_devices.go implements the device-configuration REST API (T14
// minimal-slice step 3): CRUD for devices, ВКМ→ЭС channel mappings, the
// single Энергосфера SQL Server connection (plus a live connection test),
// and the Akron northbound listen address. This is what the future Web UI
// (step 4) calls instead of the operator hand-editing config.yaml/
// es_sync.txt.
//
// Deliberately plain net/http + encoding/json, no router/framework — go
// 1.20's http.ServeMux (used everywhere else in this project, see
// Start() in server.go) doesn't support path parameters or method-based
// routing, so every handler below dispatches on r.Method itself and takes
// identifiers as either a query parameter or a JSON body field, never a
// path segment like /api/devices/{id}. This matches the plain-ES5/no-
// build-step frontend constraint (IE compatibility, see handleDashboard's
// doc comment) — no client-side router needed either.
//
// Every handler writes UTF-8 JSON and never panics on malformed input —
// decode errors return 400 with a message, not a 500.

// deviceJSON is the wire shape for GET/POST /api/devices — a flat,
// JSON-tagged mirror of sqliterepo.DeviceRecord (which has no JSON tags
// of its own, since it is also used internally by cmd/mbgw/server.go
// without ever being serialized there).
type deviceJSON struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Kind                  string `json:"kind"` // "vkm360" | "akron"
	Profile               string `json:"profile"`
	TransportKind         string `json:"transport_kind"`
	Host                  string `json:"host"`
	Port                  int    `json:"port"`
	COM                   string `json:"com"`
	Baudrate              int    `json:"baudrate"`
	Parity                string `json:"parity"`
	StopBits              int    `json:"stopbits"`
	TimeoutMs             int    `json:"timeout_ms"`
	UnitID                int    `json:"unit_id"`
	Retries               int    `json:"retries"`
	CurrentPollSeconds    int    `json:"current_poll_seconds"`
	BackfillMaxDepthHours int    `json:"backfill_max_depth_hours"`
	GapScanWindowHours    int    `json:"gap_scan_window_hours"`
	ArchiveAtMinute       int    `json:"archive_at_minute"` // -1 = unset/default
	Enabled               bool   `json:"enabled"`
	// Overwrite must be explicitly true to upsert over an ID that already
	// exists. Defense in depth against the 2026-08-23 incident (saving a
	// new device silently overwrote a different, already-saved one that
	// happened to auto-generate the same ID from a similar name) — the
	// Web UI already guards against this client-side (see saveDevice's
	// isNew/findDevice check in api_admin_ui.go), but a client-side check
	// alone can't catch a stale device list (e.g. two operators/tabs) or
	// a direct API call bypassing the UI entirely.
	Overwrite bool `json:"overwrite"`
}

func deviceToJSON(d sqliterepo.DeviceRecord) deviceJSON {
	return deviceJSON{
		ID: d.ID, Name: d.Name, Kind: d.Kind, Profile: d.Profile,
		TransportKind: d.TransportKind, Host: d.Host, Port: d.Port, COM: d.COM,
		Baudrate: d.Baudrate, Parity: d.Parity, StopBits: d.StopBits,
		TimeoutMs: d.TimeoutMs, UnitID: d.UnitID, Retries: d.Retries,
		CurrentPollSeconds: d.CurrentPollSeconds, BackfillMaxDepthHours: d.BackfillMaxDepthHours,
		GapScanWindowHours: d.GapScanWindowHours, ArchiveAtMinute: d.ArchiveAtMinute,
		Enabled: d.Enabled,
	}
}

func deviceFromJSON(j deviceJSON) sqliterepo.DeviceRecord {
	return sqliterepo.DeviceRecord{
		ID: j.ID, Name: j.Name, Kind: j.Kind, Profile: j.Profile,
		TransportKind: j.TransportKind, Host: j.Host, Port: j.Port, COM: j.COM,
		Baudrate: j.Baudrate, Parity: j.Parity, StopBits: j.StopBits,
		TimeoutMs: j.TimeoutMs, UnitID: j.UnitID, Retries: j.Retries,
		CurrentPollSeconds: j.CurrentPollSeconds, BackfillMaxDepthHours: j.BackfillMaxDepthHours,
		GapScanWindowHours: j.GapScanWindowHours, ArchiveAtMinute: j.ArchiveAtMinute,
		Enabled: j.Enabled,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// Запрет кеширования — ОБЯЗАТЕЛЕН для всех ответов API. Без него
	// браузер может закешировать GET-запрос (например, к /api/archive с
	// конкретными датами) и повторно отдать СТАРЫЙ ответ на идентичный
	// запрос даже после того, как логика на сервере изменилась — именно
	// это произошло 2026-08-23: запрос архива за «сегодня» уже
	// выполнялся ДО фикса расчёта расхода, браузер закешировал старый
	// (сырые показания) ответ и продолжал его отдавать; запрос за
	// «неделю» с другими датами кеша не имел и показал уже исправленные
	// данные. Раньше запрет кеша стоял только на HTML-странице /admin
	// (см. handleAdminUI) — этого было недостаточно, кешируются и сами
	// JSON-ответы API.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleDevices: GET lists every device; POST upserts one (JSON body =
// deviceJSON, ID required).
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.repo.ListDevices(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать список приборов: "+err.Error())
			return
		}
		out := make([]deviceJSON, 0, len(list))
		for _, d := range list {
			out = append(out, deviceToJSON(d))
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var j deviceJSON
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		if j.ID == "" {
			writeError(w, http.StatusBadRequest, "поле id обязательно")
			return
		}
		if j.Kind != "vkm360" && j.Kind != "akron" {
			writeError(w, http.StatusBadRequest, `поле kind должно быть "vkm360" или "akron"`)
			return
		}
		existing, existsAlready, err := s.repo.GetDevice(r.Context(), j.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось проверить существующий прибор: "+err.Error())
			return
		}
		if existsAlready && !j.Overwrite {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("прибор с ID %q уже существует (название: %q) — это другой прибор, а не редактирование; используйте другое название", j.ID, existing.Name),
			})
			return
		}
		if j.ArchiveAtMinute == 0 {
			// JSON omits ArchiveAtMinute entirely on a brand-new device
			// from a simple form (no explicit -1) -> zero value comes
			// through as 0, which would wrongly anchor to HH:00 instead
			// of using the default. Treat unset the same way the DB
			// schema's DEFAULT -1 does; a form that genuinely wants HH:00
			// must send -1... no: 0 IS a valid anchor minute (top of the
			// hour). To avoid this ambiguity the Web UI must always send
			// an explicit value (-1 for "use default"); this handler does
			// not guess. Left as 0 here deliberately - see the Web UI
			// step (step 4) for the actual default-filling behavior.
			_ = 0
		}
		if err := s.repo.UpsertDevice(r.Context(), deviceFromJSON(j)); err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить прибор: "+err.Error())
			return
		}
		// Пересохранение УЖЕ существующего прибора чистит его старые
		// текущие показания (readings_current) — иначе, если тип прибора
		// (или профиль) когда-либо менялся, показания с прошлыми именами
		// точек остаются висеть в базе рядом со свежими навсегда и
		// показываются на экране вперемешку (см. doc-комментарий
		// DeleteCurrentReadings). Для НОВОГО прибора чистить нечего —
		// показаний ещё не было.
		if existsAlready {
			if err := s.repo.DeleteCurrentReadings(r.Context(), j.ID); err != nil {
				log.Printf("[WEB] не удалось очистить старые показания прибора %s: %v\n", j.ID, err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}

// handleDeviceDelete: POST {"id": "..."} removes a device and its channel
// mappings. POST (not DELETE) so a plain ES5 XMLHttpRequest form submit
// works the same way as every other write endpoint here — see this
// file's package doc comment on why no method-based routing is used.
func (s *Server) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if body.ID == "" {
		writeError(w, http.StatusBadRequest, "поле id обязательно")
		return
	}
	if err := s.repo.DeleteDevice(r.Context(), body.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось удалить прибор: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// vkmChannelJSON is the wire shape for one tag->ЭС-channel mapping row.
type vkmChannelJSON struct {
	Tag         string  `json:"tag"` // "ST" | "S" | "T" | "Pi"
	ESChannelID int     `json:"es_channel_id"`
	Factor      float64 `json:"factor"`
}

// handleVKMChannels: GET ?device_id=xxx lists channel mappings; POST
// {"device_id": "...", "channels": [...]} replaces the whole set for that
// device (see SetVKMChannels's doc comment on why "replace whole set" is
// the right shape for a form that always submits all 4 tag rows at once).
func (s *Server) handleVKMChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		deviceID := r.URL.Query().Get("device_id")
		if deviceID == "" {
			writeError(w, http.StatusBadRequest, "параметр device_id обязателен")
			return
		}
		rows, err := s.repo.GetVKMChannels(r.Context(), deviceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать каналы: "+err.Error())
			return
		}
		out := make([]vkmChannelJSON, 0, len(rows))
		for _, c := range rows {
			out = append(out, vkmChannelJSON{Tag: c.Tag, ESChannelID: c.ESChannelID, Factor: c.Factor})
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var body struct {
			DeviceID string           `json:"device_id"`
			Channels []vkmChannelJSON `json:"channels"`
			Force    bool             `json:"force"` // явное подтверждение "да, я знаю про конфликт, сохранить всё равно"
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		if body.DeviceID == "" {
			writeError(w, http.StatusBadRequest, "поле device_id обязательно")
			return
		}
		rows := make([]sqliterepo.VKMChannelRecord, 0, len(body.Channels))
		channelIDs := make([]int, 0, len(body.Channels))
		for _, c := range body.Channels {
			factor := c.Factor
			if factor == 0 {
				factor = 1.0
			}
			rows = append(rows, sqliterepo.VKMChannelRecord{
				DeviceID: body.DeviceID, Tag: c.Tag, ESChannelID: c.ESChannelID, Factor: factor,
			})
			if c.ESChannelID != 0 {
				channelIDs = append(channelIDs, c.ESChannelID)
			}
		}

		// Защита от случайного ввода номера канала, который уже занят
		// ДРУГИМ прибором — если оба прибора начнут писать в один канал
		// ЭС, данные одного будут затирать данные другого, и заметить
		// это по внешним признакам не всегда просто. Force=true
		// позволяет явно подтвердить и сохранить всё равно (например,
		// если оператор осознанно переносит канал с одного прибора на
		// другой).
		if !body.Force {
			conflicts, err := s.repo.FindChannelConflicts(r.Context(), body.DeviceID, channelIDs)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "не удалось проверить занятость каналов: "+err.Error())
				return
			}
			if len(conflicts) > 0 {
				parts := make([]string, 0, len(conflicts))
				for ch, owner := range conflicts {
					parts = append(parts, fmt.Sprintf("канал %d уже занят прибором %q", ch, owner))
				}
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":     "Обнаружено совпадение номеров каналов с другим прибором: " + strings.Join(parts, "; "),
					"conflicts": conflicts,
				})
				return
			}
		}

		if err := s.repo.SetVKMChannels(r.Context(), body.DeviceID, rows); err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить каналы: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}

// esConnectionJSON deliberately OMITS the password on the way OUT (GET) —
// only PasswordSet (a boolean) tells the UI whether a password has been
// configured, so the browser never re-displays or re-transmits a secret
// it already knows. POST always requires a fresh Password value to save
// (the Web UI's connection form should treat "leave blank to keep
// existing" as its own concern, not this API's — kept simple here).
type esConnectionJSON struct {
	SQLServer        string `json:"sql_server"`
	SQLDatabase      string `json:"sql_database"`
	SQLUser          string `json:"sql_user"`
	SQLPort          int    `json:"sql_port"`
	PasswordSet      bool   `json:"password_set"`
	TimeShiftMinutes int    `json:"time_shift_minutes"`
}

func (s *Server) handleESConnection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		conn, found, err := s.repo.GetESConnection(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать настройки подключения: "+err.Error())
			return
		}
		if !found {
			writeJSON(w, http.StatusOK, esConnectionJSON{})
			return
		}
		writeJSON(w, http.StatusOK, esConnectionJSON{
			SQLServer: conn.SQLServer, SQLDatabase: conn.SQLDatabase,
			SQLUser: conn.SQLUser, SQLPort: conn.SQLPort,
			PasswordSet:      conn.SQLPassword != "",
			TimeShiftMinutes: conn.TimeShiftMinutes,
		})

	case http.MethodPost:
		var body struct {
			SQLServer        string `json:"sql_server"`
			SQLDatabase      string `json:"sql_database"`
			SQLUser          string `json:"sql_user"`
			SQLPassword      string `json:"sql_password"`
			SQLPort          int    `json:"sql_port"`
			TimeShiftMinutes int    `json:"time_shift_minutes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		if body.SQLServer == "" || body.SQLDatabase == "" || body.SQLUser == "" || body.SQLPassword == "" {
			writeError(w, http.StatusBadRequest, "поля sql_server, sql_database, sql_user, sql_password обязательны")
			return
		}
		port := body.SQLPort
		if port == 0 {
			port = 1433
		}
		err := s.repo.SetESConnection(r.Context(), sqliterepo.ESConnection{
			SQLServer: body.SQLServer, SQLDatabase: body.SQLDatabase,
			SQLUser: body.SQLUser, SQLPassword: body.SQLPassword, SQLPort: port,
			TimeShiftMinutes: body.TimeShiftMinutes,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить настройки подключения: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}

// handleESConnectionTest tries to connect and PING the given SQL Server
// parameters WITHOUT saving them — lets the operator verify a connection
// works before committing it. Accepts the same body shape as POST
// /api/es-connection (password required — testing an already-saved
// connection without re-entering the password is a future convenience,
// not implemented here to avoid re-reading and re-transmitting a secret
// unnecessarily).
func (s *Server) handleESConnectionTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	var body struct {
		SQLServer   string `json:"sql_server"`
		SQLDatabase string `json:"sql_database"`
		SQLUser     string `json:"sql_user"`
		SQLPassword string `json:"sql_password"`
		SQLPort     int    `json:"sql_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	// SQLDatabase НЕ обязателен здесь (в отличие от сохранения) — тест
	// подключения нужен ДО того, как оператор выбрал базу, именно чтобы
	// предложить ему список реальных баз вместо ручного ввода вслепую
	// (см. ListDatabases ниже). Подключение без указанной базы обычно
	// уходит на базу по умолчанию для этого логина — этого достаточно,
	// чтобы прочитать sys.databases.
	if body.SQLServer == "" || body.SQLUser == "" || body.SQLPassword == "" {
		writeError(w, http.StatusBadRequest, "поля sql_server, sql_user, sql_password обязательны")
		return
	}
	port := body.SQLPort
	if port == 0 {
		port = 1433
	}

	writer, err := integration.OpenMainsWriter(integration.SQLServerConfig{
		Server: body.SQLServer, Database: body.SQLDatabase,
		User: body.SQLUser, Password: body.SQLPassword, Port: port,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "не удалось создать подключение: " + err.Error()})
		return
	}
	defer writer.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := writer.Ping(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Список баз — необязательная часть ответа: если прочитать не
	// получилось (например, у логина нет прав на sys.databases), тест
	// подключения всё равно считается успешным, просто без выпадающего
	// списка — оператор допишет имя базы вручную.
	databases, err := writer.ListDatabases(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "databases": databases})
}

// handleAkronNorthbound: GET ?device_id=xxx returns the configured listen
// address; POST {"device_id": "...", "listen_addr": "..."} sets it.
func (s *Server) handleAkronNorthbound(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		deviceID := r.URL.Query().Get("device_id")
		if deviceID == "" {
			writeError(w, http.StatusBadRequest, "параметр device_id обязателен")
			return
		}
		addr, found, err := s.repo.GetAkronNorthboundAddr(r.Context(), deviceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось прочитать адрес: "+err.Error())
			return
		}
		if !found {
			writeJSON(w, http.StatusOK, map[string]string{"listen_addr": ""})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"listen_addr": addr})

	case http.MethodPost:
		var body struct {
			DeviceID   string `json:"device_id"`
			ListenAddr string `json:"listen_addr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
			return
		}
		if body.DeviceID == "" || body.ListenAddr == "" {
			writeError(w, http.StatusBadRequest, "поля device_id и listen_addr обязательны")
			return
		}
		if err := s.repo.SetAkronNorthboundAddr(r.Context(), body.DeviceID, body.ListenAddr); err != nil {
			writeError(w, http.StatusInternalServerError, "не удалось сохранить адрес: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "используйте GET или POST")
	}
}
