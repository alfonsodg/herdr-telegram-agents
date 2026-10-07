package domain

import (
	"strconv"
	"time"
)

// Usage providers, in the order the quota lines list them.
const (
	UsageClaude = "Claude"
	UsageCodex  = "Codex"
)

// Usage is one provider's rate-limit windows as last seen.
type Usage struct {
	Provider string
	// ObservedAt is when the provider reported these numbers.
	ObservedAt time.Time
	Windows    []UsageWindow
}

// UsageWindow is one rolling limit: Label names its length ("5h", "7d"),
// UsedPercent is how much of it is spent and ResetsAt when it starts over.
type UsageWindow struct {
	Label       string
	UsedPercent float64
	ResetsAt    time.Time
}

// WindowLabel names a window by its length in minutes: whole hours under a
// day as "<N>h", anything else as whole days "<N>d" (rounded up, at least 1).
func WindowLabel(minutes int) string {
	if minutes <= 0 {
		return "?"
	}
	if minutes < 24*60 && minutes%60 == 0 {
		return strconv.Itoa(minutes/60) + "h"
	}
	if minutes < 24*60 {
		return strconv.Itoa(minutes) + "m"
	}
	days := (minutes + 24*60 - 1) / (24 * 60)
	return strconv.Itoa(days) + "d"
}
