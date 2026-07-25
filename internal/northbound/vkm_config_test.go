package northbound

import "testing"

func TestParseVKMConfig(t *testing.T) {
	text := `
# time tuning for the live smoke
year_mode = y2000     # try 2-digit year
clock_offset_sec = -4 # nudge back 4s
archive_string = V01{Расход}=1.5 кг/с;
`
	cfg := parseVKMConfig(text)
	if cfg.yearMode != "y2000" {
		t.Errorf("yearMode = %q, want y2000", cfg.yearMode)
	}
	if cfg.clockOffset.Seconds() != -4 {
		t.Errorf("clockOffset = %v, want -4s", cfg.clockOffset)
	}
	if cfg.archiveString != "V01{Расход}=1.5 кг/с;" {
		t.Errorf("archiveString = %q", cfg.archiveString)
	}
}

func TestParseVKMConfig_DefaultsOnEmptyOrBad(t *testing.T) {
	cfg := parseVKMConfig("")
	def := defaultVKMConfig()
	if cfg.yearMode != def.yearMode || cfg.clockOffset != def.clockOffset {
		t.Fatalf("empty config should yield defaults, got %+v", cfg)
	}

	// Unknown year_mode value is ignored (keeps default); junk lines skipped.
	cfg = parseVKMConfig("year_mode = nonsense\ngarbage line\nclock_offset_sec = notanumber\n")
	if cfg.yearMode != "full" {
		t.Errorf("invalid year_mode should fall back to full, got %q", cfg.yearMode)
	}
	if cfg.clockOffset != 0 {
		t.Errorf("invalid offset should stay 0, got %v", cfg.clockOffset)
	}
}

// TestParseVKMConfig_StripsBOM reproduces the live regression: PowerShell's
// `Set-Content -Encoding UTF8` writes a UTF-8 BOM, which glued onto the
// first line's key and silently disabled year_mode, bringing back the
// clock skew that halts ЭС polling (24.07.2026).
func TestParseVKMConfig_StripsBOM(t *testing.T) {
	text := "\uFEFFyear_mode = y2000\nclock_offset_sec = 0\n"
	cfg := parseVKMConfig(text)
	if cfg.yearMode != "y2000" {
		t.Fatalf("BOM broke year_mode parsing: got %q, want y2000", cfg.yearMode)
	}
}
