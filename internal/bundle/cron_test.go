package bundle

import (
	"errors"
	"testing"
	"time"
)

// mustCron parses s or fails t.
func mustCron(t *testing.T, s string) Cron {
	t.Helper()
	c, err := ParseSchedule(s)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", s, err)
	}
	return c
}

// The §8.3 cron table: parse+validate cases (30+), Next advances, Between
// windows, aliases, vixie dom/dow OR, and every rejection named in the doc.
func TestParseSchedule(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string // "" = parse ok
	}{
		// --- accepted ---
		{name: "6 fields", in: "0 0 0 1 1 *"},
		{name: "6 fields full numeric", in: "0 30 2 29 2 1"},
		{name: "star fields", in: "* * * * * *"},
		{name: "alias yearly", in: "@yearly"},
		{name: "alias annually", in: "@annually"},
		{name: "alias monthly", in: "@monthly"},
		{name: "alias weekly", in: "@weekly"},
		{name: "alias daily", in: "@daily"},
		{name: "alias midnight", in: "@midnight"},
		{name: "alias hourly", in: "@hourly"},
		{name: "step on star", in: "*/15 * * * * *"},
		{name: "range step", in: "0 0 9-18/4 * * *"},
		{name: "list", in: "0 0 0 1,15 * *"},
		{name: "list with ranges", in: "0 0 0 1-5,20-25 * *"},
		{name: "vixie a/s", in: "0 0 0 1/10 * *"},
		{name: "dow 7 sunday", in: "0 0 0 * * 7"},
		{name: "dow range", in: "0 0 0 * * 1-5"},
		{name: "duplicate collapse", in: "0 0 0 3,3,3 * *"},
		{name: "stepped seconds", in: "*/10 0 0 * * *"},
		// --- rejected (§8.3) ---
		{name: "5-field cron", in: "0 0 0 * *", wantErr: "exactly 6"},
		{name: "7 fields", in: "0 0 0 0 0 0 0", wantErr: "exactly 6"},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "month name", in: "0 0 0 1 jan *", wantErr: "names"},
		{name: "weekday name", in: "0 0 0 * * mon", wantErr: "names"},
		{name: "@every", in: "@every 1h", wantErr: "@every"},
		{name: "@every alone", in: "@every", wantErr: "@every"},
		{name: "range a>b", in: "0 0 0 10-5 * *", wantErr: "lo > hi"},
		{name: "step zero", in: "0 0 0 */0 * *", wantErr: "step"},
		{name: "step negative", in: "0 0 0 */-2 * *", wantErr: "step"},
		{name: "sec out of bounds", in: "60 0 0 * * *", wantErr: "out of bounds"},
		{name: "min out of bounds", in: "0 60 0 * * *", wantErr: "out of bounds"},
		{name: "hour out of bounds", in: "0 0 24 * * *", wantErr: "out of bounds"},
		{name: "dom out of bounds", in: "0 0 0 32 * *", wantErr: "out of bounds"},
		{name: "mon out of bounds", in: "0 0 0 * 13 *", wantErr: "out of bounds"},
		{name: "dow out of bounds", in: "0 0 0 * * 8", wantErr: "out of bounds"},
		{name: "unknown alias", in: "@fortnightly", wantErr: "unknown alias"},
		{name: "bad step token", in: "0 0 0 */x * *", wantErr: "step"},
		{name: "empty term", in: "0 0 0 1,,2 * *", wantErr: "empty term"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseSchedule(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseSchedule(%q) = ok, want error containing %q", tt.in, tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseSchedule(%q) error %q, want containing %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSchedule(%q): %v", tt.in, err)
			}
			_ = c
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// TestNext covers the cron-advance algorithm at second resolution (§8.3).
func TestNext(t *testing.T) {
	hourly, _ := ParseSchedule("@hourly")
	weekly, _ := ParseSchedule("@weekly")
	daily, _ := ParseSchedule("@daily")
	hourField, _ := ParseSchedule("0 0 9-18/4 * * *") // 09,13,17
	monthStep, _ := ParseSchedule("0 0 0 1 */3 *")    // Jan, Apr, Jul, Oct
	impossible, _ := ParseSchedule("0 0 0 31 2 *")    // Feb 31 never exists

	tests := []struct {
		name        string
		c           Cron
		after, want string
	}{
		{name: "hourly next hour", c: hourly, after: "2026-08-30T05:07:09Z", want: "2026-08-30T06:00:00Z"},
		{name: "hourly same hour boundary", c: hourly, after: "2026-08-30T05:00:00Z", want: "2026-08-30T06:00:00Z"},
		{name: "hourly strictly after", c: hourly, after: "2026-08-30T05:00:00.5Z", want: "2026-08-30T06:00:00Z"},
		{name: "weekly from midweek", c: weekly, after: "2026-08-26T12:00:00Z", want: "2026-08-30T00:00:00Z"}, // Wed → Sun
		{name: "weekly on sunday", c: weekly, after: "2026-08-30T00:00:00Z", want: "2026-09-06T00:00:00Z"},
		{name: "daily", c: daily, after: "2026-08-30T23:59:59Z", want: "2026-08-31T00:00:00Z"},
		{name: "stepped hour", c: hourField, after: "2026-08-30T10:00:00Z", want: "2026-08-30T13:00:00Z"},
		{name: "stepped hour wraps to next day", c: hourField, after: "2026-08-30T18:00:01Z", want: "2026-08-31T09:00:00Z"},
		{name: "month step skips", c: monthStep, after: "2026-01-01T00:00:01Z", want: "2026-04-01T00:00:00Z"},
		{name: "month step across year", c: monthStep, after: "2026-07-01T00:00:00Z", want: "2026-10-01T00:00:00Z"},
		{name: "dow 7 == sunday", c: must2(t, "0 0 0 * * 7"), after: "2026-08-27T00:00:00Z", want: "2026-08-30T00:00:00Z"},
		{name: "dom OR dow both restricted (friday)", c: must2(t, "0 0 0 13 * 5"), after: "2026-01-01T00:00:00Z", want: "2026-01-02T00:00:00Z"},
		{name: "dom OR dow both restricted (next friday)", c: must2(t, "0 0 0 13 * 5"), after: "2026-01-02T00:00:01Z", want: "2026-01-09T00:00:00Z"},
		{name: "dom OR dow 13th later", c: must2(t, "0 0 0 13 * 5"), after: "2026-01-09T00:00:01Z", want: "2026-01-13T00:00:00Z"},
		{name: "second field advance", c: must2(t, "*/20 * * * * *"), after: "2026-08-30T00:00:21Z", want: "2026-08-30T00:00:40Z"},
		{name: "minute+second reset", c: must2(t, "30 15 * * * *"), after: "2026-08-30T15:30:31Z", want: "2026-08-30T16:15:30Z"},
		{name: "impossible returns ErrNoFire", c: impossible, after: "2026-01-01T00:00:00Z", want: "ERR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.c.Next(at(tt.after))
			if tt.want == "ERR" {
				if !errors.Is(err, ErrNoFire) {
					t.Fatalf("Next(%s) err = %v, want ErrNoFire", tt.after, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Next(%s): %v", tt.after, err)
			}
			if got.Format(time.RFC3339) != tt.want {
				t.Fatalf("Next(%s) = %s, want %s", tt.after, got.Format(time.RFC3339), tt.want)
			}
		})
	}

	// Sanity: weekly fires only on Sunday 00:00:00 UTC.
	for i := range 400 {
		probe := at("2026-08-23T00:00:00Z").AddDate(0, 0, i)
		n, err := weekly.Next(probe)
		if err != nil {
			t.Fatalf("weekly Next(%s): %v", probe, err)
		}
		if n.Weekday() != time.Sunday || n.Hour() != 0 || n.Minute() != 0 || n.Second() != 0 {
			t.Fatalf("weekly Next(%s) = %s, want a Sunday 00:00:00", probe, n)
		}
	}
}

func must2(t *testing.T, s string) Cron {
	t.Helper()
	c, err := ParseSchedule(s)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", s, err)
	}
	return c
}

