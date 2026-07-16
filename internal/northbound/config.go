package northbound

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// pointConfigYAML/uspdConfigYAML mirror NorthPoint/NorthUSPD for YAML
// loading. YAML tags are kept out of the core contract types in regmap.go
// on purpose (TRD addendum §12 treats NorthPoint/NorthUSPD as a stable
// contract) — this file is the only place that knows about file format.
type pointConfigYAML struct {
	Register uint16  `yaml:"register"`
	Space    string  `yaml:"space"`
	Type     string  `yaml:"type"`
	Order    string  `yaml:"order"`
	Source   string  `yaml:"source"`
	Scale    float64 `yaml:"scale"`
	Bias     float64 `yaml:"bias"`
}

type uspdConfigYAML struct {
	ID       string            `yaml:"id"`
	DeviceID string            `yaml:"device_id"`
	Listen   string            `yaml:"listen"`
	UnitID   uint8             `yaml:"unit_id"`
	Interval string            `yaml:"interval"`
	Points   []pointConfigYAML `yaml:"points"`
}

// LoadUSPDConfig reads one logical UPD's northbound definition (listen
// address, unit id, and register map) from a YAML file.
func LoadUSPDConfig(path string) (NorthUSPD, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return NorthUSPD{}, fmt.Errorf("northbound: read config %s: %w", path, err)
	}

	var cfg uspdConfigYAML
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return NorthUSPD{}, fmt.Errorf("northbound: parse config %s: %w", path, err)
	}
	if cfg.Listen == "" {
		return NorthUSPD{}, fmt.Errorf("northbound: config %s: listen is required", path)
	}
	if cfg.DeviceID == "" {
		return NorthUSPD{}, fmt.Errorf("northbound: config %s: device_id is required", path)
	}

	points := make([]NorthPoint, 0, len(cfg.Points))
	for _, p := range cfg.Points {
		if p.Source == "" {
			return NorthUSPD{}, fmt.Errorf("northbound: config %s: point at register %d missing source", path, p.Register)
		}
		points = append(points, NorthPoint{
			Register: p.Register,
			Space:    p.Space,
			Type:     p.Type,
			Order:    p.Order,
			Source:   p.Source,
			Scale:    p.Scale,
			Bias:     p.Bias,
		})
	}

	return NorthUSPD{
		ID:            cfg.ID,
		DeviceID:      cfg.DeviceID,
		Listen:        cfg.Listen,
		UnitID:        cfg.UnitID,
		Interval:      cfg.Interval,
		CurrentPoints: points,
	}, nil
}
