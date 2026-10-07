//go:build darwin

package system

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// idleFor asks IOKit for the HID idle time. ioreg ships with every macOS
// and needs no permission for this property.
func idleFor(ctx context.Context) (time.Duration, string, error) {
	out, err := command(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4").CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return 0, "", fmt.Errorf("ioreg: %w: %s", err, msg)
		}
		return 0, "", fmt.Errorf("ioreg: %w", err)
	}
	d, err := parseHIDIdleTime(out)
	if err != nil {
		return 0, "", err
	}
	return d, "ioreg", nil
}
