package profile

import (
    "fmt"
    "os"

    "gopkg.in/yaml.v3"
)

// Profile describes the full device profile.
type Profile struct {
    Meta       Meta          `yaml:"meta"`
    Transport  Transport     `yaml:"transport"`
    Codec      Codec         `yaml:"codec"`
    Session    Session       `yaml:"session"`
    Instances  map[string]Instance `yaml:"instances"`
    Points     []Point       `yaml:"points"`
    Archives   []Archive     `yaml:"archives"`
    QualityMap []QualityRule `yaml:"quality_map"`
}

type Meta struct {
    Vendor         string   `yaml:"vendor"`
    Model          string   `yaml:"model"`
    ProfileVersion string   `yaml:"profile_version"`
    Protocol       string   `yaml:"protocol"`
    FirmwareCompat []string `yaml:"firmware_compat"`
    Description    string   `yaml:"description"`
}

type Transport struct {
    Supported []string               `yaml:"supported"`
    Defaults  map[string]interface{} `yaml:"defaults"`
}

type Codec struct {
    WordOrder32    string `yaml:"word_order_32"`
    WordOrder64    string `yaml:"word_order_64"`
    StringEncoding string `yaml:"string_encoding"`
}

type Session struct {
    Type      string                 `yaml:"type"`
    ByteOrder map[string]interface{} `yaml:"byte_order"`
    Auth      map[string]interface{} `yaml:"auth"`
    Keepalive map[string]interface{} `yaml:"keepalive"`
    Params    map[string]interface{} `yaml:"params"`
}

// Instance describes how instances of a point (pipe/logical_input/etc) are enumerated.
type Instance struct {
    Enumerate string `yaml:"enumerate"` // fixed_count | bitmask_register | config
    Count     int    `yaml:"count"`
    Register  int    `yaml:"register"`
    Space     string `yaml:"space"`
    MaxCount  int    `yaml:"max_count"`
}

// Point describes a single data point in a device profile.
type Point struct {
    Name        string            `yaml:"name"`
    Space       string            `yaml:"space"`        // Modbus: HR | IR | coil | discrete
    // Addr is a pointer so that a legitimate register address of 0
    // (e.g. Akron-01's "V" point at HR 0x0000) is distinguishable from
    // "not set" - a plain int couldn't tell those apart, which would
    // have wrongly rejected any point living at address 0 as missing an
    // address (mirrors the same fix already applied to QualityRule.Code
    // for the same reason).
    Addr        *int              `yaml:"addr"`
    AddrFormula string            `yaml:"addr_formula"`  // parametric address (e.g. pipe-based) or Merkuriy command encoding
    Instance    string            `yaml:"instance"`       // reference to Instances key
    Type        string            `yaml:"type"`
    Length      int               `yaml:"length"`
    Unit        string            `yaml:"unit"`
    Scale       float64           `yaml:"scale"`
    Access      string            `yaml:"access"`
    Group       string            `yaml:"group"`
    Bits        map[string]string `yaml:"bits"`
    Note        string            `yaml:"note"`
    // ByteOffset selects a sub-region of the raw register read, for
    // devices that pack more than one logical field into a single
    // register (e.g. Akron-01/02's clock: register 0x0010 holds both
    // "second" (byte 0) and "minute" (byte 1)). Zero for the common case
    // (the point occupies the whole read). Two points may then share the
    // same Addr with different ByteOffset - each triggers its own
    // (duplicate) register read, which is an acceptable tradeoff for
    // rarely-polled fields like a clock.
    ByteOffset int `yaml:"byte_offset"`
    // Epoch mirrors RecordField.Epoch: when set on a uint32 point (e.g.
    // "1970-01-01"), decodePoint returns a time.Time instead of a raw
    // int64, for registers that hold a Unix timestamp (e.g. VZLET's own
    // HR/IR 0x8000 "Текущее время" registers).
    Epoch string `yaml:"epoch"`
}

// AddrOrZero returns *Addr, or 0 if Addr is nil (a point whose address is
// unset, e.g. because it uses AddrFormula/Instance instead of a fixed
// Addr). Callers that only read Addr for non-instance points can use this
// safely, since Validate() guarantees every point has either Addr set or
// AddrFormula set.
func (p Point) AddrOrZero() int {
    if p.Addr == nil {
        return 0
    }
    return *p.Addr
}

// RecordField describes one field inside an archive record layout.
type RecordField struct {
    Offset int               `yaml:"offset"`
    Name   string            `yaml:"name"`
    Type   string            `yaml:"type"`
    Unit   string            `yaml:"unit"`
    Scale  float64           `yaml:"scale"`
    CRC    bool              `yaml:"crc"`
    Epoch  string            `yaml:"epoch"`
    Decode string            `yaml:"decode"`
    Bits   map[string]string `yaml:"bits"`
}

// Archive describes one archive strategy configuration.
type Archive struct {
    ID              string                 `yaml:"id"`
    Strategy        string                 `yaml:"strategy"`
    Params          map[string]interface{} `yaml:"params"`
    RecordLayout    []RecordField          `yaml:"record_layout"`
    RingBuffer      bool                   `yaml:"ring_buffer"`
    TimeAccess      bool                   `yaml:"time_access"`
    IndexAccess     bool                   `yaml:"index_access"`
    DSTAware        bool                   `yaml:"dst_aware"`
    FirmwareVariant string                 `yaml:"firmware_variant"`
    Note            string                 `yaml:"note"`
}

