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

// kindAgy is the Herdr agent kind this reader understands.
const kindAgy = "agy"

// agyConversationRe matches a conversation id copied into a file path;
// anything else is refused rather than joined.
var agyConversationRe = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// AgyReader implements domain.ReplySource for Antigravity. Herdr reports
// the conversation id as the pane's agent_session, and the CLI keeps a
// clean JSONL transcript per conversation under
// ~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/.
type AgyReader struct {
	session func(context.Context, string) (domain.SessionTuple, error)
	home    func() (string, error)
	now     func() time.Time
	log     *slog.Logger
	maxScan int64
}

// NewAgyReader wires the reader over Herdr's session lookup.
func NewAgyReader(session func(context.Context, string) (domain.SessionTuple, error), log *slog.Logger) *AgyReader {
	return newAgyReader(session, os.UserHomeDir, time.Now, log)
}

// newAgyReader takes home and clock sources so tests can point the reader
// at a temporary directory.
func newAgyReader(session func(context.Context, string) (domain.SessionTuple, error), home func() (string, error), now func() time.Time, log *slog.Logger) *AgyReader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &AgyReader{session: session, home: home, now: now, log: log, maxScan: defaultMaxScan}
}

// LastReply returns the newest complete model answer Antigravity recorded
// for the pane's conversation. Every failure is domain.ErrNoReply wrapped
// with a reason; a running turn answers domain.ErrReplyPending.
func (r *AgyReader) LastReply(ctx context.Context, agent domain.Agent) (domain.Reply, error) {
	if err := ctx.Err(); err != nil {
		return domain.Reply{}, err
	}
	if agent.Kind != kindAgy {
		return domain.Reply{}, fmt.Errorf("%w: unsupported agent %q", domain.ErrNoReply, agent.Kind)
	}
	tuple, err := r.session(ctx, agent.PaneID)
	if err != nil {
		return domain.Reply{}, fmt.Errorf("%w: session lookup failed", domain.ErrNoReply)
	}
	if tuple.Agent != kindAgy || tuple.Kind != "id" || !agyConversationRe.MatchString(tuple.Value) {
		return domain.Reply{}, fmt.Errorf("%w: herdr reports no agy conversation id for the pane", domain.ErrNoReply)
	}
	if digest := tuple.Digest(); digest == "" || agent.SessionDigest == "" || digest != agent.SessionDigest {
		return domain.Reply{}, fmt.Errorf("%w: the pane now runs another conversation", domain.ErrNoReply)
	}
	home, err := r.home()
	if err != nil {
		return domain.Reply{}, fmt.Errorf("%w: home directory: %v", domain.ErrNoReply, err)
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", tuple.Value, ".system_generated", "logs")
	var text string
	var written time.Time
	found := false
	for _, name := range []string{"transcript.jsonl", "transcript_full.jsonl"} {
		got, when, state, err := scanAgyFile(filepath.Join(dir, name), r.maxScan)
		if err != nil {
			continue
		}
		if state == agyPending {
			return domain.Reply{}, fmt.Errorf("%w: the turn is still running", domain.ErrReplyPending)
		}
		if state == agyFound {
			text, written, found = got, when, true
			break
		}
	}
	if !found {
		return domain.Reply{}, fmt.Errorf("%w: no agy answer in the transcript", domain.ErrNoReply)
	}
	age := r.now().Sub(written)
	r.log.Debug("agy transcript scanned", slog.String("pane", agent.PaneID),
		slog.Int("chars", len(text)), slog.Int64("age_ms", age.Milliseconds()))
	return domain.Reply{Text: text, Source: "agy transcript", Age: age, Written: written}, nil
}

// agyRecord is the part of one transcript line this reader uses.
type agyRecord struct {
	Type      string `json:"type"`
	Source    string `json:"source"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// Scan states for one transcript variant.
const (
	agyFound = iota
	agyPending
	agyEmpty
)

// scanAgyFile reads the tail of one transcript variant and returns the
// newest complete model answer, or a pending state when tool activity or a
// newer prompt follows it (the turn is still running).
func scanAgyFile(path string, maxScan int64) (string, time.Time, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, agyEmpty, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", time.Time{}, agyEmpty, err
	}
	start := int64(0)
	if size := info.Size(); size > maxScan {
		start = size - maxScan
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", time.Time{}, agyEmpty, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", time.Time{}, agyEmpty, err
	}
	lines := strings.Split(string(data), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	text := ""
	var written time.Time
	invalidated := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec agyRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case rec.Type == "PLANNER_RESPONSE" && rec.Source == "MODEL" && strings.TrimSpace(rec.Content) != "":
			text = strings.TrimSpace(rec.Content)
			written = agyTime(rec.CreatedAt)
			invalidated = false
		case text != "" && (rec.Type == "GENERIC" || rec.Type == "USER_INPUT" || rec.Type == "PLANNER_RESPONSE"):
			invalidated = true
		}
	}
	switch {
	case text == "":
		return "", time.Time{}, agyEmpty, nil
	case invalidated:
		return text, written, agyPending, nil
	}
	return text, written, agyFound, nil
}

// agyTime parses the transcript's RFC 3339 timestamp; a bad value stays
// zero and the caller's freshness check treats the reply as stale.
func agyTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
