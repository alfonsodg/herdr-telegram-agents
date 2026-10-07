//go:build linux

package system

import (
	"context"
	"os/exec"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// linuxDBusIdleQueries are the session idle-time queries tried in order on
// Linux; the first answer wins. GNOME's Mutter idle monitor and the
// freedesktop ScreenSaver idle time cover GNOME and most X11 desktops.
var linuxDBusIdleQueries = []struct{ dest, path, method string }{
	{"org.gnome.Mutter.IdleMonitor", "/org/gnome/Mutter/IdleMonitor/Core", "org.gnome.Mutter.IdleMonitor.GetIdleTime"},
	{"org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver", "org.freedesktop.ScreenSaver.GetSessionIdleTime"},
}

// idleFor samples the machine's input idle time on Linux. There is no
// portable interface: it asks the known session sources in turn and reports
// ErrIdleUnsupported when none answers (headless machines, KDE Wayland
// today), so the automatic verdict there is always "away". The presence
// tracker retries an unsupported platform once a minute in case a source
// appears, and /here turns quiet on by hand until /away.
func idleFor(ctx context.Context) (time.Duration, error) {
	for _, q := range linuxDBusIdleQueries {
		if d, ok := dbusIdleTime(ctx, q.dest, q.path, q.method); ok {
			return d, nil
		}
	}
	if d, ok := xprintIdle(ctx); ok {
		return d, nil
	}
	return 0, domain.ErrIdleUnsupported
}

// dbusIdleTime asks one D-Bus method through dbus-send. A missing helper,
// an absent session bus or a method the platform does not support all mean
// "try the next source".
func dbusIdleTime(ctx context.Context, dest, path, method string) (time.Duration, bool) {
	bin, err := exec.LookPath("dbus-send")
	if err != nil {
		return 0, false
	}
	out, err := command(ctx, bin, "--session", "--print-reply", "--dest="+dest, path, method).Output()
	if err != nil {
		return 0, false
	}
	return parseIdleMillis(out)
}

// xprintIdle is the traditional X11 source; absent without X11 tools.
func xprintIdle(ctx context.Context) (time.Duration, bool) {
	bin, err := exec.LookPath("xprintidle")
	if err != nil {
		return 0, false
	}
	out, err := command(ctx, bin).Output()
	if err != nil {
		return 0, false
	}
	return parseIdleMillis(out)
}
