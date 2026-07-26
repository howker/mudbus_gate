package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type AppConfig struct {
	App struct {
		StoragePath string `yaml:"storage_path"`
		WebPort     int    `yaml:"web_port"`
	} `yaml:"app"`
	Devices []DeviceConfig `yaml:"devices"`

	// NorthboundAkron is optional and only read by `mbgw serve` (the
	// merged single-process southbound+northbound command). When present,
	// serve also starts the Akron raw-RTU carrier in-process, sharing the
	// same *sqliterepo.Repo the poller writes through, instead of the
	// separate `mbgw northbound --serve-akron` process. `mbgw run` never
	// reads this field, so existing config.yaml files keep working
	// unchanged whether or not this section is present.
	NorthboundAkron *NorthboundAkronConfig `yaml:"northbound_akron,omitempty"`
}

// NorthboundAkronConfig mirrors the flags `mbgw northbound --serve-akron`
// used to take on the command line (--listen/--device/--log), now sourced
// from config.yaml instead so `mbgw serve` needs only --config.
type NorthboundAkronConfig struct {
	Listen   string `yaml:"listen"`
	DeviceID string `yaml:"device_id"`
	Log      string `yaml:"log"` // default: akron_carrier_live.jsonl
}

type DeviceConfig struct {
	ID        string `yaml:"id"`
	Profile   string `yaml:"profile"`
	Transport struct {
		Kind string `yaml:"kind"`
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		// COM/Baudrate/Parity/StopBits are for kind: rtu_serial or
		// tcp_serial (a real or converter-emulated COM port) — added
		// because the config previously only supported TCP-addressed
		// devices (Host/Port), leaving no way to describe a device
		// like our real Акрон-01, which is reached via a virtual COM
		// port (COM105) emulated by an Ethernet/RS-485 converter's
		// own driver software, not a raw TCP socket.
		COM       string `yaml:"com"`
		Baudrate  int    `yaml:"baudrate"`
		Parity    string `yaml:"parity"`   // "none"|"even"|"odd"
		StopBits  int    `yaml:"stopbits"` // 1|2
		TimeoutMs int    `yaml:"timeout_ms"`
		// UnitID is the Modbus slave/bus address. Previously hardcoded
		// to 1 for every device in cmd/mbgw/run.go; now configurable
		// per device (still defaults to 1 if unset/zero — see run.go —
		// so existing configs without this field keep working).
		UnitID uint8 `yaml:"unit_id"`
	} `yaml:"transport"`
}

func Load(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения конфига: %w", err)
	}
	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("ошибка парсинга YAML: %w", err)
	}
	return &cfg, nil
}
