package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"mbgw/internal/storage"
)

type Server struct {
	repo storage.Repo
	port int
	// onManualPoll, when set, is invoked by POST /api/poll — the
	// dashboard's "Опросить сейчас" button and the equivalent curl call.
	// It triggers an immediate operator-requested poll (archive + current)
	// on the live process without a restart. nil = the endpoint reports
	// that manual polling isn't wired (e.g. a context that only serves the
	// dashboard read-only), rather than panicking.
	onManualPoll func()
}

func NewServer(repo storage.Repo, port int) *Server {
	return &Server{repo: repo, port: port}
}

// SetManualPoll wires the operator "poll now" action. Called by run.go /
// serve.go after the scheduler and devices exist. Kept separate from
// NewServer so the web package doesn't need to import scheduler/device.
func (s *Server) SetManualPoll(fn func()) {
	s.onManualPoll = fn
}

func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", s.handleAPI)
	mux.HandleFunc("/api/poll", s.handlePoll)
	mux.HandleFunc("/", s.handleDashboard)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("[WEB] сервер диагностики запущен на http://%s\n", addr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[WEB] ошибка: %v\n", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	deviceID := r.URL.Query().Get("device_id")
	readings, _ := s.repo.GetLatestReadings(r.Context(), deviceID)
	_ = json.NewEncoder(w).Encode(readings)
}

// handlePoll triggers an immediate operator-requested poll (archive +
// current) on all devices. Wired to the dashboard "Опросить сейчас" button
// and callable directly, e.g. from PowerShell 2.0 on the ЭС server:
//
//	(New-Object System.Net.WebClient).UploadString('http://127.0.0.1:8080/api/poll','POST','')
//
// Accepts POST only (a plain browser GET must not trigger device I/O).
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "используйте POST"})
		return
	}
	if s.onManualPoll == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "ручной опрос не подключён в этом режиме"})
		return
	}
	s.onManualPoll()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "опрос запрошен (архив + текущие)"})
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
	// is a hard platform fact, not a version-specific quirk). The
	// earlier version of this page used `async function` + `await
	// fetch(...)`, which IE silently fails to even parse/run, so the
	// page never got past "жидание данных..." despite the backend and
	// database working correctly (confirmed live: the API returned
	// valid JSON with real Akron readings via WebClient.DownloadString
	// while the page showed nothing in IE).
	//
	// Fix: plain ES5 — XMLHttpRequest instead of fetch, a callback
	// instead of async/await, string concatenation instead of template
	// literals (already the case before), `var` instead of
	// let/const/arrow functions. This runs in IE8+ as well as every
	// modern browser, so nothing is lost for operators who do have a
	// modern browser available.
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
<tbody id="data"><tr><td colspan="7" style="text-align: center; padding: 20px;">ожидание данных...</td></tr></tbody>
</table>
<p class="time-text">Last Update: <span id="time">-</span></p>

<script>
function loadData() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/current', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) {
      // eslint-disable-next-line no-console
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
  status.innerText = 'запрос отправлен...';
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/poll', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    btn.disabled = false;
    if (xhr.status === 200) {
      status.innerText = 'опрос запрошен — данные появятся в таблице через несколько секунд';
      setTimeout(loadData, 4000);
    } else {
      status.innerText = 'ошибка: HTTP ' + xhr.status;
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
