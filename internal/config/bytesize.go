package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ByteSize is a non-negative byte count rendered with IEC suffixes.
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be a scalar")
	}
	value, err := ParseByteSize(node.Value)
	if err != nil {
		return err
	}
	*b = value
	return nil
}

func (b ByteSize) MarshalYAML() (any, error)    { return b.String(), nil }
func (b ByteSize) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }

func (b ByteSize) String() string {
	value := int64(b)
	for _, unit := range []struct {
		suffix string
		value  int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if value >= unit.value && value%unit.value == 0 {
			return fmt.Sprintf("%d%s", value/unit.value, unit.suffix)
		}
	}
	return strconv.FormatInt(value, 10)
}

func ParseByteSize(raw string) (ByteSize, error) {
	raw = strings.TrimSpace(raw)
	units := []struct {
		suffix     string
		multiplier int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}, {"", 1}}
	for _, unit := range units {
		suffix, multiplier := unit.suffix, unit.multiplier
		if !strings.HasSuffix(raw, suffix) {
			continue
		}
		numeric := strings.TrimSuffix(raw, suffix)
		if numeric == "" {
			continue
		}
		value, err := strconv.ParseInt(numeric, 10, 64)
		if err != nil || value < 0 || (multiplier != 0 && value > (1<<63-1)/multiplier) {
			return 0, fmt.Errorf("invalid byte size %q", raw)
		}
		return ByteSize(value * multiplier), nil
	}
	return 0, fmt.Errorf("invalid byte size %q", raw)
}
