package profile

import (
	"path/filepath"
	"runtime"
	"testing"
)

// repoPath resolves a path relative to the repository root, regardless of
// where `go test` is invoked from. This test file lives in
// internal/profile/, so the repo root is two directories up.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, filepath.FromSlash(rel))
}

// TestParseAcron01FromDisk loads the SHIPPING acron-01.yaml through the
// real Parse() path (YAML unmarshal + Validate). No existing test does
// this — the profile tests all use inline VKM fixtures — so a broken
// field in the real profile (e.g. a time point missing its type) would
// otherwise go unnoticed until runtime. This is the regression guard.
func TestParseAcron01FromDisk(t *testing.T) {
	path := repoPath(t, "profiles/acron-01.yaml")
	p, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(acron-01.yaml) failed: %v", err)
	}

	if p.Meta.Model == "" {
		t.Fatal("meta.model is empty")
	}

	// Every point must carry a valid type — this is exactly what catches
	// the "month" point if it was left without a type.
	for i, pt := range p.Points {
		if pt.Type == "" {
			t.Errorf("points[%d] %q: type is empty", i, pt.Name)
		}
	}

	// The hourly archive must exist and use the akron_archive strategy.
	var hourly *Archive
	for idx := range p.Archives {
		if p.Archives[idx].ID == "hourly" {
			hourly = &p.Archives[idx]
			break
		}
	}
	if hourly == nil {
		t.Fatal("no archive with id=hourly found")
	}
	if hourly.Strategy != "akron_archive" {
		t.Fatalf("hourly.strategy = %q, want akron_archive", hourly.Strategy)
	}
	if got := hourly.Params["archive_kind"]; got != "hourly" {
		t.Fatalf("hourly.params.archive_kind = %v, want \"hourly\"", got)
	}

	// The record layout must match the command-104 row format:
	//   volume(akron_volume)@0, then hour/day/month/year(bcd)@5..8.
	// This is what device.go relies on to reassemble each record's
	// timestamp and value, so pin it down here.
	want := []struct {
		offset int
		name   string
		typ    string
	}{
		{0, "volume", "akron_volume"},
		{5, "hour", "bcd"},
		{6, "day", "bcd"},
		{7, "month", "bcd"},
		{8, "year", "bcd"},
	}
	if len(hourly.RecordLayout) != len(want) {
		t.Fatalf("record_layout has %d fields, want %d", len(hourly.RecordLayout), len(want))
	}
	for i, w := range want {
		f := hourly.RecordLayout[i]
		if f.Offset != w.offset || f.Name != w.name || f.Type != w.typ {
			t.Errorf("record_layout[%d] = {offset:%d name:%q type:%q}, want {offset:%d name:%q type:%q}",
				i, f.Offset, f.Name, f.Type, w.offset, w.name, w.typ)
		}
	}
}
