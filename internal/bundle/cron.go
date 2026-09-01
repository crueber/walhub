// Package bundle turns calendar slots into client-compatible bundle lists
// (08_bundles.md): the 6-field UTC cron, as-of content resolution, chain math,
// gates, building, retention/backfill, list rendering/serving, v2 advertisement,
// and D17 forcing.
//
// Seams (§8.2): the package consumes WalView (internal/wal), Primitives
// (internal/git §7.8), and the store.ObjectStore contract through narrow
// interfaces declared here; the real bindings live in bind_wal.go/bind_git.go.
package bundle

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// ErrNoFire is returned by Next when the schedule has no fire time within the
// 5-year search horizon (e.g. dom 31 in February).
var ErrNoFire = errors.New("bundle: schedule has no fire time within 5 years")

// ErrBetweenCap is returned by Between when the window holds more than
// 10 000 fire times.
var ErrBetweenCap = errors.New("bundle: between-window exceeds 10 000 fire times")

// Cron is a parsed 6-field UTC schedule: sec, min, hour, dom, mon, dow as
// uint64 bitmasks (bit i = value i allowed). dow uses bits 0..6 (Sun..Sat);
// a syntax dow of 7 is folded onto bit 0 (7 ≡ 0 Sun).
type Cron struct {
	sec, min, hour, dom, mon, dow uint64

	// domSet/dowSet: at least one term other than "*" restricted the field
	// (§8.3 vixie dom/dow OR rule).
	domSet, dowSet bool
}

// Field bounds: sec 0-59, min 0-59, hour 0-23, dom 1-31, mon 1-12, dow 0-7.
var fieldBounds = [6]struct{ lo, hi int }{
	{0, 59}, {0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7},
}

var fieldNames = [6]string{"second", "minute", "hour", "dom", "month", "dow"}

func maskOf(n int) uint64 { return uint64(1) << uint(n) }

func allMask(lo, hi int) uint64 {
	var m uint64
	for i := lo; i <= hi; i++ {
		m |= maskOf(i)
	}
	return m
}

// aliases expand to fixed 6-field schedules (§8.3):
// @yearly/@annually = 0 0 0 1 1 *; @monthly = 0 0 0 1 * *;
// @weekly = 0 0 0 * * 0; @daily/@midnight = 0 0 0 * * *; @hourly = 0 0 * * * *.
var aliases = map[string][6]uint64{
	"@yearly":   {maskOf(0), maskOf(0), maskOf(0), maskOf(1), maskOf(1), allMask(0, 6)},
	"@annually": {maskOf(0), maskOf(0), maskOf(0), maskOf(1), maskOf(1), allMask(0, 6)},
	"@monthly":  {maskOf(0), maskOf(0), maskOf(0), maskOf(1), allMask(1, 12), allMask(0, 6)},
	"@weekly":   {maskOf(0), maskOf(0), maskOf(0), allMask(1, 31), allMask(1, 12), maskOf(0)},
	"@daily":    {maskOf(0), maskOf(0), maskOf(0), allMask(1, 31), allMask(1, 12), allMask(0, 6)},
	"@midnight": {maskOf(0), maskOf(0), maskOf(0), allMask(1, 31), allMask(1, 12), allMask(0, 6)},
	"@hourly":   {maskOf(0), maskOf(0), allMask(0, 23), allMask(1, 31), allMask(1, 12), allMask(0, 6)},
}

func (c Cron) has(f, v int) bool {
	var m uint64
	switch f {
	case 0:
		m = c.sec
	case 1:
		m = c.min
	case 2:
		m = c.hour
	case 3:
		m = c.dom
	case 4:
		m = c.mon
	default:
		m = c.dow
	}
	return m&maskOf(v) != 0
}

func (c Cron) first(f int) int {
	var m uint64
	switch f {
	case 0:
		m = c.sec
	case 1:
		m = c.min
	case 2:
		m = c.hour
	case 3:
		m = c.dom
	case 4:
		m = c.mon
	default:
		m = c.dow
	}
	for i := 0; i < 64; i++ {
		if m&(maskOf(i)) != 0 {
			return i
		}
	}
	return -1
}

