package system

import (
	"bytes"
	"errors"
	"math"
	"regexp"
	"strconv"
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

// mutterIdletime matches gdbus's print of GetIdletime's uint64 reply, in
// milliseconds:
//
//	(uint64 1234,)
var mutterIdletime = regexp.MustCompile(`^\(uint64 (\d+),\)$`)

var errNoMutterIdletime = errors.New("no idle time in gdbus output")

// parseMutterIdletime reads the reply of org.gnome.Mutter.IdleMonitor.GetIdletime.
func parseMutterIdletime(out []byte) (time.Duration, error) {
	m := mutterIdletime.FindSubmatch(bytes.TrimSpace(out))
	if m == nil {
		return 0, errNoMutterIdletime
	}
	ms, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return msDuration(ms)
}

var errNoXprintidle = errors.New("no idle time in xprintidle output")

// parseXprintidle reads xprintidle's answer: milliseconds and a newline.
func parseXprintidle(out []byte) (time.Duration, error) {
	ms, err := strconv.ParseUint(string(bytes.TrimSpace(out)), 10, 64)
	if err != nil {
		return 0, errNoXprintidle
	}
	return msDuration(ms)
}

var errIdleOverflow = errors.New("idle time out of range")

// msDuration converts milliseconds, refusing a value that would wrap into a
// negative Duration (a negative idle time reads as "at the desk").
func msDuration(ms uint64) (time.Duration, error) {
	if ms > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return 0, errIdleOverflow
	}
	return time.Duration(ms) * time.Millisecond, nil
}
