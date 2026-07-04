package config

import (
"fmt"
"os"

"gopkg.in/yaml.v3"
)

type AppConfig struct {
App struct {
StoragePath string `yaml:"storage_path"`
WebPort     int    `yaml:"web_port"` // <--- от это поле мы забыли!
} `yaml:"app"`
Devices []DeviceConfig `yaml:"devices"`
}

type DeviceConfig struct {
ID        string `yaml:"id"`
Profile   string `yaml:"profile"`
Transport struct {
Kind      string `yaml:"kind"`
Host      string `yaml:"host"`
Port      int    `yaml:"port"`
TimeoutMs int    `yaml:"timeout_ms"`
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
