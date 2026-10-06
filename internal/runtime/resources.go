package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// Resources are the cgroup limits for a container. Zero values mean no limit.
type Resources struct {
	// Memory is the most memory the container may use, in bytes. The
	// container may also use the same amount of swap, as with Docker's
	// default.
	Memory int64 `json:"memory,omitempty"`
	// CPUs is how many CPUs' worth of time the container may use, such as
	// 1.5.
	CPUs float64 `json:"cpus,omitempty"`
	// PidsLimit is the most processes and threads the container may have.
	PidsLimit int64 `json:"pidsLimit,omitempty"`
	// Per-device disk I/O limits, in bytes or operations per second.
	DeviceReadBps   []DeviceLimit `json:"deviceReadBps,omitempty"`
	DeviceWriteBps  []DeviceLimit `json:"deviceWriteBps,omitempty"`
	DeviceReadIOPS  []DeviceLimit `json:"deviceReadIOPS,omitempty"`
	DeviceWriteIOPS []DeviceLimit `json:"deviceWriteIOPS,omitempty"`
}

// IsZero reports whether no limits are set.
func (r Resources) IsZero() bool {
	return r.Memory == 0 && r.CPUs == 0 && r.PidsLimit == 0 &&
		len(r.DeviceReadBps) == 0 && len(r.DeviceWriteBps) == 0 &&
		len(r.DeviceReadIOPS) == 0 && len(r.DeviceWriteIOPS) == 0
}

// DeviceLimit is a rate limit for one block device, such as /dev/sda.
type DeviceLimit struct {
	Path string `json:"path"`
	Rate uint64 `json:"rate"`
}

// ParseBytes parses a size such as "512m", "1g", or "1048576". Units are
// powers of 1024 and may end in an optional "b", as in Docker ("512mb").
func ParseBytes(s string) (int64, error) {
	text := strings.ToLower(strings.TrimSpace(s))
	text = strings.TrimSuffix(text, "b")
	multiplier := int64(1)
	if text != "" {
		switch text[len(text)-1] {
		case 'k':
			multiplier = 1 << 10
		case 'm':
			multiplier = 1 << 20
		case 'g':
			multiplier = 1 << 30
		case 't':
			multiplier = 1 << 40
		}
		if multiplier != 1 {
			text = text[:len(text)-1]
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid size %q: want a positive number with an optional unit (k, m, g, t)", s)
	}
	if value > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return value * multiplier, nil
}

// ParseDeviceLimit parses "PATH:RATE", such as "/dev/sda:1mb". With bytes
// set, RATE may have a size unit; otherwise it is a plain count.
func ParseDeviceLimit(s string, bytes bool) (DeviceLimit, error) {
	path, rateText, ok := strings.Cut(s, ":")
	if !ok || path == "" || rateText == "" {
		return DeviceLimit{}, fmt.Errorf("invalid device limit %q: want PATH:RATE, such as /dev/sda:1mb", s)
	}
	if bytes {
		rate, err := ParseBytes(rateText)
		if err != nil {
			return DeviceLimit{}, fmt.Errorf("device limit %q: %w", s, err)
		}
		return DeviceLimit{Path: path, Rate: uint64(rate)}, nil
	}
	rate, err := strconv.ParseUint(rateText, 10, 64)
	if err != nil || rate == 0 {
		return DeviceLimit{}, fmt.Errorf("invalid device limit %q: want a positive number of operations per second", s)
	}
	return DeviceLimit{Path: path, Rate: rate}, nil
}
