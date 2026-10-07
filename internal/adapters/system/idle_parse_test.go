package system

import (
	"errors"
	"testing"
	"time"
)

func TestParseHIDIdleTime(t *testing.T) {
	real := []byte(`+-o IOHIDSystem  <class IOHIDSystem, id 0x100000456, registered, matched, active, busy 0 (0 ms), retain 12>
    {
      "IOClass" = "IOHIDSystem"
      "HIDIdleTime" = 194607375
      "HIDParameters" = {"HIDPointerAcceleration"=45056}
    }
`)
	cases := []struct {
		name string
		in   []byte
		want time.Duration
		err  error
	}{
		{"real excerpt", real, 194607375 * time.Nanosecond, nil},
		{"zero", []byte(`"HIDIdleTime" = 0`), 0, nil},
		{"first match wins", []byte("\"HIDIdleTime\" = 5000000000\n\"HIDIdleTime\" = 1"), 5 * time.Second, nil},
		{"missing", []byte(`"HIDParameters" = {}`), 0, errNoHIDIdleTime},
		{"garbage", []byte("nothing here"), 0, errNoHIDIdleTime},
	}
	for _, tc := range cases {
		got, err := parseHIDIdleTime(tc.in)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: parseHIDIdleTime = %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestParseMutterIdletime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
		err  error
	}{
		{"gdbus reply", "(uint64 1234,)\n", 1234 * time.Millisecond, nil},
		{"zero", "(uint64 0,)", 0, nil},
		{"hours", "(uint64 7200000,)", 2 * time.Hour, nil},
		{"wrong type", "(uint32 5,)", 0, errNoMutterIdletime},
		{"error text", "Error: GDBus.Error:org.freedesktop.DBus.Error.ServiceUnknown", 0, errNoMutterIdletime},
		{"empty", "", 0, errNoMutterIdletime},
		{"overflow", "(uint64 18446744073709551615,)", 0, errIdleOverflow},
	}
	for _, tc := range cases {
		got, err := parseMutterIdletime([]byte(tc.in))
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: parseMutterIdletime = %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestParseXprintidle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
		err  error
	}{
		{"with newline", "4567\n", 4567 * time.Millisecond, nil},
		{"zero", "0", 0, nil},
		{"negative", "-1", 0, errNoXprintidle},
		{"display error", "couldn't open display", 0, errNoXprintidle},
		{"empty", "", 0, errNoXprintidle},
		{"overflow", "9223372036854775807", 0, errIdleOverflow},
	}
	for _, tc := range cases {
		got, err := parseXprintidle([]byte(tc.in))
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: parseXprintidle = %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}
