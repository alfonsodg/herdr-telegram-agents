package system

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hidIdleTime matches the IOHIDSystem property ioreg prints, in nanoseconds:
//
//	"HIDIdleTime" = 194607375
var hidIdleTime = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

var errNoHIDIdleTime = errors.New("HIDIdleTime not found in ioreg output")

// parseHIDIdleTime extracts the first HIDIdleTime value from ioreg output.
func parseHIDIdleTime(out []byte) (time.Duration, error) {
	m := hidIdleTime.FindSubmatch(out)
	if m == nil {
		return 0, errNoHIDIdleTime
	}
	ns, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}

// parseIdleMillis reads the millisecond count dbus-send or xprintidle
// prints: the value is the last whitespace-separated token of the output.
func parseIdleMillis(out []byte) (time.Duration, bool) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, false
	}
	ms, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}
