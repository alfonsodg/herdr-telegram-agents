package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// ClaudeUsageFileName is the file under the state dir that the status line
// tap writes and the daemon reads for the Claude quota line.
const ClaudeUsageFileName = "claude-usage.json"

const (
	// claudeTapMaxInput caps the status line JSON the tap parses; larger
	// input is still passed through, only not parsed.
	claudeTapMaxInput = 1 << 20
	// claudeUsageMaxFile caps what the daemon reads back; the tap writes a
	// few hundred bytes.
	claudeUsageMaxFile = 64 << 10
)

// claudeUsageFile is the on-disk shape: the two windows Claude Code hands
// its status line command, plus when the tap saw them. Nothing else from
// the status line input (cwd, session, model, cost) is ever stored.
type claudeUsageFile struct {
	ObservedAt int64         `json:"observed_at"`
	FiveHour   *claudeWindow `json:"five_hour,omitempty"`
	SevenDay   *claudeWindow `json:"seven_day,omitempty"`
}

// claudeWindow is one rate-limit window as Claude Code reports it. Both
// fields are JSON numbers; resets_at is unix seconds and may carry a
// fraction.
type claudeWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *float64 `json:"resets_at"`
}

func (w *claudeWindow) complete() bool {
	return w != nil && w.UsedPercentage != nil && w.ResetsAt != nil
}

// claudeStatusLine is the part of Claude Code's status line input the tap
// reads. rate_limits is missing for API-key logins and before the first
// answer of a session.
type claudeStatusLine struct {
	RateLimits *struct {
		FiveHour *claudeWindow `json:"five_hour"`
		SevenDay *claudeWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

// TapClaudeUsage is the status line tap: it copies in to out byte for byte
// first, so the operator's own status line command chained after it keeps
// working whatever happens next, then stores the rate-limit windows from
// the input at path. Input without rate_limits leaves the file untouched.
// An error never concerns out unless the copy itself failed.
func TapClaudeUsage(in io.Reader, out io.Writer, path string, now func() time.Time) error {
	head, readErr := io.ReadAll(io.LimitReader(in, claudeTapMaxInput+1))
	if _, err := out.Write(head); err != nil {
		return fmt.Errorf("pass status line through: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("pass status line through: %w", err)
	}
	if readErr != nil {
		return fmt.Errorf("read status line: %w", readErr)
	}
	if len(head) > claudeTapMaxInput {
		return fmt.Errorf("status line input over %d bytes, not parsed", claudeTapMaxInput)
	}
	if len(bytes.TrimSpace(head)) == 0 {
		return nil
	}
	var line claudeStatusLine
	if err := json.Unmarshal(head, &line); err != nil {
		return fmt.Errorf("parse status line: %w", err)
	}
	if line.RateLimits == nil {
		return nil
	}
	rec := claudeUsageFile{ObservedAt: now().Unix()}
	if w := line.RateLimits.FiveHour; w.complete() {
		rec.FiveHour = w
	}
	if w := line.RateLimits.SevenDay; w.complete() {
		rec.SevenDay = w
	}
	if rec.FiveHour == nil && rec.SevenDay == nil {
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode claude usage: %w", err)
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

// ClaudeUsageFile implements domain.UsageSource over the file the status
// line tap writes.
type ClaudeUsageFile struct {
	path string
}

var _ domain.UsageSource = (*ClaudeUsageFile)(nil)

// NewClaudeUsageFile reads the tap's file at path.
func NewClaudeUsageFile(path string) *ClaudeUsageFile { return &ClaudeUsageFile{path: path} }

// Path returns the file the source reads, for doctor's setup hint.
func (s *ClaudeUsageFile) Path() string { return s.path }

// Usage returns the windows from the file. A missing file means the tap
// was never installed or has not seen a rate_limits object yet: no data,
// no error.
func (s *ClaudeUsageFile) Usage(context.Context) (domain.Usage, bool, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.Usage{}, false, nil
	}
	if err != nil {
		return domain.Usage{}, false, fmt.Errorf("open %s: %w", ClaudeUsageFileName, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, claudeUsageMaxFile+1))
	if err != nil {
		return domain.Usage{}, false, fmt.Errorf("read %s: %w", ClaudeUsageFileName, err)
	}
	if len(data) > claudeUsageMaxFile {
		return domain.Usage{}, false, fmt.Errorf("%s is over %d bytes", ClaudeUsageFileName, claudeUsageMaxFile)
	}
	var rec claudeUsageFile
	if err := json.Unmarshal(data, &rec); err != nil {
		return domain.Usage{}, false, fmt.Errorf("parse %s: %w", ClaudeUsageFileName, err)
	}
	u := domain.Usage{Provider: domain.UsageClaude, ObservedAt: time.Unix(rec.ObservedAt, 0)}
	for _, w := range []struct {
		label string
		win   *claudeWindow
	}{{"5h", rec.FiveHour}, {"7d", rec.SevenDay}} {
		if !w.win.complete() {
			continue
		}
		u.Windows = append(u.Windows, domain.UsageWindow{
			Label:       w.label,
			UsedPercent: *w.win.UsedPercentage,
			ResetsAt:    unixFloat(*w.win.ResetsAt),
		})
	}
	if len(u.Windows) == 0 {
		return domain.Usage{}, false, nil
	}
	return u, true, nil
}

// unixFloat turns unix seconds with an optional fraction into a time.
func unixFloat(sec float64) time.Time {
	whole := math.Floor(sec)
	return time.Unix(int64(whole), int64((sec-whole)*1e9))
}