type QualityRule struct {
    Source  string `yaml:"source"`
    Bit     int    `yaml:"bit"`
    Code    *int   `yaml:"code"`
    Meaning string `yaml:"meaning"`
    Quality string `yaml:"quality"`
}

// allowed value sets, mirrored from profile.schema.json (kept in sync manually).
var allowedProtocols = map[string]bool{"modbus": true, "merkuriy": true}
var allowedDataTypes = map[string]bool{
    "int16": true, "uint16": true, "int32": true, "uint32": true,
    "float": true, "double": true,
    "string": true, "asciiz": true,
    "u32+float": true, "long+float": true,
    "scaled_int": true, "bitfield": true, "stInfoEvent": true, "bcd": true, "akron_volume": true,
    "coil": true,
}
var allowedSpaces = map[string]bool{"HR": true, "IR": true, "coil": true, "discrete": true}
var allowedAccess = map[string]bool{"read": true, "write": true, "readwrite": true}
var allowedStrategies = map[string]bool{
    "mb_request_poll_string": true,
    "mb_indexed_binary":       true,
    "mb_func65":                true,
    "merkuriy_long_response":  true,
    "akron_archive":            true,
}
var allowedQuality = map[string]bool{
    "VALID": true, "VERIFIED": true, "ESTIMATED": true, "INTERPOLATED": true,
    "STALE": true, "INVALID": true, "NO_DATA": true,
}

// Validate checks the profile against the rules mirrored from profile.schema.json.
// This is a lightweight hand-written validator (no external jsonschema dependency
// available offline); it enforces the same required fields and enums as the schema.
func (p *Profile) Validate() error {
    if p.Meta.Vendor == "" {
        return fmt.Errorf("meta.vendor is required")
    }
    if p.Meta.Model == "" {
        return fmt.Errorf("meta.model is required")
    }
    if p.Meta.ProfileVersion == "" {
        return fmt.Errorf("meta.profile_version is required")
    }
    if !allowedProtocols[p.Meta.Protocol] {
        return fmt.Errorf("meta.protocol %q is not one of modbus|merkuriy", p.Meta.Protocol)
    }

    if len(p.Points) == 0 {
        return fmt.Errorf("points must not be empty")
    }

    names := make(map[string]bool)
    for i, pt := range p.Points {
        if pt.Name == "" {
            return fmt.Errorf("points[%d]: name is required", i)
        }
        if pt.Addr == nil && pt.AddrFormula == "" {
            return fmt.Errorf("points[%d] %q: addr or addr_formula is required", i, pt.Name)
        }
        if !allowedDataTypes[pt.Type] {
            return fmt.Errorf("points[%d] %q: type %q is not allowed", i, pt.Name, pt.Type)
        }
        if pt.Space != "" && !allowedSpaces[pt.Space] {
            return fmt.Errorf("points[%d] %q: space %q is not allowed", i, pt.Name, pt.Space)
        }
        if pt.Access != "" && !allowedAccess[pt.Access] {
            return fmt.Errorf("points[%d] %q: access %q is not allowed", i, pt.Name, pt.Access)
        }
        if pt.ByteOffset < 0 {
            return fmt.Errorf("points[%d] %q: byte_offset must be >= 0, got %d", i, pt.Name, pt.ByteOffset)
        }
        if pt.Instance != "" {
            if _, ok := p.Instances[pt.Instance]; !ok {
                return fmt.Errorf("points[%d] %q: instance %q is not defined in instances", i, pt.Name, pt.Instance)
            }
        }
        names[pt.Name] = true
    }

    archiveFieldNames := make(map[string]bool)
    for i, a := range p.Archives {
        if !allowedStrategies[a.Strategy] {
            return fmt.Errorf("archives[%d] %q: strategy %q is not allowed", i, a.ID, a.Strategy)
        }
        for _, f := range a.RecordLayout {
            if !allowedDataTypes[f.Type] {
                return fmt.Errorf("archives[%d] %q: record_layout field %q has invalid type %q", i, a.ID, f.Name, f.Type)
            }
            archiveFieldNames[f.Name] = true
        }
    }

    for i, q := range p.QualityMap {
        if !allowedQuality[q.Quality] {
            return fmt.Errorf("quality_map[%d]: quality %q is not allowed", i, q.Quality)
        }
        if q.Source != "" && !names[q.Source] && !archiveFieldNames[q.Source] {
            return fmt.Errorf("quality_map[%d]: source %q not found among points or archive record fields", i, q.Source)
        }
    }

    return nil
}

// ParseBytes parses a profile from a byte slice and validates it.
func ParseBytes(data []byte) (*Profile, error) {
    var p Profile
    if err := yaml.Unmarshal(data, &p); err != nil {
        return nil, fmt.Errorf("yaml unmarshal error: %w", err)
    }
    if err := p.Validate(); err != nil {
        return nil, fmt.Errorf("profile validation error: %w", err)
    }
    return &p, nil
}

// Parse reads a YAML file from disk, parses and validates it.
func Parse(path string) (*Profile, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("failed to read profile %s: %w", path, err)
    }
    return ParseBytes(data)
}