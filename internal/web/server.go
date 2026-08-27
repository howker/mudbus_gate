package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	sqliterepo "mbgw/internal/storage/sqlite"
)

// Server's repo field is *sqliterepo.Repo (a concrete type), not the
// storage.Repo interface it used to hold. WHY: the device-config API
// below (devices/channels/es-connection/akron-northbound) needs the new
// methods added in repo_device_config.go (UpsertDevice, ListDevices,
// SetVKMChannels, ...), which are NOT part of storage.Repo — and
// storage.go is treated as a stable contract file here, not something to
// widen casually. Every actual caller of NewServer (run.go, serve.go,
// cmd/mbgw/server.go) already constructs a *sqliterepo.Repo via
// sqliterepo.New(...) and was only passing it through the interface, so
// this is a compile-compatible narrowing at the call sites — no caller
// needs to change.
type Server struct {
	repo *sqliterepo.Repo
	port int
	// onManualPoll, when set, is invoked by POST /api/poll — the
	// dashboard's "Опросить сейчас" button and the equivalent curl call.
	// It triggers an immediate operator-requested poll (archive + current)
	// on the live process without a restart. nil = the endpoint reports
	// that manual polling isn't wired (e.g. a context that only serves the
	// dashboard read-only), rather than panicking.
	onManualPoll func()

	// onForceReload, если задан, вызывается для принудительного
	// переопроса архива с указанного периода — см. api_reload.go и
	// SetForceReload. nil означает, что этот режим запуска не умеет
	// принудительно переопрашивать (например, старый диагностический
	// дашборд без полного набора приборов).
	//
	// onProgress (последний параметр) вызывается изнутри переопроса по
	// мере обработки — используется для заполнения reloadJobs (ниже),
	// который опрашивает браузер, чтобы показывать реальный прогресс
	// длительной операции (добавлено 2026-08-27 — раньше оператор видел
	// полную тишину на много минут без единого признака, что вообще
	// происходит).
	onForceReload func(deviceID string, from, to time.Time, onProgress func(done, total int)) (int, error)

	// reloadJobs хранит текущее состояние фоновых переопросов, по одному
	// на прибор — POST /api/devices/reload-archive запускает переопрос в
	// фоновой горутине и сразу возвращает управление, а браузер опрашивает
	// GET /api/devices/reload-progress каждые полторы секунды, пока не
	// увидит finished=true.
	reloadJobsMu sync.Mutex
	reloadJobs   map[string]*reloadJob

	// mu protects httpSrv/mux/port for Rebind — called from an HTTP
	// handler goroutine (settings save), while Start's own goroutine also
	// touches httpSrv. Без этого — гонка данных.
	mu      sync.Mutex
	httpSrv *http.Server
	mux     *http.ServeMux
}

func NewServer(repo *sqliterepo.Repo, port int) *Server {
	return &Server{repo: repo, port: port, reloadJobs: make(map[string]*reloadJob)}
}

// reloadJob — состояние одного фонового переопроса, по одному на прибор
// (новый запуск переопроса для того же прибора просто заменяет старую
// запись — параллельных переопросов одного прибора всё равно быть не
// должно, у него один физический канал связи).
type reloadJob struct {
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	Saved    int    `json:"saved"`
	Finished bool   `json:"finished"`
	Error    string `json:"error,omitempty"`
}

// SetManualPoll wires the operator "poll now" action. Called by run.go /
// serve.go after the scheduler and devices exist. Kept separate from
// NewServer so the web package doesn't need to import scheduler/device.
func (s *Server) SetManualPoll(fn func()) {
	s.onManualPoll = fn
}

// SetForceReload подключает возможность принудительного переопроса
// архива — вызывается из cmd/mbgw/server.go после того, как приборы
// созданы и открыты (см. runServer). Работает для обоих типов приборов
// (Akron и ВКМ) — какой именно метод вызывать, решает сам callback по
// типу конкретного прибора.
func (s *Server) SetForceReload(fn func(deviceID string, from, to time.Time, onProgress func(done, total int)) (int, error)) {
	s.onForceReload = fn
}

