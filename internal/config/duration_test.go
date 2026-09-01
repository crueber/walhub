package config

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Duration
		wantErr bool
	}{
		{name: "millis", in: "5ms", want: Duration(5 * time.Millisecond)},
		{name: "seconds", in: "20s", want: Duration(20 * time.Second)},
		{name: "zero", in: "0s", want: 0},
		{name: "minutes", in: "10m", want: Duration(10 * time.Minute)},
		{name: "hours", in: "1h", want: Duration(time.Hour)},
		{name: "days", in: "30d", want: Duration(30 * 24 * time.Hour)},
		{name: "week", in: "7d", want: Duration(7 * 24 * time.Hour)},
		{name: "weeks", in: "2w", want: Duration(14 * 24 * time.Hour)},
		{name: "bare integer is seconds", in: "90", want: Duration(90 * time.Second)},
		{name: "compound go form", in: "1h30m", want: Duration(90 * time.Minute)},
		{name: "fractional go form", in: "1.5h", want: Duration(90 * time.Minute)},
		{name: "negative go form", in: "-20s", want: Duration(-20 * time.Second)},
		{name: "space before unit", in: "5 h", want: Duration(5 * time.Hour)},
		{name: "garbage", in: "abc", wantErr: true},
		{name: "unknown unit", in: "5x", wantErr: true},
		{name: "unit only", in: "h", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "overflow", in: "99999999999999999999d", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDurationStringRoundTrip(t *testing.T) {
	tests := map[Duration]string{
		0:                              "0s",
		Duration(time.Hour):            "1h0m0s",
		Duration(5 * time.Millisecond): "5ms",
		Duration(30 * 24 * time.Hour):  "720h0m0s",
	}
	for d, want := range tests {
		if got := d.String(); got != want {
			t.Errorf("Duration(%v).String() = %q, want %q", time.Duration(d), got, want)
		}
		back, err := ParseDuration(want)
		if err != nil || back != d {
			t.Errorf("round-trip %q: got %v, %v", want, back, err)
		}
	}
}

func TestParseDurationOverflowBranches(t *testing.T) {
	for _, in := range []string{"9999999999999999999s", "9223372036854775807"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q): want overflow error", in)
		}
	}
}
