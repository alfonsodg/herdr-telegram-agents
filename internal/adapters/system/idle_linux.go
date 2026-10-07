//go:build linux

package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// idleFor walks the Linux chain (Mutter over gdbus, then xprintidle) built
// from the live environment; see linuxProbes.
func idleFor(ctx context.Context) (time.Duration, string, error) {
	return sampleChain(ctx, linuxProbes(os.LookupEnv, exec.LookPath, os.Stat, runHelper))
}

// runHelper starts a helper through command() and returns its stdout; a
// failure carries the helper's stderr so the log says why (no session bus,
// gnome-shell not on the bus, no display).
func runHelper(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := command(ctx, name, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
				return nil, fmt.Errorf("%w: %s", err, msg)
			}
		}
		return nil, err
	}
	return out, nil
}