func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", s.handleAPI)
	mux.HandleFunc("/api/poll", s.handlePoll)

	// Device configuration API (T14 minimal slice, step 3). See
	// api_devices.go for handler bodies — kept in a separate file so this
	// file stays focused on the pre-existing diagnostic dashboard.
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/devices/delete", s.handleDeviceDelete)
	mux.HandleFunc("/api/devices/probe", s.handleDeviceProbe)
	mux.HandleFunc("/api/vkm-channels", s.handleVKMChannels)
	mux.HandleFunc("/api/vkm-channels/check-history", s.handleCheckChannelHistory)
	mux.HandleFunc("/api/es-connection", s.handleESConnection)
	mux.HandleFunc("/api/es-connection/test", s.handleESConnectionTest)
	mux.HandleFunc("/api/akron-northbound", s.handleAkronNorthbound)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/archive", s.handleArchive)
	mux.HandleFunc("/api/archive/export", s.handleArchiveExport)
	mux.HandleFunc("/api/devices/reload-archive", s.handleForceReload)
	mux.HandleFunc("/api/devices/reload-progress", s.handleReloadProgress)
	mux.HandleFunc("/admin", s.handleAdminUI)

	mux.HandleFunc("/", s.handleDashboard)

	s.mu.Lock()
	s.mux = mux
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	srv := &http.Server{Addr: addr, Handler: mux}
	s.httpSrv = srv
	s.mu.Unlock()

	go func() {
		log.Printf("[WEB] сервер диагностики запущен на http://%s\n", addr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[WEB] ошибка: %v\n", err)
		}
	}()

	<-ctx.Done()
	s.mu.Lock()
	current := s.httpSrv
	s.mu.Unlock()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = current.Shutdown(shutdownCtx)
}

// Rebind РїРµСЂРµРєР»СЋС‡Р°РµС‚ РІРµР±-СЃРµСЂРІРµСЂ РЅР° РЅРѕРІС‹Р№ РїРѕСЂС‚ Р’РќРЈРўР Р СЂР°Р±РѕС‚Р°СЋС‰РµРіРѕ
// процесса, без перезапуска всего mbgw.exe — вызывается из обработчика
// сохранения настроек (POST /api/settings), когда оператор меняет порт
// через UI. Сначала открывает слушатель на НОВОМ порту (если порт занят
// — Rebind вернёт ошибку, ничего не сломав), и только потом закрывает
// старый — так что если новый порт недоступен, старое соединение с UI
// не обрывается, оператор просто увидит ошибку сохранения.
func (s *Server) Rebind(newPort int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", newPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("порт %d недоступен: %w", newPort, err)
	}

	s.mu.Lock()
	newSrv := &http.Server{Addr: addr, Handler: s.mux}
	oldSrv := s.httpSrv
	s.httpSrv = newSrv
	s.port = newPort
	s.mu.Unlock()

	go func() {
		log.Printf("[WEB] переключение на порт %d\n", newPort)
		if err := newSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[WEB] ошибка после переключения порта: %v\n", err)
		}
	}()

	if oldSrv != nil {
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = oldSrv.Shutdown(shutdownCtx)
		}()
	}
	return nil
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// Запрет кеширования — та же причина, что и в writeJSON
	// (api_devices.go): без него браузер мог отдавать устаревший ответ
	// на повторный запрос того же URL, что и объясняет жалобу на
	// «несвежие» текущие данные (2026-08-23).
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	deviceID := r.URL.Query().Get("device_id")
	readings, _ := s.repo.GetLatestReadings(r.Context(), deviceID)
	_ = json.NewEncoder(w).Encode(readings)
}