// TestAliases pins the §8.3 alias expansions by observed fire times.
func TestAliases(t *testing.T) {
	tests := []struct {
		alias, after, want string
	}{
		{"@yearly", "2026-03-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"@annually", "2026-03-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"@monthly", "2026-01-15T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"@weekly", "2026-08-26T00:00:00Z", "2026-08-30T00:00:00Z"},
		{"@daily", "2026-08-30T01:00:00Z", "2026-08-31T00:00:00Z"},
		{"@midnight", "2026-08-30T01:00:00Z", "2026-08-31T00:00:00Z"},
		{"@hourly", "2026-08-30T01:30:00Z", "2026-08-30T02:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			c, err := ParseSchedule(tt.alias)
			if err != nil {
				t.Fatalf("ParseSchedule(%q): %v", tt.alias, err)
			}
			got, err := c.Next(at(tt.after))
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got.Format(time.RFC3339) != tt.want {
				t.Fatalf("%s Next(%s) = %s, want %s", tt.alias, tt.after, got, tt.want)
			}
		})
	}
}

// TestBetween covers windowed fire enumeration and the 10 000 cap (§8.3).
func TestBetween(t *testing.T) {
	hourly, _ := ParseSchedule("@hourly")
	fires, err := hourly.Between(at("2026-08-30T00:00:00Z"), at("2026-08-30T05:00:00Z"))
	if err != nil {
		t.Fatalf("Between: %v", err)
	}
	if len(fires) != 6 {
		t.Fatalf("got %d fires, want 6 (00..05 inclusive)", len(fires))
	}
	if fires[0] != at("2026-08-30T00:00:00Z") || fires[5] != at("2026-08-30T05:00:00Z") {
		t.Fatalf("window endpoints wrong: %s..%s", fires[0], fires[5])
	}

	// start > end → empty.
	if fires, err := hourly.Between(at("2026-08-30T05:00:00Z"), at("2026-08-30T00:00:00Z")); err != nil || fires != nil {
		t.Fatalf("reversed window = %v, %v", fires, err)
	}

	// Impossible schedule → empty, no error.
	impossible, _ := ParseSchedule("0 0 0 31 2 *")
	if fires, err := impossible.Between(at("2026-01-01T00:00:00Z"), at("2026-12-31T00:00:00Z")); err != nil || len(fires) != 0 {
		t.Fatalf("impossible between = %v, %v", fires, err)
	}

	// Cap: ~2 years of hourly fires exceeds 10 000 → ErrBetweenCap.
	if _, err := hourly.Between(at("2024-01-01T00:00:00Z"), at("2026-01-01T00:00:00Z")); err != nil && !errors.Is(err, ErrBetweenCap) {
		t.Fatalf("two-year between err = %v", err)
	}
	if _, err := hourly.Between(at("2024-01-01T00:00:00Z"), at("2026-01-01T00:00:00Z")); !errors.Is(err, ErrBetweenCap) {
		t.Fatalf("want ErrBetweenCap over a >10 000 window, got %v", err)
	}

	// Exactly 10 000 fits; one more errors.
	minute, _ := ParseSchedule("0 */1 * * * *") // every minute
	// 10 000 minutes = 6 days 22:40
	_, err = minute.Between(at("2026-01-01T00:00:00Z"), at("2026-01-07T22:38:00Z")) // 9999 fires (6d22h38m = 9998 min after start
	if err != nil {
		t.Fatalf("9999 fires: %v", err)
	}
	if _, err := minute.Between(at("2026-01-01T00:00:00Z"), at("2026-01-07T22:40:00Z")); !errors.Is(err, ErrBetweenCap) {
		t.Fatalf("10001 fires should hit the cap, got %v", err)
	}
}

