// Package devicestatus holds small, process-lifetime, in-memory runtime
// facts about devices that don't belong in the SQLite config/archive
// schema (repo_device_config.go, archive_types.go) — they're not
// configuration, and they're not archived history, just "what did we
// last observe, live" — closer in spirit to reloadJobs in
// internal/web/server.go than to anything persisted.
//
// A separate package (rather than living directly in internal/device or
// internal/web) exists specifically to avoid an import cycle: the WRITER
// of this data is internal/device (vkm_hourly.go, where the device's own
// reported archive-period timestamp is parsed out of the raw response),
// while the READER is internal/web (the new dashboard tab, added
// 2026-08-29 per direct operator request — "мы никак не отслеживаем
// какое время сейчас в приборе"). Neither of those two packages may
// import the other, so the shared state needs a home both can import
// without either depending on the other.
package devicestatus

import (
	"sync"
	"time"
)

// TimeDrift is one device's most recently observed clock discrepancy
// against this server's own system clock — computed by comparing the
// END-of-period timestamp the DEVICE ITSELF reports in its archive
// response against the period boundary WE requested (which is derived
// from this server's own clock, see collectVKMPeriod in
// internal/device/vkm_hourly.go).
//
// Read-only monitoring, not correction: ВКМ-360 has no confirmed,
// working Modbus register for WRITING its clock (checked live,
// 2026-08-29, tools/vkmtimeprobe — even the read-only clock block
// documented for this device model, HR 1800-1805, doesn't respond on the
// real boylernaya_par unit; the UVP-280.01 NTP/time-correction register
// block, HR 1005-1009, doesn't either). This is the closest thing to
// "keeping an eye on it" that's actually achievable today — see it, log
// it, show it on the dashboard; correcting it would need either a
// firmware-specific undocumented command (unknown) or physical access to
// the meter.
type TimeDrift struct {
	// CheckedAt is when THIS measurement was taken (server's own clock) —
	// shown on the dashboard so the operator can tell a stale reading
	// (device stopped answering archive requests a while ago) from a
	// fresh one, rather than trusting a number that might be hours old.
	CheckedAt time.Time

	// DriftSeconds — positive means the device's own reported time is
	// AHEAD of what the server expected (device clock runs fast);
	// negative means BEHIND (device clock runs slow). Only meaningful
	// when Reliable is true.
	DriftSeconds float64

	// Reliable is false when the device's archive response used the
	// "raw seconds" Time= format instead of the normal calendar-date
	// format (see isVKMTimeAnomalous's doc comment in vkm_hourly.go for
	// the full history of that format) — those raw numbers are NOT
	// confirmed to be Unix epoch seconds in any known base, so treating
	// them as a real instant would silently invent a wrong drift value
	// instead of correctly reporting "can't tell right now". The
	// dashboard shows a neutral "не определено" for these, not a
	// (possibly wildly wrong) number.
	Reliable bool

	// Note is a short human-readable explanation, filled in when
	// Reliable is false (or on any other reason drift couldn't be
	// computed this time) — shown on the dashboard instead of a number.
	Note string
}

var (
	mu     sync.Mutex
	drifts = make(map[string]TimeDrift)
)

// Set records the latest TimeDrift observation for a device — called
// from internal/device/vkm_hourly.go's collectVKMPeriod after every
// successful archive read, whether or not the reading was Reliable (an
// unreliable/anomalous reading still overwrites a stale reliable one —
// intentional: it's a genuinely CURRENT statement of "can't tell right
// now", more honest than continuing to show old, possibly-outdated drift
// number from several polls ago).
func Set(deviceID string, d TimeDrift) {
	mu.Lock()
	defer mu.Unlock()
	drifts[deviceID] = d
}

// Get returns the last recorded TimeDrift for a device, if any —
// ok=false means this device has never had an archive period collected
// since this process started (a fresh restart, or a device that's never
// successfully polled its archive at all).
func Get(deviceID string) (TimeDrift, bool) {
	mu.Lock()
	defer mu.Unlock()
	d, ok := drifts[deviceID]
	return d, ok
}

// All returns a snapshot copy of every recorded TimeDrift, keyed by
// device id — used by the dashboard handler to build one response
// without a lock held per-device across the whole HTTP handler.
func All() map[string]TimeDrift {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]TimeDrift, len(drifts))
	for k, v := range drifts {
		out[k] = v
	}
	return out
}
