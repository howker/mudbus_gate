package northbound

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"mbgw/internal/simulator"
)

// akronSimYAML is the on-disk shape of the raw-discovery responder config.
// Kept separate from simulator.AkronSimConfig so the simulator package
// stays free of a YAML dependency and the file format can evolve
// independently of the runtime struct.
type akronSimYAML struct {
	Cmd110    string `yaml:"cmd110"`     // nil|empty|ready|zero|echo
	LiveClock *bool  `yaml:"live_clock"` // pointer: distinguish "unset" from "false"
	Identity  struct {
		DeviceType  *int `yaml:"device_type"`
		FirmwareBCD *int `yaml:"firmware_bcd"`
		Serial      *int `yaml:"serial"`
	} `yaml:"identity"`
	Overrides map[string]string `yaml:"overrides"` // decimal function code -> response PDU hex
}

// LoadAkronSimConfig reads the responder config from a YAML file and maps
// it onto simulator.AkronSimConfig. Missing fields keep their defaults.
func LoadAkronSimConfig(path string) (simulator.AkronSimConfig, error) {
	cfg := simulator.DefaultAkronSimConfig()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("northbound: read akron sim config %s: %w", path, err)
	}
	var y akronSimYAML
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return cfg, fmt.Errorf("northbound: parse akron sim config %s: %w", path, err)
	}

	if y.Cmd110 != "" {
		cfg.Cmd110 = y.Cmd110
	}
	if y.LiveClock != nil {
		cfg.LiveClock = *y.LiveClock
	}
	if y.Identity.DeviceType != nil {
		cfg.DeviceType = byte(*y.Identity.DeviceType)
	}
	if y.Identity.FirmwareBCD != nil {
		cfg.FirmwareBCD = byte(*y.Identity.FirmwareBCD)
	}
	if y.Identity.Serial != nil {
		cfg.SerialLENum = uint32(*y.Identity.Serial)
	}
	if len(y.Overrides) > 0 {
		cfg.Overrides = y.Overrides
	}

	return cfg, nil
}
