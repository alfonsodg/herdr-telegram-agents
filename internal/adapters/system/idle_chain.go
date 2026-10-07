package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// idleProbe is one way to read the input idle time: available says whether
// its helper and session exist at all, run asks it once, parse reads the
// answer.
type idleProbe struct {
	name      string
	available func() bool
	run       func(ctx context.Context) ([]byte, error)
	parse     func(out []byte) (time.Duration, error)
}

// sampleChain asks the available probes in order; the first answer wins and
// is returned with the probe's name. No available probe means the platform
// has no source (ErrIdleUnsupported, final for the tracker). Probes that
// exist but fail give a plain error naming each one, so a transient failure
// keeps the previous verdict instead of switching quiet mode off.
func sampleChain(ctx context.Context, probes []idleProbe) (time.Duration, string, error) {
	var errs []error
	for _, p := range probes {
		if !p.available() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, "", fmt.Errorf("%s: %w", p.name, err)
		}
		out, err := p.run(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
			continue
		}
		d, err := p.parse(out)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
			continue
		}
		return d, p.name, nil
	}
	if len(errs) == 0 {
		return 0, "", domain.ErrIdleUnsupported
	}
	return 0, "", probeErrors(errs)
}

// probeErrors is every failed probe's reason on one line for the log;
// errors.Is and errors.As see each of them.
type probeErrors []error

func (e probeErrors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

func (e probeErrors) Unwrap() []error { return e }

// runFunc starts a helper and returns its stdout.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// linuxProbes builds the Linux chain: GNOME's Mutter IdleMonitor over the
// session bus (Wayland and X11), then xprintidle (X11; under XWayland it
// sees only X input and fails safe as "away"). The ScreenSaver interface is
// left out: its GetSessionIdleTime unit differs between desktops and Plasma
// on Wayland refuses it. Availability reads only the environment and PATH;
// whether the service answers is a runtime failure, never "unsupported".
// The parameters are seams for tests; production passes os and exec.
func linuxProbes(lookup Lookup, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error), run runFunc) []idleProbe {
	set := func(key string) bool {
		v, ok := lookup(key)
		return ok && v != ""
	}
	onPath := func(name string) bool {
		_, err := lookPath(name)
		return err == nil
	}
	sessionBus := func() bool {
		if set("DBUS_SESSION_BUS_ADDRESS") {
			return true
		}
		dir, ok := lookup("XDG_RUNTIME_DIR")
		if !ok || dir == "" {
			return false
		}
		_, err := stat(filepath.Join(dir, "bus"))
		return err == nil
	}
	return []idleProbe{
		{
			name: "mutter",
			available: func() bool {
				return (set("WAYLAND_DISPLAY") || set("DISPLAY")) && sessionBus() && onPath("gdbus")
			},
			run: func(ctx context.Context) ([]byte, error) {
				return run(ctx, "gdbus", mutterIdleArgs...)
			},
			parse: parseMutterIdletime,
		},
		{
			name: "xprintidle",
			available: func() bool {
				return set("DISPLAY") && onPath("xprintidle")
			},
			run: func(ctx context.Context) ([]byte, error) {
				return run(ctx, "xprintidle")
			},
			parse: parseXprintidle,
		},
	}
}

// mutterIdleArgs asks GNOME Shell for the time since the last input, in
// milliseconds. The method is GetIdletime (lower-case t) in Mutter's
// interface XML.
var mutterIdleArgs = []string{
	"call", "--session",
	"--dest", "org.gnome.Mutter.IdleMonitor",
	"--object-path", "/org/gnome/Mutter/IdleMonitor/Core",
	"--method", "org.gnome.Mutter.IdleMonitor.GetIdletime",
}
