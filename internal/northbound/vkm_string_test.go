package northbound

import (
	"strings"
	"testing"
	"time"
)

// TestBuildVKMArchiveString_MatchesSpec pins the three rules from the
// ЭЛЕМЕР-ВКМ-360 register map (p.7) that the earlier hand-written test
// string violated — and which are why Энергосфера read our string but
// never accepted the data.
func TestBuildVKMArchiveString_MatchesSpec(t *testing.T) {
	start := time.Date(2026, 7, 22, 16, 0, 0, 0, time.Local)
	end := time.Date(2026, 7, 22, 16, 30, 0, 0, time.Local)
	got := BuildVKMArchiveString(start, end, "", defaultVKMParams())

	// Rule 1: the FIRST parameter's value is the period — two timestamps
	// separated by "-", not a data value.
	first := strings.SplitN(got, ";", 2)[0]
	if !strings.Contains(first, "22.07.2026 16:00:00-22.07.2026 16:30:00") {
		t.Fatalf("first parameter must carry the period, got %q", first)
	}

	// Rule 2: unit follows the value with NO space.
	if !strings.Contains(got, "678.90кг") {
		t.Errorf("unit must follow value without a space; got %q", got)
	}
	if strings.Contains(got, "678.90 кг") {
		t.Errorf("found a space before the unit: %q", got)
	}

	// Rule 3: string terminates with a NUL byte.
	if got[len(got)-1] != 0 {
		t.Errorf("string must end with a NUL terminator")
	}

	// Parameters separated by ';'.
	if !strings.Contains(got, ";M{Масса}=") {
		t.Errorf("parameters must be ';'-separated with tag{header}=; got %q", got)
	}
}

func TestBuildVKMArchiveString_CustomTimeLayout(t *testing.T) {
	start := time.Date(2026, 7, 22, 16, 0, 0, 0, time.Local)
	end := start.Add(30 * time.Minute)
	got := BuildVKMArchiveString(start, end, "02.01.06 15:04", nil)
	if !strings.Contains(got, "22.07.26 16:00-22.07.26 16:30") {
		t.Fatalf("custom layout not applied: %q", got)
	}
}

// TestConfigArchiveSource_BuildsPeriodFromRequest verifies the served
// string describes the window the master actually requested, since that
// timestamp is what the driver files the record under.
func TestConfigArchiveSource_BuildsPeriodFromRequest(t *testing.T) {
	withVKMConfig(t, "") // no archive_string override

	src := ConfigArchiveSource{}
	start := time.Date(2026, 7, 15, 16, 0, 0, 0, time.Local)
	end := time.Date(2026, 7, 15, 16, 30, 0, 0, time.Local)
	got, ok := src.Archive(1, start, end)
	if !ok {
		t.Fatal("expected a result")
	}
	if !strings.Contains(got, "15.07.2026 16:00:00-15.07.2026 16:30:00") {
		t.Fatalf("period does not match the request: %q", got)
	}
}
