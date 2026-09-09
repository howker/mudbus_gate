package profile

import (
	"testing"
	"time"
)

func TestParseVKM360Mock(t *testing.T) {
	// митация кусочка профиля vkm360.yaml.
	yamlData := []byte(`
meta:
  vendor: "лемер"
  model: "-360"
  profile_version: "1.0.0"
  protocol: "modbus"
transport:
  supported: ["tcp", "rtu", "tcp-serial"]
codec:
  word_order_32: "0123"
points:
  - name: "ассовый расход"
    space: "IR"
    addr: 2008
    type: "float"
    unit: "кг/с"
    access: "read"
    group: "current"
`)

	p, err := ParseBytes(yamlData)
	if err != nil {
		t.Fatalf("ошибка парсинга: %v", err)
	}

	if p.Meta.Vendor != "лемер" {
		t.Errorf("ожидался вендор 'лемер', получено '%s'", p.Meta.Vendor)
	}
	if p.Codec.WordOrder32 != "0123" {
		t.Errorf("ожидался порядок 0123, получено '%s'", p.Codec.WordOrder32)
	}
	if len(p.Points) != 1 || p.Points[0].AddrOrZero() != 2008 {
		t.Errorf("точки данных распарсились неверно")
	}
}

func TestValidateRejectsEmptyPoints(t *testing.T) {
	yamlData := []byte(`
meta:
  vendor: "Тест"
  model: "T-1"
  profile_version: "1.0.0"
  protocol: "modbus"
`)
	if _, err := ParseBytes(yamlData); err == nil {
		t.Fatal("ожидалась ошибка валидации: пустой points")
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	yamlData := []byte(`
meta:
  vendor: "Тест"
  model: "T-1"
  profile_version: "1.0.0"
  protocol: "modbus"
points:
  - name: "Точка"
    space: "IR"
    addr: 1
    type: "not_a_real_type"
`)
	if _, err := ParseBytes(yamlData); err == nil {
		t.Fatal("ожидалась ошибка валидации: неизвестный type")
	}
}

func TestValidateRejectsUnknownProtocol(t *testing.T) {
	yamlData := []byte(`
meta:
  vendor: "Тест"
  model: "T-1"
  profile_version: "1.0.0"
  protocol: "not_a_real_protocol"
points:
  - name: "Точка"
    space: "IR"
    addr: 1
    type: "float"
`)
	if _, err := ParseBytes(yamlData); err == nil {
		t.Fatal("ожидалась ошибка валидации: неизвестный protocol")
	}
}
func TestArchivePeriodFromProfile(t *testing.T) {
	yamlData := []byte(`
meta:
  vendor: "Тест"
  model: "Archive-30"
  profile_version: "1.0.0"
  protocol: "modbus"
  archive_period_minutes: 30
points:
  - name: "x"
    space: "IR"
    addr: 1
    type: "float"
`)
	p, err := ParseBytes(yamlData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := p.ArchivePeriod(); got != 30*time.Minute {
		t.Fatalf("ArchivePeriod=%v, want 30m", got)
	}
}