// ParseSchedule parses the normative 6-field UTC cron (§8.3). Exactly six
// fields; lists/ranges/steps including `a/s`; the seven @aliases. Month/weekday
// names and `@every <dur>` are rejected by name; so are 5-field crons, ranges
// a > b, step 0, and out-of-bounds values.
func ParseSchedule(s string) (Cron, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Cron{}, errors.New("schedule: empty")
	}
	if strings.HasPrefix(raw, "@every") {
		return Cron{}, fmt.Errorf("schedule: @every durations are not supported: %q", raw)
	}
	if strings.HasPrefix(raw, "@") {
		a, ok := aliases[strings.ToLower(raw)]
		if !ok {
			return Cron{}, fmt.Errorf("schedule: unknown alias %q (supported: @yearly @annually @monthly @weekly @daily @midnight @hourly)", raw)
		}
		set := a[3] != allMask(1, 31)
		dowSet := a[5] != allMask(0, 6)
		return Cron{sec: a[0], min: a[1], hour: a[2], dom: a[3], mon: a[4], dow: a[5],
			domSet: set, dowSet: dowSet}, nil
	}
	fields := strings.Fields(raw)
	if len(fields) != 6 {
		return Cron{}, fmt.Errorf("schedule: want exactly 6 fields (sec min hour dom mon dow), got %d: %q", len(fields), raw)
	}
	var c Cron
	for i, f := range fields {
		m, set, err := parseField(i, f)
		if err != nil {
			return Cron{}, err
		}
		switch i {
		case 0:
			c.sec = m
		case 1:
			c.min = m
		case 2:
			c.hour = m
		case 3:
			c.dom, c.domSet = m, set
		case 4:
			c.mon = m
		default:
			c.dow, c.dowSet = m, set
		}
	}
	return c, nil
}

// parseField parses one field: term ("," term)*, term := ("*" | range) ["/" step].
// Returns the value bitmask and whether any term restricted the field.
func parseField(fi int, s string) (uint64, bool, error) {
	bounds := fieldBounds[fi]
	var m uint64
	restricted := false
	for _, term := range strings.Split(s, ",") {
		if term == "" {
			return 0, false, fmt.Errorf("%s: empty term in %q", fieldNames[fi], s)
		}
		body, stepStr, hasStep := strings.Cut(term, "/")
		step := 1
		if hasStep {
			v, err := strconv.Atoi(stepStr)
			if err != nil || v <= 0 {
				return 0, false, fmt.Errorf("%s: step must be a positive integer, got %q", fieldNames[fi], stepStr)
			}
			step = v
		}
		var lo, hi int
		switch {
		case body == "*":
			lo, hi = bounds.lo, bounds.hi
			if hasStep {
				restricted = true // `*/s` is a term ≠ `*` (§8.3 day rule)
			}
		case strings.Contains(body, "-"):
			a, b, ok := strings.Cut(body, "-")
			if !ok {
				return 0, false, fmt.Errorf("%s: bad range %q", fieldNames[fi], body)
			}
			var err error
			lo, err = parseValue(fi, a)
			if err != nil {
				return 0, false, err
			}
			hi, err = parseValue(fi, b)
			if err != nil {
				return 0, false, err
			}
			if lo > hi {
				return 0, false, fmt.Errorf("%s: range %d-%d has lo > hi", fieldNames[fi], lo, hi)
			}
			restricted = true
		default:
			v, err := parseValue(fi, body)
			if err != nil {
				return 0, false, err
			}
			lo = v
			hi = bounds.hi // vixie `a/s` = {a, a+s, … ≤ max}; without a step this collapses below
			restricted = true
		}
		if !hasStep && body != "*" && !strings.Contains(body, "-") {
			hi = lo // single value
		}
		for v := lo; v <= hi; v += step {
			if v > bounds.hi {
				break
			}
			d := v
			if fi == 5 && d == 7 {
				d = 0 // 7 ≡ 0 Sunday
			}
			m |= maskOf(d)
		}
	}
	return m, restricted, nil
}

