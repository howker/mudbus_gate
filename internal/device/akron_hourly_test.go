package device

import (
	"testing"
	"time"
)

func TestAkronRowTime_Valid(t *testing.T) {
	// 16:00 on 17 July 2026 → year field carries 26 (year-2000).
	fields := map[string]any{
		"volume": float64(582.7),
		"hour":   int64(16),
		"day":    int64(17),
		"month":  int64(7),
		"year":   int64(26),
	}
	got, ok := akronRowTime(fields)
	if !ok {
		t.Fatal("akronRowTime returned ok=false for a valid row")
	}
	want := time.Date(2026, 7, 17, 16, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Minute/second must be zeroed to the hour boundary.
	if got.Minute() != 0 || got.Second() != 0 {
		t.Fatalf("hour boundary not clean: %v", got)
	}
}

func TestAkronRowTime_RejectsGarbage(t *testing.T) {
	cases := map[string]map[string]any{
		"all-zero filler": {"hour": int64(0), "day": int64(0), "month": int64(0), "year": int64(0)},
		"month 13":        {"hour": int64(1), "day": int64(1), "month": int64(13), "year": int64(24)},
		"hour 24":         {"hour": int64(24), "day": int64(1), "month": int64(1), "year": int64(24)},
		"day 0":           {"hour": int64(1), "day": int64(0), "month": int64(1), "year": int64(24)},
		"missing hour":    {"day": int64(1), "month": int64(1), "year": int64(24)},
		"wrong type":      {"hour": "16", "day": int64(1), "month": int64(1), "year": int64(24)},
	}
	for name, fields := range cases {
		if _, ok := akronRowTime(fields); ok {
			t.Errorf("%s: expected ok=false, got ok=true", name)
		}
	}
}

func TestFieldFloat_Types(t *testing.T) {
	if v, ok := fieldFloat(map[string]any{"volume": float64(582.7)}, "volume"); !ok || v != 582.7 {
		t.Fatalf("float64 volume: got %v ok=%v", v, ok)
	}
	// akron_volume always decodes to float64, but be defensive about int64.
	if v, ok := fieldFloat(map[string]any{"volume": int64(583)}, "volume"); !ok || v != 583 {
		t.Fatalf("int64 volume: got %v ok=%v", v, ok)
	}
	if _, ok := fieldFloat(map[string]any{"volume": "x"}, "volume"); ok {
		t.Fatal("string volume should be rejected")
	}
	if _, ok := fieldFloat(map[string]any{}, "volume"); ok {
		t.Fatal("missing volume should be rejected")
	}
}
