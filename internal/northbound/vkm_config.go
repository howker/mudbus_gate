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
//	archive_string = V01{Расход}=123.45 кг/с;V02{Масса}=678.90 кг;
//
// Missing file or missing keys → documented defaults, so the carrier runs
// fine with no config file at all.
const vkmConfigFileName = "vkm_config.txt"

type vkmConfig struct {
	yearMode      string
	clockOffset   time.Duration
	minReady      time.Duration
	archiveString string
}

func defaultVKMConfig() vkmConfig {
	return vkmConfig{
		yearMode:      "full",
		clockOffset:   0,
		minReady:      0,
		archiveString: "V01{Расход}=123.45 кг/с;V02{Масса}=678.90 кг;",
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
