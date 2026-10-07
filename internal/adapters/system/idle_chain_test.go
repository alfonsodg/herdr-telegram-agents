package system

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// probe builds a test probe that records its runs and parses milliseconds.
func probe(name string, avail bool, out string, runErr error, runs *[]string) idleProbe {
	return idleProbe{
		name:      name,
		available: func() bool { return avail },
		run: func(context.Context) ([]byte, error) {
			*runs = append(*runs, name)
			return []byte(out), runErr
		},
		parse: parseXprintidle,
	}
}

func TestSampleChain(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name     string
		probes   func(runs *[]string) []idleProbe
		want     time.Duration
		source   string
		runs     []string
		unsupp   bool
		errParts []string
	}{
		{
			name: "first wins",
			probes: func(r *[]string) []idleProbe {
				return []idleProbe{probe("a", true, "1500", nil, r), probe("b", true, "9", nil, r)}
			},
			want: 1500 * time.Millisecond, source: "a", runs: []string{"a"},
		},
		{
			name: "first fails, second answers",
			probes: func(r *[]string) []idleProbe {
				return []idleProbe{probe("a", true, "", boom, r), probe("b", true, "9\n", nil, r)}
			},
			want: 9 * time.Millisecond, source: "b", runs: []string{"a", "b"},
		},
		{
			name: "unavailable is never run",
			probes: func(r *[]string) []idleProbe {
				return []idleProbe{probe("a", false, "1", nil, r), probe("b", true, "2", nil, r)}
			},
			want: 2 * time.Millisecond, source: "b", runs: []string{"b"},
		},
		{
			name: "parse failure falls through",
			probes: func(r *[]string) []idleProbe {
				return []idleProbe{probe("a", true, "garbage", nil, r), probe("b", true, "3", nil, r)}
			},
			want: 3 * time.Millisecond, source: "b", runs: []string{"a", "b"},
		},
		{
			name: "none available is unsupported",
			probes: func(r *[]string) []idleProbe {
				return []idleProbe{probe("a", false, "1", nil, r), probe("b", false, "2", nil, r)}
			},
			unsupp: true,
		},
		{
			name:   "empty chain is unsupported",
			probes: func(*[]string) []idleProbe { return nil },
			unsupp: true,
		},
		{
			name: "all available fail is a plain error",
			probes: func(r *[]string) []idleProbe {
				return []idleProbe{probe("a", true, "", boom, r), probe("b", false, "", nil, r), probe("c", true, "x", nil, r)}
			},
			runs: []string{"a", "c"}, errParts: []string{"a: boom", "c: " + errNoXprintidle.Error()},
		},
	}
	for _, tc := range cases {
		var runs []string
		d, source, err := sampleChain(context.Background(), tc.probes(&runs))
		switch {
		case tc.unsupp:
			if !errors.Is(err, domain.ErrIdleUnsupported) {
				t.Errorf("%s: err = %v; want ErrIdleUnsupported", tc.name, err)
			}
		case tc.errParts != nil:
			if err == nil || errors.Is(err, domain.ErrIdleUnsupported) {
				t.Fatalf("%s: err = %v; want a plain error", tc.name, err)
			}
			for _, part := range tc.errParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: err %q lacks %q", tc.name, err, part)
				}
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("%s: err %q spans lines", tc.name, err)
			}
			if !errors.Is(err, boom) || !errors.Is(err, errNoXprintidle) {
				t.Errorf("%s: err %v hides a probe's error", tc.name, err)
			}
		default:
			if err != nil || d != tc.want || source != tc.source {
				t.Errorf("%s: sampleChain = %v, %q, %v; want %v, %q", tc.name, d, source, err, tc.want, tc.source)
			}
		}
		if tc.runs != nil && !slices.Equal(runs, tc.runs) {
			t.Errorf("%s: ran %v; want %v", tc.name, runs, tc.runs)
		}
	}
}

func TestSampleChainCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var runs []string
	_, _, err := sampleChain(ctx, []idleProbe{probe("a", true, "1", nil, &runs)})
	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrIdleUnsupported) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
	if len(runs) != 0 {
		t.Errorf("ran %v after cancel", runs)
	}
}

// linuxEnv is a fake environment for linuxProbes.
type linuxEnv struct {
	vars  map[string]string
	path  []string
	files []string
	calls [][]string
}

