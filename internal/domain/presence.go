package domain

import "time"

// PresenceState is a snapshot of the presence tracker for /status, the
// /away and /here replies and the daemon's status line.
type PresenceState struct {
	// Enabled mirrors the quiet.enabled option.
	Enabled bool
	// Supported is false when the platform has no input idle source; the
	// automatic verdict is then always "away".
	Supported bool
	// AtDesk is the latest automatic verdict: input seen within the idle
	// threshold.
	AtDesk bool
	// ManualAway is set by /away and cleared by /here or by Until passing.
	ManualAway bool
	// ManualHere is set by /here on a platform without an idle source:
	// quiet on by hand until /away.
	ManualHere bool
	// Until is when a timed /away expires; zero for "until /here".
	Until time.Time
	// Quiet is the effective flag: Enabled && !ManualAway && (ManualHere ||
	// (Supported && AtDesk)).
	Quiet bool
}

// Word is the one-token form for the daemon's status line: off (quiet mode
// disabled, or unsupported with no manual presence), on (quiet in force),
// away (automatic verdict), away-manual (/away in force).
func (s PresenceState) Word() string {
	switch {
	case !s.Enabled:
		return "off"
	case s.Quiet:
		return "on"
	case !s.Supported:
		return "off"
	case s.ManualAway:
		return "away-manual"
	default:
		return "away"
	}
}
