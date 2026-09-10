package web

import (
	"testing"
	"time"
)

func TestParseArchiveRangeBoundMinutePrecision(t *testing.T) {
	loc := time.FixedZone("test", 4*60*60)
	from, err := parseArchiveRangeBound("2026-09-10T12:00", false, loc)
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	to, err := parseArchiveRangeBound("2026-09-10T15:00", true, loc)
	if err != nil {
		t.Fatalf("to: %v", err)
	}
	wantFrom := time.Date(2026, 9, 10, 12, 0, 0, 0, loc)
	wantTo := time.Date(2026, 9, 10, 15, 0, 59, int(time.Second-time.Nanosecond), loc)
	if !from.Equal(wantFrom) {
		t.Fatalf("from=%v, want %v", from, wantFrom)
	}
	if !to.Equal(wantTo) {
		t.Fatalf("to=%v, want %v", to, wantTo)
	}
}

func TestParseArchiveRangeBoundKeepsDateOnlyCompatibility(t *testing.T) {
	loc := time.FixedZone("test", 4*60*60)
	from, err := parseArchiveRangeBound("2026-09-10", false, loc)
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	to, err := parseArchiveRangeBound("2026-09-10", true, loc)
	if err != nil {
		t.Fatalf("to: %v", err)
	}
	if from.Hour() != 0 || from.Minute() != 0 {
		t.Fatalf("date-only lower bound=%v, want start of day", from)
	}
	nextDay := time.Date(2026, 9, 11, 0, 0, 0, 0, loc)
	if !to.Equal(nextDay.Add(-time.Nanosecond)) {
		t.Fatalf("date-only upper bound=%v, want end of day", to)
	}
}