func (e *linuxEnv) probes() []idleProbe {
	lookup := func(k string) (string, bool) { v, ok := e.vars[k]; return v, ok }
	lookPath := func(name string) (string, error) {
		if slices.Contains(e.path, name) {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	stat := func(name string) (os.FileInfo, error) {
		if slices.Contains(e.files, name) {
			return nil, nil
		}
		return nil, fs.ErrNotExist
	}
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		e.calls = append(e.calls, append([]string{name}, args...))
		if name == "gdbus" {
			return []byte("(uint64 4200,)\n"), nil
		}
		return []byte("700\n"), nil
	}
	return linuxProbes(lookup, lookPath, stat, run)
}

func availableNames(probes []idleProbe) []string {
	var names []string
	for _, p := range probes {
		if p.available() {
			names = append(names, p.name)
		}
	}
	return names
}

func TestLinuxProbesAvailability(t *testing.T) {
	both := []string{"gdbus", "xprintidle"}
	cases := []struct {
		name string
		env  linuxEnv
		want []string
	}{
		{"headless with helpers", linuxEnv{vars: map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, path: both}, nil},
		{"wayland gnome", linuxEnv{vars: map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DBUS_SESSION_BUS_ADDRESS": "unix:x"}, path: both}, []string{"mutter"}},
		{"wayland with xwayland", linuxEnv{vars: map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0", "DBUS_SESSION_BUS_ADDRESS": "unix:x"}, path: both}, []string{"mutter", "xprintidle"}},
		{"x11 without bus", linuxEnv{vars: map[string]string{"DISPLAY": ":0"}, path: both}, []string{"xprintidle"}},
		{"bus from XDG_RUNTIME_DIR", linuxEnv{vars: map[string]string{"WAYLAND_DISPLAY": "wayland-0", "XDG_RUNTIME_DIR": "/run/user/1000"}, path: both, files: []string{"/run/user/1000/bus"}}, []string{"mutter"}},
		{"XDG_RUNTIME_DIR without bus socket", linuxEnv{vars: map[string]string{"WAYLAND_DISPLAY": "wayland-0", "XDG_RUNTIME_DIR": "/run/user/1000"}, path: both}, nil},
		{"empty DISPLAY counts as unset", linuxEnv{vars: map[string]string{"DISPLAY": "", "DBUS_SESSION_BUS_ADDRESS": "unix:x"}, path: both}, nil},
		{"no helpers", linuxEnv{vars: map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0", "DBUS_SESSION_BUS_ADDRESS": "unix:x"}}, nil},
	}
	for _, tc := range cases {
		if got := availableNames(tc.env.probes()); !slices.Equal(got, tc.want) {
			t.Errorf("%s: available = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestLinuxProbesRun(t *testing.T) {
	env := &linuxEnv{vars: map[string]string{"DISPLAY": ":0", "DBUS_SESSION_BUS_ADDRESS": "unix:x"}, path: []string{"gdbus", "xprintidle"}}
	d, source, err := sampleChain(context.Background(), env.probes())
	if err != nil || d != 4200*time.Millisecond || source != "mutter" {
		t.Fatalf("sampleChain = %v, %q, %v; want 4.2s from mutter", d, source, err)
	}
	want := []string{"gdbus", "call", "--session", "--dest", "org.gnome.Mutter.IdleMonitor",
		"--object-path", "/org/gnome/Mutter/IdleMonitor/Core", "--method", "org.gnome.Mutter.IdleMonitor.GetIdletime"}
	if len(env.calls) != 1 || !slices.Equal(env.calls[0], want) {
		t.Errorf("calls = %v; want [%v]", env.calls, want)
	}

	env = &linuxEnv{vars: map[string]string{"DISPLAY": ":0"}, path: []string{"gdbus", "xprintidle"}}
	d, source, err = sampleChain(context.Background(), env.probes())
	if err != nil || d != 700*time.Millisecond || source != "xprintidle" {
		t.Fatalf("sampleChain = %v, %q, %v; want 700ms from xprintidle", d, source, err)
	}
	if len(env.calls) != 1 || !slices.Equal(env.calls[0], []string{"xprintidle"}) {
		t.Errorf("calls = %v; want [[xprintidle]]", env.calls)
	}
}
