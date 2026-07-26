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
}

func NewServer(repo storage.Repo, port int) *Server {
	return &Server{repo: repo, port: port}
}

func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", s.handleAPI)
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
loadData();
setInterval(loadData, 3000);
</script>
</body>
</html>`)
}