// parseValue parses one decimal value with the field's bounds and rejects
// alphabetic names (jan/mon/…) with a named-field error (§8.3).
func parseValue(fi int, s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("%s: empty value", fieldNames[fi])
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%s: month/weekday names are not supported: %q", fieldNames[fi], s)
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: bad value %q", fieldNames[fi], s)
	}
	b := fieldBounds[fi]
	if v < b.lo || v > b.hi {
		return 0, fmt.Errorf("%s: value %d out of bounds %d-%d", fieldNames[fi], v, b.lo, b.hi)
	}
	return v, nil
}

// dayOK applies the vixie dom/dow rule (§8.3): both restricted → either may
// match; exactly one restricted → it must match; neither → the day is fine.
func (c Cron) dayOK(t time.Time) bool {
	domOK := c.dom&maskOf(t.Day()) != 0
	dowOK := c.dow&maskOf(int(t.Weekday())) != 0
	switch {
	case c.domSet && c.dowSet:
		return domOK || dowOK
	case c.domSet:
		return domOK
	case c.dowSet:
		return dowOK
	default:
		return true
	}
}

// Next returns the first fire time strictly after `after`, in UTC (§8.3:
// classic cron-advance at second resolution; ErrNoFire after a 5-year horizon).
func (c Cron) Next(after time.Time) (time.Time, error) {
	t := after.UTC().Truncate(time.Second).Add(time.Second)
	limit := t.AddDate(5, 0, 0)
	sec0, min0, hour0 := c.first(0), c.first(1), c.first(2)
	for i := 0; ; i++ {
		if i > 2_000_000 || !t.Before(limit) {
			return time.Time{}, ErrNoFire
		}
		if c.mon&maskOf(int(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month(), 1, hour0, min0, sec0, 0, time.UTC).AddDate(0, 1, 0)
			for c.mon&maskOf(int(t.Month())) == 0 {
				t = t.AddDate(0, 1, 0)
			}
			continue
		}
		if !c.dayOK(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), hour0, min0, sec0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if c.hour&maskOf(t.Hour()) == 0 {
			if h := nextSet(c.hour, t.Hour()); h >= 0 {
				t = time.Date(t.Year(), t.Month(), t.Day(), h, min0, sec0, 0, time.UTC)
			} else {
				t = time.Date(t.Year(), t.Month(), t.Day(), hour0, min0, sec0, 0, time.UTC).AddDate(0, 0, 1)
			}
			continue
		}
		if c.min&maskOf(t.Minute()) == 0 {
			if mi := nextSet(c.min, t.Minute()); mi >= 0 {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), mi, sec0, 0, time.UTC)
			} else {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), min0, sec0, 0, time.UTC).Add(time.Hour)
			}
			continue
		}
		if c.sec&maskOf(t.Second()) == 0 {
			if s := nextSet(c.sec, t.Second()); s >= 0 {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), s, 0, time.UTC)
			} else {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), sec0, 0, time.UTC).Add(time.Minute)
			}
			continue
		}
		return t, nil
	}
}

// nextSet returns the smallest set value strictly greater than after, or -1.
func nextSet(m uint64, after int) int {
	if after >= 63 {
		return -1
	}
	high := m >> uint(after+1)
	if high == 0 {
		return -1
	}
	return after + 1 + bits.TrailingZeros64(high)
}

// Between returns the fire times in [start, end] (UTC, second resolution).
// More than 10 000 fires in the window → ErrBetweenCap.
func (c Cron) Between(start, end time.Time) ([]time.Time, error) {
	if end.Before(start) {
		return nil, nil
	}
	var out []time.Time
	t := start.UTC().Add(-time.Second)
	for {
		n, err := c.Next(t)
		if err != nil {
			if errors.Is(err, ErrNoFire) {
				return out, nil
			}
			return out, err
		}
		if n.After(end) {
			return out, nil
		}
		if len(out) >= 10_000 {
			return out, ErrBetweenCap
		}
		out = append(out, n)
		t = n
	}
}
