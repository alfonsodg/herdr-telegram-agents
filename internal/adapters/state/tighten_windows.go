//go:build windows

package state

import "log/slog"

// TightenPermissions is a no-op on Windows, where Unix mode bits carry no
// meaning; the per-user profile directories already restrict access.
func TightenPermissions(dirs, files []string, log *slog.Logger) int {
	return 0
}
