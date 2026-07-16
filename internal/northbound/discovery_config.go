package northbound

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadDiscoveryFixture reads a DiscoveryFixture from a YAML file. The
// zero-value DiscoveryFixture{} (ReadFillByte=0, no overrides) is a
// perfectly valid fixture, so callers may skip this and pass a literal
// DiscoveryFixture{} when no file is supplied.
func LoadDiscoveryFixture(path string) (DiscoveryFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return DiscoveryFixture{}, fmt.Errorf("northbound: read discovery fixture %s: %w", path, err)
	}
	var f DiscoveryFixture
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return DiscoveryFixture{}, fmt.Errorf("northbound: parse discovery fixture %s: %w", path, err)
	}
	return f, nil
}
