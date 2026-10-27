package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Bandwidth is a byte-per-second rate. It can be written in YAML either as a
// raw integer (bytes per second) or as a human-readable bit rate such as
// "100 mbps", "1 gbps", or "500 kbps".
type Bandwidth uint64

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *Bandwidth) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("bandwidth must be an integer or a string")
	}
	if value.Tag == "!!int" {
		var raw uint64
		if err := value.Decode(&raw); err != nil {
			return fmt.Errorf("invalid bandwidth %q: %w", value.Value, err)
		}
		*b = Bandwidth(raw)
		return nil
	}
	if value.Tag != "!!str" {
		return fmt.Errorf("bandwidth must be an integer or a string")
	}
	parsed, err := parseBandwidth(value.Value)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (b Bandwidth) MarshalYAML() (any, error) {
	return uint64(b), nil
}

// ByteSize is a byte count that accepts either a raw integer or a string such
// as "8 mb". It is used for QUIC window sizes.
type ByteSize uint64

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be an integer or a string")
	}
	if value.Tag == "!!int" {
		var raw uint64
		if err := value.Decode(&raw); err != nil {
			return fmt.Errorf("invalid byte size %q: %w", value.Value, err)
		}
		*b = ByteSize(raw)
		return nil
	}
	if value.Tag != "!!str" {
		return fmt.Errorf("byte size must be an integer or a string")
	}
	parsed, err := parseByteSize(value.Value)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (b ByteSize) MarshalYAML() (any, error) {
	return uint64(b), nil
}

func parseByteSize(s string) (ByteSize, error) {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if normalized == "" {
		return 0, fmt.Errorf("byte size must not be empty")
	}

	type unit struct {
		suffix string
		factor float64
	}
	units := []unit{
		{"gb", 1024 * 1024 * 1024},
		{"mb", 1024 * 1024},
		{"kb", 1024},
		{"b", 1},
		{"", 1},
	}

	for _, u := range units {
		if !strings.HasSuffix(normalized, u.suffix) {
			continue
		}
		number := strings.TrimSuffix(normalized, u.suffix)
		if number == "" {
			return 0, fmt.Errorf("invalid byte size %q", s)
		}
		value, err := strconv.ParseFloat(number, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("invalid byte size %q", s)
		}
		if value < 0 {
			return 0, fmt.Errorf("byte size must not be negative")
		}
		bytes := value * u.factor
		if bytes > float64(math.MaxUint64) {
			return 0, fmt.Errorf("byte size %q is too large", s)
		}
		return ByteSize(math.Round(bytes)), nil
	}
	return 0, fmt.Errorf("invalid byte size %q", s)
}

// parseBandwidth parses bit rates (kbps/mbps/gbps, decimal) and byte sizes
// (kb/mb/gb, binary) into bytes. A bare number is treated as bytes.
func parseBandwidth(s string) (Bandwidth, error) {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if normalized == "" {
		return 0, fmt.Errorf("bandwidth must not be empty")
	}

	type unit struct {
		suffix string
		factor float64
	}
	// Bit-rate units come first so "100mbps" is not mistaken for bytes.
	units := []unit{
		{"tbps", 1e12 / 8},
		{"gbps", 1e9 / 8},
		{"mbps", 1e6 / 8},
		{"kbps", 1e3 / 8},
		{"gb", 1024 * 1024 * 1024},
		{"mb", 1024 * 1024},
		{"kb", 1024},
		{"b", 1},
		{"", 1},
	}

	for _, u := range units {
		if !strings.HasSuffix(normalized, u.suffix) {
			continue
		}
		number := strings.TrimSuffix(normalized, u.suffix)
		if number == "" {
			return 0, fmt.Errorf("invalid bandwidth %q", s)
		}
		value, err := strconv.ParseFloat(number, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("invalid bandwidth %q", s)
		}
		if value < 0 {
			return 0, fmt.Errorf("bandwidth must not be negative")
		}
		bytes := value * u.factor
		if bytes > float64(math.MaxUint64) {
			return 0, fmt.Errorf("bandwidth %q is too large", s)
		}
		return Bandwidth(math.Round(bytes)), nil
	}
	return 0, fmt.Errorf("invalid bandwidth %q", s)
}

// Duration is a time.Duration with YAML support for values such as "30s" and
// "24h".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string")
	}
	if value.Tag == "!!int" {
		return fmt.Errorf("duration must be a string such as \"30s\"")
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// String returns the duration in Go's standard form.
func (d Duration) String() string {
	return time.Duration(d).String()
}
