package app

import (
	"context"
	"html"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// QuotaSource names one provider's usage source; the quota lines follow
// the order the sources are given in.
type QuotaSource struct {
	Provider string
	Source   domain.UsageSource
}

// quotaEntry is what Quota remembers of one source between reads.
type quotaEntry struct {
	usage domain.Usage
	has   bool
	// ready marks that the source produced data once, for the one info
	// line when it first does.
	ready bool
	// errText is the last error, so a repeated failure logs at debug.
	errText string
}

// Quota renders the usage lines under the dashboard and /status: one line
// per provider with data, its windows with the percentage used and the
// time left until each starts over. Sources are read at most once per
// quotaRefresh; a source that fails keeps its last good numbers. Lines is
// safe from the daemon loop and the bridge at once.
type Quota struct {
	sources []QuotaSource
	opts    *Options
	clock   domain.Clock
	log     *slog.Logger

	mu      sync.Mutex
	entries []quotaEntry
	read    time.Time
}

// NewQuota wires the quota lines over sources, in display order.
func NewQuota(sources []QuotaSource, opts *Options, clock domain.Clock, log *slog.Logger) *Quota {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts == nil {
		opts = NewOptions(domain.DefaultOptions(), nil, nil, log)
	}
	return &Quota{sources: sources, opts: opts, clock: clock, log: log, entries: make([]quotaEntry, len(sources))}
}

// Lines returns the HTML quota lines without a trailing newline, or ""
// when the option is off or no provider has a window left to show.
func (q *Quota) Lines(ctx context.Context) string {
	if q == nil || len(q.sources) == 0 || !q.opts.QuotaEnabled() {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock.Now()
	if q.read.IsZero() || now.Sub(q.read) >= quotaRefresh {
		q.refresh(ctx)
		q.read = now
	}
	return q.render(now)
}

// refresh reads every source once.
func (q *Quota) refresh(ctx context.Context) {
	for i, src := range q.sources {
		e := &q.entries[i]
		u, ok, err := src.Source.Usage(ctx)
		if err != nil {
			if text := err.Error(); text != e.errText {
				q.log.Warn("quota source failed", slog.String("provider", src.Provider), slog.String("err", text))
				e.errText = text
			} else {
				q.log.Debug("quota source failed again", slog.String("provider", src.Provider), slog.String("err", text))
			}
			continue
		}
		e.errText = ""
		if !ok {
			if e.has {
				q.log.Debug("quota source has no data any more", slog.String("provider", src.Provider))
			}
			e.has = false
			continue
		}
		if !e.ready {
			q.log.Info("quota source ready", slog.String("provider", src.Provider))
			e.ready = true
		}
		e.usage, e.has = u, true
		q.log.Debug("quota read", slog.String("provider", src.Provider), slog.Int("windows", len(u.Windows)),
			slog.Duration("age", q.clock.Now().Sub(u.ObservedAt).Round(time.Second)))
	}
}

// render builds one line per provider from the remembered numbers.
// Windows past their reset are dropped: their percentage no longer holds.
func (q *Quota) render(now time.Time) string {
	var lines []string
	for i, src := range q.sources {
		e := q.entries[i]
		if !e.has {
			continue
		}
		var parts []string
		for _, w := range e.usage.Windows {
			if !w.ResetsAt.After(now) {
				continue
			}
			parts = append(parts, html.EscapeString(w.Label)+" "+quotaPercent(w.UsedPercent)+" ↻"+quotaLeft(w.ResetsAt.Sub(now)))
		}
		if len(parts) == 0 {
			continue
		}
		line := "🔑 " + html.EscapeString(src.Provider) + " " + strings.Join(parts, " · ")
		if at := e.usage.ObservedAt; !at.IsZero() && now.Sub(at) > quotaStale {
			line += " · as of " + wallClock(q.clock, at)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// quotaPercent rounds a used percentage to a whole number.
func quotaPercent(p float64) string {
	if math.IsNaN(p) || p < 0 {
		p = 0
	}
	return strconv.Itoa(int(math.Round(p))) + "%"
}

// quotaLeft words the time to a reset coarsely, so the dashboard is not
// edited every minute for it: "N min" under an hour, "N h" under a day,
// "N d M h" beyond.
func quotaLeft(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(max(1, int(d/time.Minute))) + " min"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	default:
		days := int(d / (24 * time.Hour))
		hours := int((d % (24 * time.Hour)) / time.Hour)
		return strconv.Itoa(days) + " d " + strconv.Itoa(hours) + " h"
	}
}
