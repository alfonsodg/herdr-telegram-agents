//go:build !windows

package state

import (
	"fmt"
	"log/slog"
	"os"
)

// TightenPermissions sets each existing directory in dirs to 0700 and each
// existing regular file in files to 0600 when it is readable or writable
// by group or others, and returns how many it changed. MkdirAll and the
// atomic writes never touch an existing directory, so installs from builds
// that created the state dir 0755 (or a config.json someone chmodded) stay
// loose without this. Paths are checked with Lstat: a symlink, a missing
// path or a path of the wrong kind is skipped, so nothing outside what the
// plugin owns is changed. Failures are logged, never fatal.
func TightenPermissions(dirs, files []string, log *slog.Logger) int {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := 0
	for _, dir := range dirs {
		if tighten(dir, true, 0o700, log) {
			n++
		}
	}
	for _, file := range files {
		if tighten(file, false, 0o600, log) {
			n++
		}
	}
	return n
}

func tighten(path string, dir bool, want os.FileMode, log *slog.Logger) bool {
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Debug("permission check skipped", slog.String("path", path), slog.String("err", err.Error()))
		}
		return false
	}
	if dir && !info.IsDir() || !dir && !info.Mode().IsRegular() {
		return false
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return false
	}
	if err := os.Chmod(path, want); err != nil {
		log.Warn("[FIX] could not tighten permissions", slog.String("path", path),
			slog.String("mode", fmt.Sprintf("%04o", perm)), slog.String("err", err.Error()))
		return false
	}
	log.Info("[FIX] permissions tightened", slog.String("path", path),
		slog.String("from", fmt.Sprintf("%04o", perm)), slog.String("to", fmt.Sprintf("%04o", want)))
	return true
}
