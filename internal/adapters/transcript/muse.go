package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// kindMuse is the Herdr agent kind this reader understands.
const kindMuse = "muse"

// museSessionIDRe matches a session id copied into a file path; anything
// else is refused rather than globbed.
var museSessionIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// MuseReader implements domain.ReplySource for Muse Code. Herdr reports no
// session tuple for muse, so the reader targets the pane's session through
// the runtime file whose workspace_label matches the working directory's
// basename and returns the newest complete assistant message from the
// session's durable log. The log is large, so only its tail is scanned.
type MuseReader struct {
	home    func() (string, error)
	now     func() time.Time
	log     *slog.Logger
	maxScan int64
}

// NewMuseReader returns a reader over the current user's home directory.
func NewMuseReader(log *slog.Logger) *MuseReader {
	return newMuseReader(os.UserHomeDir, time.Now, log)
}

// newMuseReader takes the home and clock sources so tests can point the
// reader at a temporary directory.
func newMuseReader(home func() (string, error), now func() time.Time, log *slog.Logger) *MuseReader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &MuseReader{home: home, now: now, log: log, maxScan: defaultMaxScan}
}

// LastReply returns the newest assistant message Muse recorded for the
// pane's session. Every failure is domain.ErrNoReply wrapped with a reason.
func (r *MuseReader) LastReply(ctx context.Context, agent domain.Agent) (domain.Reply, error) {
	if err := ctx.Err(); err != nil {
		return domain.Reply{}, err
	}
	if agent.Kind != kindMuse {
		return domain.Reply{}, fmt.Errorf("%w: unsupported agent %q", domain.ErrUnsupportedAgent, agent.Kind)
	}
	if strings.TrimSpace(agent.Cwd) == "" {
		return domain.Reply{}, fmt.Errorf("%w: agent has no working directory", domain.ErrNoReply)
	}
	home, err := r.home()
	if err != nil {
		return domain.Reply{}, fmt.Errorf("%w: home directory: %v", domain.ErrNoReply, err)
	}
	id, err := museSessionFor(home, agent.Cwd)
	if err != nil {
		return domain.Reply{}, err
	}
	path, err := museLogPath(home, id)
	if err != nil {
		return domain.Reply{}, err
	}
	text, written, err := lastMuseMessage(path, r.maxScan)
	if err != nil {
		return domain.Reply{}, err
	}
	age := r.now().Sub(written)
	r.log.Debug("muse log scanned", slog.String("pane", agent.PaneID),
		slog.Int("chars", len(text)), slog.Int64("age_ms", age.Milliseconds()))
	return domain.Reply{Text: text, Source: "muse session log", Age: age, Written: written}, nil
}

// museSessionFor picks the newest runtime session whose workspace label is
// the basename of the pane's working directory.
func museSessionFor(home, cwd string) (string, error) {
	dir := filepath.Join(home, ".local", "share", "muse", "runtime", "muse", "sessions")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("%w: no muse runtime session", domain.ErrNoReply)
	}
	want := filepath.Base(filepath.Clean(cwd))
	best, bestAt := "", time.Time{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var s struct {
			SessionID      string `json:"session_id"`
			WorkspaceLabel string `json:"workspace_label"`
		}
		if json.Unmarshal(data, &s) != nil || !museSessionIDRe.MatchString(s.SessionID) || s.WorkspaceLabel != want {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestAt) {
			best, bestAt = s.SessionID, info.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("%w: no muse session for %q", domain.ErrNoReply, want)
	}
	return best, nil
}

// museLogPath finds the durable log of one session. Session directories are
// date-partitioned, so the lexically last match is the newest.
func museLogPath(home, id string) (string, error) {
	pattern := filepath.Join(home, ".local", "share", "muse", "sessions", "*", "*", "*", id, "session.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("%w: muse session log not found", domain.ErrNoReply)
	}
	return files[len(files)-1], nil
}

// museRecord is the part of one durable-log line this reader uses.
type museRecord struct {
	RecordedAt int64 `json:"recorded_at"`
	Payload    struct {
		Event struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"event"`
	} `json:"payload"`
}

// lastMuseMessage scans the tail of a session log and returns the newest
// complete assistant message and when it was recorded. A scan that starts
// in the middle of a line drops that partial line.
func lastMuseMessage(path string, maxScan int64) (string, time.Time, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: session log unreadable", domain.ErrNoReply)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: session log unreadable", domain.ErrNoReply)
	}
	start := int64(0)
	if size := info.Size(); size > maxScan {
		start = size - maxScan
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: session log unreadable", domain.ErrNoReply)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: session log unreadable", domain.ErrNoReply)
	}
	lines := strings.Split(string(data), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	text := ""
	var written time.Time
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec museRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Payload.Event.Kind != "assistant_message_committed" {
			continue
		}
		if t := strings.TrimSpace(rec.Payload.Event.Text); t != "" {
			text = t
			written = time.UnixMicro(rec.RecordedAt)
		}
	}
	if text == "" {
		return "", time.Time{}, fmt.Errorf("%w: no assistant message in the session log", domain.ErrNoReply)
	}
	return text, written, nil
}
