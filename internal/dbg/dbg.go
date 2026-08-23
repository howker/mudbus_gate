// Package dbg provides a process-wide on/off switch for VERBOSE
// diagnostic logging — specifically the raw request/response hex dumps
// added to internal/archive/akron_archive.go while chasing a data
// anomaly (2026-08-22): useful when actively diagnosing a problem, but
// noisy (and a modest performance/log-file-size cost) during normal
// operation. Regular operational log lines (device registered, sync
// summary, errors) are NOT gated by this — they always print via the
// standard log package, exactly as before. Only the verbose byte-level
// dumps route through dbg.Printf instead.
//
// Enabled is a plain bool, not behind a mutex/atomic: it is set ONCE at
// process startup (cmd/mbgw/server.go, before any polling goroutine
// starts) from the app_settings table, and never written again during
// the process's lifetime — changing it via the Web UI takes effect on
// the next restart, matching how every other Web UI-configured setting
// in this project already works (see repo_device_config.go's doc comment
// on why devices are read once at startup, not hot-reloaded).
package dbg

import "log"

// Enabled controls whether dbg.Printf actually writes anything. Default
// false (quiet) — matches the pre-existing behavior of the raw-byte
// diagnostic logging when it was unconditionally on, since an operator
// who hasn't visited Settings yet should not suddenly get a noisier log
// than they're used to relative to before this toggle existed... note:
// the byte dumps were ADDED recently specifically to diagnose the Akron
// anomaly and were always-on before this toggle; defaulting to false here
// means a fresh install is quiet by default, and the operator opts into
// verbose logging only when actively troubleshooting — the intended,
// sustainable long-term behavior once the anomaly investigation is done.
var Enabled bool

// Printf writes a formatted log line ONLY if Enabled is true. Same
// semantics as log.Printf otherwise (including the timestamp/prefix
// already configured on the standard logger by cmd/mbgw's startup code).
func Printf(format string, args ...any) {
	if !Enabled {
		return
	}
	log.Printf(format, args...)
}
