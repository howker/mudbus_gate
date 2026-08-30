// Package logbuf holds the last several thousand lines the process has
// logged, in memory — for the «Лог» tab in /admin (добавлено 2026-08-30,
// прямой запрос оператора: "добавить в юай вкладку где будет крутится
// лог нашего сервера опроса как он сейчас в окне крутится"). Purely an
// in-memory ring buffer, NOT persisted — surviving a restart isn't the
// goal (the real, complete history is in mbgw_server.log itself, see
// rotating_log.go), this is just a live view for an operator who'd
// otherwise have to keep a separate console window open next to the
// browser.
//
// A separate package (rather than living in cmd/mbgw or internal/web)
// exists for the same reason as internal/devicestatus: the WRITER is
// wired into log.SetOutput in cmd/mbgw/server.go (a third io.Writer
// alongside stdout and the rotating file), while the READER is
// internal/web's new /api/log handler — neither of those two packages
// may import the other, so the shared buffer needs a home both can
// import without a cycle.
package logbuf

import (
	"strings"
	"sync"
)

// maxLines caps how many recent lines are retained — old lines are
// dropped from the front once exceeded. 5000 lines is generously more
// than an operator would ever want to scroll through in the browser
// (the actual complete history remains in the log file on disk).
const maxLines = 5000

// Entry is one retained log line, tagged with a monotonically
// increasing Seq so a client can ask "give me everything after Seq N"
// (see Since) instead of re-fetching everything on every poll.
type Entry struct {
	Seq  int64
	Text string
}

var (
	mu      sync.Mutex
	entries []Entry
	nextSeq int64 = 1
)

// Writer is an io.Writer meant to be one leg of an io.MultiWriter
// alongside os.Stdout and the rotating log file (see cmd/mbgw/server.go:
// multiWriter := io.MultiWriter(os.Stdout, logWriter, logbuf.Writer{})).
// Splits each Write call on newlines and appends each resulting line as
// its own Entry — the standard log package always calls Write once per
// formatted line, but splitting defensively here means this doesn't
// silently misbehave if that ever changes or another writer sends a
// multi-line chunk.
type Writer struct{}

func (Writer) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()

	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}
	for _, line := range strings.Split(text, "\n") {
		entries = append(entries, Entry{Seq: nextSeq, Text: line})
		nextSeq++
	}
	if len(entries) > maxLines {
		entries = entries[len(entries)-maxLines:]
	}
	return len(p), nil
}

// Since returns every retained entry with Seq strictly greater than
// afterSeq (afterSeq<=0 means "everything currently retained" — used
// for the tab's very first load), plus the latest Seq present (0 if the
// buffer is still empty) so the caller knows what afterSeq to pass on
// its next poll even if nothing new arrived this time.
func Since(afterSeq int64) ([]Entry, int64) {
	mu.Lock()
	defer mu.Unlock()

	latest := int64(0)
	if len(entries) > 0 {
		latest = entries[len(entries)-1].Seq
	}

	if afterSeq <= 0 {
		out := make([]Entry, len(entries))
		copy(out, entries)
		return out, latest
	}

	// entries is always append-only with strictly increasing Seq, so a
	// linear scan from the front is simplest; at maxLines=5000 this is
	// cheap enough that a binary search would be unwarranted complexity
	// for a tab that's polled once every 2 seconds by, realistically,
	// one operator at a time.
	var out []Entry
	for _, e := range entries {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, latest
}
