package device

import (
	"testing"
	"time"

	"mbgw/internal/profile"
)

func TestFunc65StorageHour_PeriodEndMovesIVKTimestampToClosingBoundary(t *testing.T) {
	loc := time.FixedZone("UTC+4", 4*60*60)
	a := profile.Archive{Params: map[string]interface{}{"timestamp_semantics": "period_end"}}
	got := func65StorageHour(a, time.Date(2026, 9, 10, 2, 59, 59, 0, loc))
	want := time.Date(2026, 9, 10, 3, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFunc65StorageHour_ExactBoundaryIsNotMovedTwice(t *testing.T) {
	loc := time.FixedZone("UTC+4", 4*60*60)
	a := profile.Archive{Params: map[string]interface{}{"timestamp_semantics": "period_end"}}
	want := time.Date(2026, 9, 10, 3, 0, 0, 0, loc)
	if got := func65StorageHour(a, want); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFunc65StorageHour_DefaultKeepsLegacyTruncateSemantics(t *testing.T) {
	loc := time.FixedZone("UTC+4", 4*60*60)
	a := profile.Archive{Params: map[string]interface{}{}}
	got := func65StorageHour(a, time.Date(2026, 9, 10, 2, 59, 59, 0, loc))
	want := time.Date(2026, 9, 10, 2, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
