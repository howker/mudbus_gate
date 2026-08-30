package web

import (
	"net/http"
	"strconv"

	"mbgw/internal/logbuf"
)

// api_log.go implements the «Лог» tab in /admin (добавлено 2026-08-30,
// прямой запрос оператора: "добавить в юай вкладку где будет крутится
// лог нашего сервера опроса как он сейчас в окне крутится"). Two
// endpoints:
//
//   - GET /api/log?after=<seq> — polled every 2s by the tab (see
//     loadLog in api_admin_ui.go); returns only entries newer than
//     `after`, not the whole buffer every time. after=0 or omitted
//     means "everything currently retained" (first load).
//   - GET /api/log/download — a plain-text snapshot of the whole
//     retained buffer with Content-Disposition: attachment, so
//     "Сохранить в файл" is just a link (window.location.href), no
//     client-side Blob/File API needed — matches the rest of this
//     project's ES5/old-IE-compatible frontend approach (see
//     handleDashboard's doc comment in server.go).
type logEntryJSON struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	rows, latest := logbuf.Since(after)

	out := make([]logEntryJSON, 0, len(rows))
	for _, e := range rows {
		out = append(out, logEntryJSON{Seq: e.Seq, Text: e.Text})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":    out,
		"latest_seq": latest,
	})
}

func (s *Server) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	rows, _ := logbuf.Since(0)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mbgw_log_snapshot.txt"`)
	for _, e := range rows {
		_, _ = w.Write([]byte(e.Text))
		_, _ = w.Write([]byte("\r\n"))
	}
}
