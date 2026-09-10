package device

import (
	"testing"
	"time"
)

func TestVZLETClockInstant_IVKTERTreatsEpochAsLocalWallClock(t *testing.T) {
	loc := time.FixedZone("UTC+4", 4*60*60)
	rawWall := time.Date(2026, 9, 10, 18, 42, 15, 0, time.UTC)

	got := vzletClockInstant(uint32(rawWall.Unix()), "IVK-TER", loc)
	want := time.Date(2026, 9, 10, 18, 42, 15, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("IVK-TER clock: got %v, want %v", got, want)
	}
	if got.Hour() != 18 || got.Location() != loc {
		t.Fatalf("IVK-TER wall clock was not preserved: got %v", got)
	}
}

func TestVZLETClockInstant_OtherProfilesKeepUnixUTC(t *testing.T) {
	raw := time.Date(2026, 9, 10, 18, 42, 15, 0, time.UTC)
	got := vzletClockInstant(uint32(raw.Unix()), "TSRV-024", time.FixedZone("UTC+4", 4*60*60))
	if !got.Equal(raw) || got.Location() != time.UTC {
		t.Fatalf("non-IVK clock semantics changed: got %v, want UTC %v", got, raw)
	}
}
