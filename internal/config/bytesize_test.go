package config

import "testing"

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    ByteSize
		wantErr bool
	}{
		{name: "gib", in: "20GiB", want: 20 << 30},
		{name: "mib", in: "64MiB", want: 64 << 20},
		{name: "zero", in: "0B", want: 0},
		{name: "kib", in: "1KiB", want: 1 << 10},
		{name: "tib", in: "1TiB", want: 1 << 40},
		{name: "lowercase", in: "64mib", want: 64 << 20},
		{name: "uppercase", in: "5KIB", want: 5 << 10},
		{name: "no i suffix", in: "512MB", want: 512 << 20},
		{name: "kb binary", in: "2KB", want: 2 << 10},
		{name: "bare integer is bytes", in: "512", want: 512},
		{name: "fractional exact", in: "1.5GiB", want: 1610612736},
		{name: "garbage", in: "abc", wantErr: true},
		{name: "unknown unit", in: "1XB", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "fractional bytes", in: "1.5B", wantErr: true},
		{name: "space before unit", in: "20 GiB", want: 20 << 30},
		{name: "overflow", in: "99999999999TiB", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseByteSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseByteSize(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteSize(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestByteSizeString(t *testing.T) {
	tests := map[ByteSize]string{
		0:         "0B",
		512:       "512B",
		1 << 10:   "1KiB",
		5 << 20:   "5MiB",
		20 << 30:  "20GiB",
		1 << 40:   "1TiB",
		1<<30 + 1: "1073741825B",
	}
	for b, want := range tests {
		if got := b.String(); got != want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(b), got, want)
		}
	}
}

func TestParseByteSizeParseFloatError(t *testing.T) {
	// Huge mantissa: regex matches, ParseFloat overflows.
	if _, err := ParseByteSize("1e999GiB"); err == nil {
		t.Error("want overflow error")
	}
}