// handlePoll triggers an immediate operator-requested poll (archive +
// current) on all devices. Wired to the dashboard "Опросить сейчас" button
// and callable directly, e.g. from PowerShell 2.0 on the сам server:
//
//	(New-Object System.Net.WebClient).UploadString('http://127.0.0.1:8080/api/poll','POST','')
//
// Accepts POST only (a plain browser GET must not trigger device I/O).
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "только POST"})
		return
	}
	if s.onManualPoll == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "ручной опрос не подключён в этом режиме запуска"})
		return
	}
	s.onManualPoll()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "опрос запущен (результат появится через несколько секунд)"})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// NOTE on browser compatibility: the target deployment platform
	// (Windows Server 2008 R2 / 2012, per FINAL_TRD.md section 2) only
	// reliably ships Internet Explorer, which implements NEITHER the
	// Fetch API NOR async/await (no IE version ever added them — this
	// is a hard platform fact, not a version-specific quirk).
	// Fix: plain ES5 — XMLHttpRequest instead of fetch, a callback
	// instead of async/await, string concatenation instead of template
	// literals, `var` instead of let/const/arrow functions. This runs in
	// IE8+ as well as every modern browser, so nothing is lost for
	// operators who do have a modern browser available.
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>mbgw Diagnostic</title>
<style>
body {
font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
padding: 30px;
background: #1e1e1e;
color: #cccccc;
}
h2 { color: #ffffff; font-weight: 500; border-bottom: 1px solid #3e3e42; padding-bottom: 10px; }
table { border-collapse: collapse; width: 100%; max-width: 1000px; margin-top: 15px; background: #252526; }
th, td { border: 1px solid #3e3e42; padding: 12px; text-align: left; }
th { background: #333337; color: #ffffff; font-weight: 600; text-transform: uppercase; font-size: 13px; }
.status-good { color: #4caf50; font-weight: bold; }
.status-bad { color: #f44336; font-weight: bold; }
.time-text { color: #858585; font-size: 13px; margin-top: 15px; }
</style>
</head>
<body>
<h2>MBGW Diagnostic Dashboard</h2>
<p>
<button id="pollBtn" onclick="pollNow()" style="font-size:14px;padding:8px 16px;background:#0e639c;color:#fff;border:none;cursor:pointer;">Опросить сейчас (архив + текущие)</button>
<span id="pollStatus" style="margin-left:12px;color:#858585;"></span>
</p>
<table>
<thead><tr><th>Device ID</th><th>Point ID</th><th>Instance</th><th>Value</th><th>Unit</th><th>Quality</th><th>Time</th></tr></thead>
<tbody id="data"><tr><td colspan="7" style="text-align: center; padding: 20px;">Загрузка данных...</td></tr></tbody>
</table>
<p class="time-text">Last Update: <span id="time">-</span></p>

<script>
function loadData() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/current', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) {
      if (window.console) { console.log('Fetch error: HTTP ' + xhr.status); }
      return;
    }
    var data;
    try {
      data = JSON.parse(xhr.responseText);
    } catch (e) {
      if (window.console) { console.log('Parse error: ' + e); }
      return;
    }
    if (!data || !data.length) { return; }

    var rows = '';
    for (var i = 0; i < data.length; i++) {
      var r = data[i];
      var t = r.Timestamp ? new Date(r.Timestamp).toLocaleTimeString('ru-RU') : '-';
      var qClass = r.Quality === 'VALID' ? 'status-good' : 'status-bad';
      var value = (r.Value === null || r.Value === undefined) ? '' : r.Value;

      rows += '<tr><td><b>' + r.DeviceID + '</b></td><td>' + r.PointID + '</td><td>' +
        r.Instance + '</td><td style="font-family: monospace; font-size: 15px;">' +
        value + '</td><td>' + r.Unit + '</td><td class="' + qClass + '">' +
        r.Quality + '</td><td>' + t + '</td></tr>';
    }
    document.getElementById('data').innerHTML = rows;

    var now = new Date();
    document.getElementById('time').innerText = now.toLocaleTimeString('ru-RU');
  };
  xhr.send();
}
function pollNow() {
  var btn = document.getElementById('pollBtn');
  var status = document.getElementById('pollStatus');
  btn.disabled = true;
  status.innerText = 'Опрос запущен...';
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/poll', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    btn.disabled = false;
    if (xhr.status === 200) {
      status.innerText = 'Опрос запущен (данные обновятся через несколько секунд)';
      setTimeout(loadData, 4000);
    } else {
      status.innerText = 'Ошибка: HTTP ' + xhr.status;
    }
  };
  xhr.send('');
}
loadData();
setInterval(loadData, 30000);
</script>
</body>
</html>`)
}
