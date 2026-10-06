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

func TestParseIdleMillis(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
		ok   bool
	}{
		{"dbus uint64", "method return time=1759760000.1 sender=:1.5 -> destination=:1.9 serial=5 reply_serial=2\n   uint64 1234\n", 1234 * time.Millisecond, true},
		{"dbus uint32 zero", "method return time=1 sender=:1.5 -> destination=:1.9 serial=5 reply_serial=2\n   uint32 0\n", 0, true},
		{"xprintidle", "12345\n", 12345 * time.Millisecond, true},
		{"empty", "", 0, false},
		{"error text", "Error org.freedesktop.DBus.Error.NotSupported: nope\n", 0, false},
		{"garbage tail", "uint64 nope\n", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseIdleMillis([]byte(tc.in))
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: parseIdleMillis = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
