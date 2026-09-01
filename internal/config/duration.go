package config

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// durationRe matches the Rust-spec single-suffix spellings: "5ms", "1h",
// "30d", "7w", "0s". The d/w units exist only in config strings — never in Go
// literals.
var durationRe = regexp.MustCompile(`^([0-9]+)\s*(ms|s|m|h|d|w)$`)

// ParseDuration parses a duration in the Rust-spec spelling shared by the TOML
// file and the WALHUB__/WALGIT__ env overlay: unit suffixes ms|s|m|h|d|w, a
// bare integer meaning seconds, "0s" meaning disabled. Compound Go forms
// ("1h30m", "-20s") are accepted via time.ParseDuration.
func ParseDuration(s string) (Duration, error) {
	if m := durationRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		var unit time.Duration
		switch m[2] {
		case "ms":
			unit = time.Millisecond
		case "s":
			unit = time.Second
		case "m":
			unit = time.Minute
		case "h":
			unit = time.Hour
		case "d":
			unit = 24 * time.Hour
		case "w":
			unit = 7 * 24 * time.Hour
		}
		d := time.Duration(n) * unit
		if n > 0 && d/unit != time.Duration(n) {
			return 0, fmt.Errorf("invalid duration %q: overflows time.Duration", s)
		}
		return Duration(d), nil
	}
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		d := time.Duration(n) * time.Second
		if n > 0 && d/time.Second != time.Duration(n) {
			return 0, fmt.Errorf("invalid duration %q: overflows time.Duration", s)
		}
		return Duration(d), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return Duration(d), nil
}

// UnmarshalText implements encoding.TextUnmarshaler so TOML strings
// ("1h") and bare TOML integers (seconds) both decode, and TOML encoding
// round-trips through the same spellings.
func (d *Duration) UnmarshalText(b []byte) error {
	parsed, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// String renders the duration in Go duration syntax (time.Duration compatible,
// "0s" for zero), which ParseDuration accepts back.
func (d Duration) String() string { return time.Duration(d).String() }
