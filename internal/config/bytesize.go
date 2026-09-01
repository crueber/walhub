package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// sizeRe matches "20GiB", "64MiB", "0B", "512MB" — case-insensitive binary
// units (the i in KiB is optional: KB == KiB == kib, all 1024-based).
var sizeRe = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(b|kb|kib|mb|mib|gb|gib|tb|tib)$`)

// ParseByteSize parses a byte size in the Rust-spec spelling shared by the
// TOML file and the WALHUB__/WALGIT__ env overlay: B|KiB|MiB|GiB|TiB
// (case-insensitive, binary), a bare integer meaning bytes, "0B" meaning
// disabled.
func ParseByteSize(s string) (ByteSize, error) {
	if m := sizeRe.FindStringSubmatch(s); m != nil {
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q: %w", s, err)
		}
		var mult float64
		switch strings.ToLower(m[2]) {
		case "b":
			mult = 1
		case "kb", "kib":
			mult = 1 << 10
		case "mb", "mib":
			mult = 1 << 20
		case "gb", "gib":
			mult = 1 << 30
		case "tb", "tib":
			mult = 1 << 40
		}
		v := f * mult
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("invalid size %q: not a whole number of bytes", s)
		}
		if v > math.MaxInt64 || v < math.MinInt64 {
			return 0, fmt.Errorf("invalid size %q: overflows int64", s)
		}
		return ByteSize(int64(v)), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return ByteSize(n), nil
}

// UnmarshalText implements encoding.TextUnmarshaler so TOML strings
// ("64GiB") and bare TOML integers (bytes) both decode, and TOML encoding
// round-trips through the same spellings.
func (b *ByteSize) UnmarshalText(text []byte) error {
	parsed, err := ParseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (b ByteSize) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// String renders the size in the largest exact binary unit (GiB/MiB/KiB),
// "0B" for zero, plain bytes otherwise.
func (b ByteSize) String() string {
	v := int64(b)
	if v == 0 {
		return "0B"
	}
	for _, u := range [...]struct {
		div  int64
		name string
	}{
		{1 << 40, "TiB"},
		{1 << 30, "GiB"},
		{1 << 20, "MiB"},
		{1 << 10, "KiB"},
	} {
		if v%u.div == 0 && v/u.div != 0 {
			return strconv.FormatInt(v/u.div, 10) + u.name
		}
	}
	return strconv.FormatInt(v, 10) + "B"
}
