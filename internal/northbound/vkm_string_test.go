package northbound

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Options observed from the LIVE Энергосфера УВП280А driver: tags +
// max-precision/seconds, NO headers (opts=12 in every captured 7900-7914
// request block, vkm_live.jsonl 24.07.2026).
const liveOpts uint16 = vkmOptTags | vkmOptMaxDigits

// TestBuildVKMArchiveString_LiveOptions pins the string shape for the
// options the live driver actually requests: tags without {headers}, and
// the period as seconds. Serving {headers} she never asked for was
// rejected as "Неверный формат пакета".
func TestBuildVKMArchiveString_LiveOptions(t *testing.T) {
	start := time.Date(2026, 7, 15, 16, 0, 0, 0, time.Local)
	end := start.Add(30 * time.Minute)
	got := BuildVKMArchiveString(start, end, "", defaultVKMParams(), liveOpts)

	if strings.Contains(got, "{") || strings.Contains(got, "}") {
		t.Fatalf("headers must be ABSENT when opts bit1 is clear: %q", got)
	}
	// Default period_format is "datetime": second-precision timestamps.
	wantPeriod := "T=15.07.2026 16:00:00-15.07.2026 16:30:00;"
	if !strings.HasPrefix(got, wantPeriod) {
		t.Fatalf("period must be first; got %q, want prefix %q", got, wantPeriod)
	}
	if !strings.Contains(got, "M=678.900000кг;") {
		t.Errorf("mass must use tag, max digits, unit without space: %q", got)
	}
	if got[len(got)-1] != 0 {
		t.Errorf("string must end with a NUL terminator")
	}
}

// TestBuildVKMArchiveString_HeadersAndHumanTime covers the opposite option
// set (headers on, human-readable time) to prove the shape tracks the
// option bits rather than being hard-coded.
func TestBuildVKMArchiveString_HeadersAndHumanTime(t *testing.T) {
	start := time.Date(2026, 7, 22, 16, 0, 0, 0, time.Local)
	end := start.Add(30 * time.Minute)
	opts := uint16(vkmOptTags | vkmOptHeaders)
	got := BuildVKMArchiveString(start, end, "", defaultVKMParams(), opts)

	if !strings.Contains(got, "T{Период}=22.07.2026 16:00:00-22.07.2026 16:30:00;") {
		t.Fatalf("expected headers + human-readable period, got %q", got)
	}
	if !strings.Contains(got, "M{Масса}=678.90кг;") {
		t.Errorf("expected header form with plain value: %q", got)
	}
}

func TestBuildVKMArchiveString_NoAbsTime(t *testing.T) {
	start := time.Date(2026, 7, 22, 16, 0, 0, 0, time.Local)
	end := start.Add(30 * time.Minute)
	got := BuildVKMArchiveString(start, end, "", defaultVKMParams(), vkmOptTags|vkmOptNoAbsTime)
	if strings.Contains(got, "T=") {
		t.Fatalf("opts bit4 must drop the period parameter: %q", got)
	}
}

func TestBuildVKMArchiveString_CustomTimeLayout(t *testing.T) {
	start := time.Date(2026, 7, 22, 16, 0, 0, 0, time.Local)
	end := start.Add(30 * time.Minute)
	got := BuildVKMArchiveString(start, end, "02.01.06 15:04", nil, vkmOptTags)
	if !strings.Contains(got, "22.07.26 16:00-22.07.26 16:30") {
		t.Fatalf("custom layout not applied: %q", got)
	}
}

// TestConfigArchiveSource_BuildsPeriodFromRequest verifies the served
// string describes the window and option set the master actually
// requested.
func TestConfigArchiveSource_BuildsPeriodFromRequest(t *testing.T) {
	withVKMConfig(t, "") // no archive_string override

	src := ConfigArchiveSource{}
	start := time.Date(2026, 7, 15, 16, 0, 0, 0, time.Local)
	end := time.Date(2026, 7, 15, 16, 30, 0, 0, time.Local)
	got, ok := src.Archive(1, start, end, liveOpts)
	if !ok {
		t.Fatal("expected a result")
	}
	wantPeriod := "T=15.07.2026 16:00:00-15.07.2026 16:30:00;"
	if !strings.HasPrefix(got, wantPeriod) {
		t.Fatalf("period does not match the request/options: %q, want prefix %q", got, wantPeriod)
	}
}

// TestBuildVKMArchiveString_UnixPeriodFormat covers the alternative reading
// of opts bit3, selectable from vkm_config.txt without a rebuild.
func TestBuildVKMArchiveString_UnixPeriodFormat(t *testing.T) {
	withVKMConfig(t, "period_format = unix\n")

	start := time.Date(2026, 7, 15, 16, 0, 0, 0, time.Local)
	end := start.Add(30 * time.Minute)
	got := BuildVKMArchiveString(start, end, "", defaultVKMParams(), liveOpts)

	wantPeriod := fmt.Sprintf("T=%d-%d;", start.Unix(), end.Unix())
	if !strings.HasPrefix(got, wantPeriod) {
		t.Fatalf("got %q, want prefix %q", got, wantPeriod)
	}
}