// TestTieSlots pins the weekly/daily tie: @weekly and @daily both fire
// Sunday 00:00:00 UTC (§8.6 tie rule inputs).
func TestTieSlots(t *testing.T) {
	weekly, _ := ParseSchedule("@weekly")
	daily, _ := ParseSchedule("@daily")
	w, _ := weekly.Between(at("2026-08-01T00:00:00Z"), at("2026-09-01T00:00:00Z"))
	d, _ := daily.Between(at("2026-08-01T00:00:00Z"), at("2026-09-01T00:00:00Z"))
	var shared []time.Time
	for _, x := range w {
		for _, y := range d {
			if x.Equal(y) {
				shared = append(shared, x)
			}
		}
	}
	// Every weekly fire instant is also a daily fire instant — the §8.6 tie.
	// The worked week's tie is Sunday 2026-08-30T00:00:00Z.
	if len(shared) != len(w) || !shared[len(shared)-1].Equal(at("2026-08-30T00:00:00Z")) {
		t.Fatalf("tie slots = %v, want every weekly slot to coincide with a daily slot", shared)
	}
	for _, s := range shared {
		if s.Weekday() != time.Sunday || s.Hour() != 0 {
			t.Fatalf("tie slot %s is not Sunday 00:00", s)
		}
	}
}
