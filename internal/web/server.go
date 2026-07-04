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
<tbody id="data"><tr><td colspan="7" style="text-align: center; padding: 20px;">жидание данных...</td></tr></tbody>
</table>
<p class="time-text">Last Update: <span id="time">-</span></p>

<script>
async function loadData() {
try {
let res = await fetch('/api/current');
let data = await res.json();
if (!data || !data.length) return;

let rows = '';
data.forEach(r => {
let t = r.Timestamp ? new Date(r.Timestamp).toLocaleTimeString('ru-RU') : '-';
let qClass = r.Quality === 'GOOD' ? 'status-good' : 'status-bad';
let value = (r.Value === null || r.Value === undefined) ? '' : r.Value;

rows += '<tr><td><b>'+r.DeviceID+'</b></td><td>'+r.PointID+'</td><td>'+r.Instance+'</td><td style="font-family: monospace; font-size: 15px;">'+value+'</td><td>'+r.Unit+'</td><td class="'+qClass+'">'+r.Quality+'</td><td>'+t+'</td></tr>';
});
document.getElementById('data').innerHTML = rows;

let now = new Date();
document.getElementById('time').innerText = now.toLocaleTimeString('ru-RU');
} catch(e) { console.log("Fetch error:", e); }
}
loadData();
setInterval(loadData, 3000);
</script>
</body>
</html>`)
}
