package device

import (
	"testing"
	"time"

	"mbgw/internal/archive"
)

func akronReloadRangeRecord(ts time.Time, value float64) archive.ArchiveRecord {
	return archive.ArchiveRecord{
		Fields: map[string]any{
			"volume": value,
			"hour":   int64(ts.Hour()),
			"day":    int64(ts.Day()),
			"month":  int64(ts.Month()),
			"year":   int64(ts.Year() % 100),
		},
	}
}

func TestFilterAkronReloadRangeHonorsBothBoundaries(t *testing.T) {
	base := time.Now().In(time.Local).Truncate(time.Hour).Add(-6 * time.Hour)
	records := []archive.ArchiveRecord{
		akronReloadRangeRecord(base, 100),
		akronReloadRangeRecord(base.Add(time.Hour), 101),
		akronReloadRangeRecord(base.Add(2*time.Hour), 102),
		akronReloadRangeRecord(base.Add(3*time.Hour), 103),
	}

	from := base.Add(time.Hour)
	to := base.Add(2 * time.Hour)
	got := filterAkronReloadRange(records, from, to)

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	first, ok := akronRowTime(got[0].Fields)
	if !ok || !first.Equal(from) {
		t.Fatalf("first timestamp = %v, ok=%v, want %v", first, ok, from)
	}
	second, ok := akronRowTime(got[1].Fields)
	if !ok || !second.Equal(to) {
		t.Fatalf("second timestamp = %v, ok=%v, want %v", second, ok, to)
	}
}
