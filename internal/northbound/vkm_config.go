package northbound

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VKM carrier tunables, read from a plain key=value text file next to the
// exe on the Энергосфера server. The whole point is to iterate on the live
// system WITHOUT rebuilding/re-uploading the binary: edit the file, and the
// change is picked up on the next request (the file is re-read on a short
// interval, so usually not even a carrier restart is needed).
//
// File format (one per line, '#' comments and blank lines ignored):
//
//	year_mode = full        # full | y2000 | y1900  — clock year register encoding
//	clock_offset_sec = 0    # shift reported device clock by ±N seconds
//	min_ready_ms = 0        # keep status "collecting" for at least N ms
//	                        # after a request before reporting "ready"
//	period_format = datetime  # datetime | unix — period rendering (opts bit3)
//	time_layout = 02.01.2006 15:04:05   # timestamp format inside the string
//	archive_string = ...    # raw override; leave unset to auto-build a
//	                        # spec-compliant string (period + parameters)
//
// Missing file or missing keys → documented defaults, so the carrier runs
// fine with no config file at all.
const vkmConfigFileName = "vkm_config.txt"

type vkmConfig struct {
	yearMode      string
	clockOffset   time.Duration
	minReady      time.Duration
	archiveString string
	timeLayout    string
	periodFormat  string
}

func defaultVKMConfig() vkmConfig {
	return vkmConfig{
		yearMode:    "full",
		clockOffset: 0,
		minReady:    0,
		// Empty by default so a caller-supplied string (CLI --vkm-string,
		// or ConfigArchiveSource.Fallback) is used unless the config file
		// explicitly overrides it.
		archiveString: "",
		periodFormat:  "datetime",
	}
}

var (
	vkmCfgMu      sync.Mutex
	vkmCfgPath    = vkmConfigFileName // overridable in tests
	vkmCfgCache   vkmConfig
	vkmCfgLoaded  bool
	vkmCfgModTime time.Time
	vkmCfgChecked time.Time
)

// loadVKMConfig returns the current config, re-reading the file at most once
// per second and only when its modification time changed. Any parse/read
// error falls back to the last good config (or defaults), never crashes the
// carrier — a typo in the file must not take polling down.
func loadVKMConfig() vkmConfig {
	vkmCfgMu.Lock()
	defer vkmCfgMu.Unlock()

	now := time.Now()
	if vkmCfgLoaded && now.Sub(vkmCfgChecked) < time.Second {
		return vkmCfgCache
	}
	vkmCfgChecked = now

	info, err := os.Stat(vkmCfgPath)
	if err != nil {
		if !vkmCfgLoaded {
			vkmCfgCache = defaultVKMConfig()
			vkmCfgLoaded = true
		}
		return vkmCfgCache
	}
	if vkmCfgLoaded && info.ModTime().Equal(vkmCfgModTime) {
		return vkmCfgCache
	}

	data, err := os.ReadFile(vkmCfgPath)
	if err != nil {
		if !vkmCfgLoaded {
			vkmCfgCache = defaultVKMConfig()
			vkmCfgLoaded = true
		}
		return vkmCfgCache
	}

	vkmCfgCache = parseVKMConfig(string(data))
	vkmCfgModTime = info.ModTime()
	vkmCfgLoaded = true
	return vkmCfgCache
}

func parseVKMConfig(text string) vkmConfig {
	// Strip a leading UTF-8 BOM: Windows PowerShell 5.1's
	// `Set-Content -Encoding UTF8` writes one by default, which — left
	// in place — glues onto the FIRST line's key (e.g. "\ufeffyear_mode")
	// and silently fails to match, reverting that one setting to its
	// default. Found live: re-saving vkm_config.txt as UTF-8 to fix
	// Cyrillic encoding in archive_string silently disabled year_mode
	// (first line in the file), which brought back the multi-billion-
	// second clock skew that halts ЭС polling entirely (24.07.2026).
	text = strings.TrimPrefix(text, "\uFEFF")

	cfg := defaultVKMConfig()
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// strip trailing inline comment for numeric/enum keys (but NOT for
		// archive_string, where '#' could be legitimate content)
		switch key {
		case "year_mode":
			if v := stripInlineComment(val); v == "full" || v == "y2000" || v == "y1900" {
				cfg.yearMode = v
			}
		case "clock_offset_sec":
			if n, err := strconv.Atoi(stripInlineComment(val)); err == nil {
				cfg.clockOffset = time.Duration(n) * time.Second
			}
		case "min_ready_ms":
			if n, err := strconv.Atoi(stripInlineComment(val)); err == nil && n >= 0 {
				cfg.minReady = time.Duration(n) * time.Millisecond
			}
		case "archive_string":
			cfg.archiveString = val
		case "period_format":
			// How the period parameter renders when opts bit3 is set:
			// "datetime" (second-precision timestamps) or "unix" (epoch
			// seconds). See vkmPeriodValue.
			if v := stripInlineComment(val); v == "datetime" || v == "unix" {
				cfg.periodFormat = v
			}
		case "time_layout":
			// Go reference-time layout for timestamps inside the archive
			// string, e.g. "02.01.2006 15:04:05" or "02.01.06 15:04".
			if v := strings.TrimSpace(val); v != "" {
				cfg.timeLayout = v
			}
		}
	}
	return cfg
}

func stripInlineComment(v string) string {
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ConfigArchiveSource serves the archive string from the config file,
// re-read on each request. This is the source used in production smoke
// runs so the string can be changed by EDITING A TEXT FILE on the
// Энергосфера server — no rebuild, no re-upload, no restart.
//
// It exists because the archive string's exact format (tags, header
// fields, units, separators) is the one thing we cannot derive without a
// live ВКМ or a vendor spec: the driver reads our string and rejects it,
// and each candidate format previously cost a full build-and-ship cycle
// over a slow link. With this source, trying a format is a text edit.
//
// A CLI-supplied string (--vkm-string) still wins when the config file has
// no archive_string key, so existing invocations keep working.
type ConfigArchiveSource struct {
	Fallback string // used when the config file specifies no archive_string
}

func (c ConfigArchiveSource) Archive(pipe int, start, end time.Time, opts uint16) (string, bool) {
	cfg := loadVKMConfig()

	// A literal archive_string in the config wins outright — an escape
	// hatch for trying a raw hand-crafted string on the live server.
	if cfg.archiveString != "" {
		return cfg.archiveString, true
	}
	if c.Fallback != "" {
		return c.Fallback, true
	}

	// Otherwise build a spec-compliant string (see BuildVKMArchiveString):
	// period first, then the data parameters. The period comes from the
	// window the master requested, so the record is timestamped with the
	// interval it actually asked for.
	if start.IsZero() {
		start = time.Now().Add(-time.Hour).Truncate(time.Hour)
	}
	if end.IsZero() {
		end = start.Add(time.Hour)
	}
	return BuildVKMArchiveString(start, end, cfg.timeLayout, defaultVKMParams(), opts), true
}