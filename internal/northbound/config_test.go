package northbound

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUSPDConfig_Valid(t *testing.T) {
	yamlContent := `
id: mbgw-heat
device_id: acron-1
listen: ":1502"
unit_id: 1
interval: "60m"
points:
  - register: 0
    space: holding
    type: float
    order: "0123"
    source: "V"
  - register: 2
    space: holding
    type: float
    order: "0123"
    source: "Q"
    scale: 10
    bias: 1.5
`
	path := filepath.Join(t.TempDir(), "uspd.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	uspd, err := LoadUSPDConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if uspd.ID != "mbgw-heat" || uspd.DeviceID != "acron-1" || uspd.Listen != ":1502" || uspd.UnitID != 1 || uspd.Interval != "60m" {
		t.Fatalf("unexpected uspd: %+v", uspd)
	}
	if len(uspd.CurrentPoints) != 2 {
		t.Fatalf("expected 2 points, got %d", len(uspd.CurrentPoints))
	}
	q := uspd.CurrentPoints[1]
	if q.Register != 2 || q.Source != "Q" || q.Scale != 10 || q.Bias != 1.5 {
		t.Fatalf("unexpected point[1]: %+v", q)
	}
}

func TestLoadUSPDConfig_MissingListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uspd.yaml")
	os.WriteFile(path, []byte("device_id: acron-1\n"), 0644)

	if _, err := LoadUSPDConfig(path); err == nil {
		t.Fatal("expected error for missing listen, got nil")
	}
}

func TestLoadUSPDConfig_MissingDeviceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uspd.yaml")
	os.WriteFile(path, []byte("listen: \":1502\"\n"), 0644)

	if _, err := LoadUSPDConfig(path); err == nil {
		t.Fatal("expected error for missing device_id, got nil")
	}
}

func TestLoadUSPDConfig_PointMissingSource(t *testing.T) {
	yamlContent := `
device_id: acron-1
listen: ":1502"
points:
  - register: 0
    type: float
`
	path := filepath.Join(t.TempDir(), "uspd.yaml")
	os.WriteFile(path, []byte(yamlContent), 0644)

	if _, err := LoadUSPDConfig(path); err == nil {
		t.Fatal("expected error for point missing source, got nil")
	}
}

func TestLoadUSPDConfig_FileNotFound(t *testing.T) {
	if _, err := LoadUSPDConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
