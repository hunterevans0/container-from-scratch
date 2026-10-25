package runtime

// Tests for resources.go: parsing --memory and the disk I/O limits. For how
// Go tests work, see "Tests" in doc.go.

import "testing"

// TestParseBytes checks that ParseBytes accepts plain numbers and b/k/m/g/t
// suffixes (in either case, with an optional trailing B), and rejects empty,
// zero, negative, fractional, unknown-unit, and overflowing sizes.
func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1048576", 1048576},
		{"100b", 100},
		{"4k", 4 << 10},
		{"512m", 512 << 20},
		{"512MB", 512 << 20},
		{"2g", 2 << 30},
		{"1t", 1 << 40},
	}
	for _, test := range tests {
		got, err := ParseBytes(test.in)
		if err != nil || got != test.want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", test.in, got, err, test.want)
		}
	}
	for _, in := range []string{"", "m", "0", "-1m", "1.5g", "12x", "99999999999t"} {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) = %d; want an error", in, got)
		}
	}
}

// TestParseDeviceLimit checks that ParseDeviceLimit parses PATH:RATE for both
// byte-rate and IOPS limits, rejects a missing separator, path, or rate, a zero
// rate, and a unit suffix on an IOPS limit.
func TestParseDeviceLimit(t *testing.T) {
	limit, err := ParseDeviceLimit("/dev/sda:1mb", true)
	if err != nil || limit != (DeviceLimit{Path: "/dev/sda", Rate: 1 << 20}) {
		t.Errorf("bytes limit = %+v, %v", limit, err)
	}
	limit, err = ParseDeviceLimit("/dev/sdb:250", false)
	if err != nil || limit != (DeviceLimit{Path: "/dev/sdb", Rate: 250}) {
		t.Errorf("IOPS limit = %+v, %v", limit, err)
	}
	for _, in := range []string{"/dev/sda", ":1mb", "/dev/sda:", "/dev/sda:0"} {
		if _, err := ParseDeviceLimit(in, true); err == nil {
			t.Errorf("ParseDeviceLimit(%q) succeeded; want an error", in)
		}
	}
	if _, err := ParseDeviceLimit("/dev/sda:1mb", false); err == nil {
		t.Error("IOPS limit with a unit succeeded; want an error")
	}
}
