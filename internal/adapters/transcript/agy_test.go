package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const agyConversation = "88276862-5cb0-43b6-8454-a240a8e7a81a"

var agyTuple = domain.SessionTuple{Source: "herdr:antigravity_cli", Agent: "agy", Kind: "id", Value: agyConversation}

// agyFixture writes a miniature Antigravity state tree: the transcript of
// one conversation, optionally only the full variant.
type agyFixture struct {
	home     string
	lines    []string
	fullOnly bool
}

func newAgyFixture(t *testing.T, lines []string) *agyFixture {
	t.Helper()
	f := &agyFixture{home: t.TempDir(), lines: lines}
	dir := filepath.Join(f.home, ".gemini", "antigravity-cli", "brain", agyConversation, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "transcript.jsonl"
	if f.fullOnly {
		name = "transcript_full.jsonl"
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *agyFixture) reader(tuple domain.SessionTuple, err error) *AgyReader {
	lookup := func(context.Context, string) (domain.SessionTuple, error) { return tuple, err }
	return newAgyReader(lookup, func() (string, error) { return f.home, nil }, func() time.Time { return time.Unix(1_791_000_000, 0) }, nil)
}

func agyAgent(digest string) domain.Agent {
	return domain.Agent{Key: domain.Key{PaneID: "p1", SessionDigest: digest}, Kind: "agy", Cwd: "/u/proj"}
}

// agyAnswer is one model response record.
func agyAnswer(at string, text string) string {
	return fmt.Sprintf(`{"type":"PLANNER_RESPONSE","source":"MODEL","content":%q,"created_at":%q,"status":"DONE"}`, text, at)
}

// agyTool is a planner step that only calls tools, or a tool output record.
func agyTool() string {
	return `{"type":"PLANNER_RESPONSE","source":"MODEL","tool_calls":[{"name":"bash"}],"created_at":"2026-10-06T13:00:00-05:00"}`
}

func agyOutput() string {
	return `{"type":"GENERIC","source":"MODEL","content":"The command exited with code 0. Output: hi","created_at":"2026-10-06T13:00:01-05:00"}`
}

func TestAgyLastReplyReturnsLastAnswer(t *testing.T) {
	f := newAgyFixture(t, []string{agyOutput(), agyAnswer("2026-10-06T13:13:14-05:00", "old answer"), agyTool(), agyAnswer("2026-10-06T13:14:14-05:00", "the **full** answer")})
	r, err := f.reader(agyTuple, nil).LastReply(context.Background(), agyAgent(agyTuple.Digest()))
	if err != nil || r.Text != "the **full** answer" || r.Source != "agy transcript" {
		t.Fatalf("LastReply = %+v, %v", r, err)
	}
	if want, _ := time.Parse(time.RFC3339, "2026-10-06T13:14:14-05:00"); !r.Written.Equal(want) {
		t.Fatalf("Written = %v, want %v", r.Written, want)
	}
}

func TestAgyLastReplyFullVariantFallback(t *testing.T) {
	f := newAgyFixture(t, []string{agyAnswer("2026-10-06T13:14:14-05:00", "answer")})
	f.fullOnly = true
	// The fixture wrote only transcript_full.jsonl; the reader must fall
	// back to it when transcript.jsonl does not exist.
	if r, err := f.reader(agyTuple, nil).LastReply(context.Background(), agyAgent(agyTuple.Digest())); err != nil || r.Text != "answer" {
		t.Fatalf("LastReply = %+v, %v", r, err)
	}
}

func TestAgyLastReplyPendingOrMissing(t *testing.T) {
	other := agyTuple
	other.Value = "11111111-2222-3333-4444-555555555555"
	cases := map[string]struct {
		lines []string
		tuple domain.SessionTuple
		want  error
	}{
		"turn still running after the answer": {lines: []string{agyAnswer("2026-10-06T13:14:14-05:00", "answer"), agyOutput()}, tuple: agyTuple, want: domain.ErrReplyPending},
		"planner tool step after the answer":  {lines: []string{agyAnswer("2026-10-06T13:14:14-05:00", "answer"), agyTool()}, tuple: agyTuple, want: domain.ErrReplyPending},
		"no answer yet":                       {lines: []string{agyOutput()}, tuple: agyTuple, want: domain.ErrNoReply},
		"pane runs another session":           {lines: []string{agyAnswer("2026-10-06T13:14:14-05:00", "answer")}, tuple: other, want: domain.ErrNoReply},
		"no transcript yet":                   {lines: nil, tuple: agyTuple, want: domain.ErrNoReply},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAgyFixture(t, tc.lines)
			_, err := f.reader(tc.tuple, nil).LastReply(context.Background(), agyAgent(agyTuple.Digest()))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAgyLastReplyRefusesUnsafeSession(t *testing.T) {
	bad := agyTuple
	bad.Value = "../escape"
	f := &agyFixture{home: t.TempDir()}
	_, err := f.reader(bad, nil).LastReply(context.Background(), agyAgent(bad.Digest()))
	if !errors.Is(err, domain.ErrNoReply) {
		t.Fatalf("err = %v, want ErrNoReply", err)
	}
}
