package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// agyTestID is made up; agyTestSecret marks text that must never reach a
// log line.
const (
	agyTestID     = "88276862-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	agyTestSecret = "PRIVATE-AGY-OUTPUT"
)

var agyTestTuple = domain.SessionTuple{Source: "herdr:antigravity_cli", Agent: "agy", Kind: "id", Value: agyTestID}

var (
	agyT0 = time.Date(2026, 10, 6, 13, 0, 0, 0, time.FixedZone("", -5*3600)) // prompt
	agyT1 = agyT0.Add(90 * time.Second)                                      // answer
)

// agyLine renders one transcript record.
func agyLine(rec map[string]any) string {
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func agyPrompt(at time.Time, text string) string {
	return agyLine(map[string]any{"type": "USER_INPUT", "source": "USER", "content": text, "created_at": at.Format(time.RFC3339)})
}

func agyAnswer(at time.Time, text string) string {
	return agyLine(map[string]any{"type": "PLANNER_RESPONSE", "source": "MODEL", "content": text, "created_at": at.Format(time.RFC3339), "status": "DONE"})
}

// agyToolStep is a planner step that calls a tool, with commentary.
func agyToolStep(at time.Time, text string) string {
	return agyLine(map[string]any{"type": "PLANNER_RESPONSE", "source": "MODEL", "content": text,
		"tool_calls": []any{map[string]any{"name": "bash"}}, "created_at": at.Format(time.RFC3339)})
}

func agyToolOutput(at time.Time, size int) string {
	return agyLine(map[string]any{"type": "GENERIC", "source": "MODEL",
		"content": "The command exited with code 0. Output: " + agyTestSecret + strings.Repeat("x", size), "created_at": at.Format(time.RFC3339)})
}

// agyTurn is a finished turn: prompt, commentary with a tool, its output,
// the answer.
func agyTurn(prompt, answer string) []string {
	return []string{
		agyPrompt(agyT0, prompt),
		agyToolStep(agyT0.Add(time.Second), "let me look"),
		agyToolOutput(agyT0.Add(2*time.Second), 64),
		agyAnswer(agyT1, answer),
	}
}

type agyFixture struct {
	t      *testing.T
	home   string
	tuple  domain.SessionTuple
	digest string
	logBuf bytes.Buffer
}

func newAgyFixture(t *testing.T) *agyFixture {
	t.Helper()
	return &agyFixture{t: t, home: t.TempDir(), tuple: agyTestTuple, digest: agyTestTuple.Digest()}
}

func (f *agyFixture) logsDir() string {
	return filepath.Join(f.home, agyHomeDir, agyTestID, agyLogsDir)
}

// write puts lines in the named transcript variant.
func (f *agyFixture) write(name string, lines ...string) string {
	f.t.Helper()
	return f.writeRaw(name, strings.Join(lines, "\n")+"\n")
}

func (f *agyFixture) writeRaw(name, body string) string {
	f.t.Helper()
	if err := os.MkdirAll(f.logsDir(), 0o755); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(f.logsDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *agyFixture) reader() *AgyReader {
	session := func(context.Context, string) (domain.SessionTuple, error) { return f.tuple, nil }
	home := func() (string, error) { return f.home, nil }
	now := func() time.Time { return agyT1.Add(5 * time.Second) }
	return newAgyReader(session, home, now,
		slog.New(slog.NewJSONHandler(&f.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func (f *agyFixture) agent() domain.Agent {
	return domain.Agent{Key: domain.Key{PaneID: "w1:p1", TerminalID: "term-1", SessionDigest: f.digest}, Kind: "agy"}
}

func (f *agyFixture) read() (domain.Reply, error) {
	return f.reader().LastReply(context.Background(), f.agent())
}

// assertLogClean fails when a log line carries the conversation id, a path
// or transcript text.
func (f *agyFixture) assertLogClean() {
	f.t.Helper()
	logs := f.logBuf.String()
	for _, secret := range []string{agyTestID, agyTestSecret, f.home, "the answer"} {
		if strings.Contains(logs, secret) {
			f.t.Fatalf("log leaks %q: %s", secret, logs)
		}
	}
}

func TestAgyLastReplyReturnsAnswer(t *testing.T) {
	f := newAgyFixture(t)
	f.write("transcript.jsonl", append(agyTurn("old", "an older answer"), agyTurn("hi", "the answer\n\n- one\n- two")...)...)

	r, err := f.read()
	if err != nil {
		t.Fatalf("LastReply: %v", err)
	}
	if r.Text != "the answer\n\n- one\n- two" || r.Source != "agy transcript" {
		t.Fatalf("reply = %q from %q", r.Text, r.Source)
	}
	if !r.Meta.Started.Equal(agyT0) || !r.Meta.Ended.Equal(agyT1) || !r.Written.Equal(agyT1) {
		t.Fatalf("meta = %+v, written %v", r.Meta, r.Written)
	}
	if r.Age != 5*time.Second {
		t.Fatalf("age = %v, want 5s", r.Age)
	}
	if r.Meta.Model != "" || len(r.Meta.Files) != 0 {
		t.Fatalf("meta invents model or files: %+v", r.Meta)
	}
	f.assertLogClean()
}

func TestAgyLastReplyNeverPostsCommentary(t *testing.T) {
	f := newAgyFixture(t)
	f.write("transcript.jsonl",
		agyPrompt(agyT0, "hi"),
		agyToolStep(agyT0.Add(time.Second), "commentary one"),
		agyToolOutput(agyT0.Add(2*time.Second), 10),
		agyToolStep(agyT0.Add(3*time.Second), "commentary two"),
		agyToolOutput(agyT0.Add(4*time.Second), 10),
		agyAnswer(agyT1, "final"),
	)
	r, err := f.read()
	if err != nil || r.Text != "final" {
		t.Fatalf("reply = %q, %v; want only the final answer", r.Text, err)
	}
}

func TestAgyLastReplyPending(t *testing.T) {
	tests := map[string][]string{
		"tool output is newest": append(agyTurn("old", "old answer"), agyPrompt(agyT1, "next"), agyToolOutput(agyT1, 10)),
		"tool call is newest":   append(agyTurn("old", "old answer"), agyPrompt(agyT1, "next"), agyToolStep(agyT1, "looking")),
		"prompt is newest":      append(agyTurn("old", "old answer"), agyPrompt(agyT1, "next")),
	}
	for name, lines := range tests {
		t.Run(name, func(t *testing.T) {
			f := newAgyFixture(t)
			f.write("transcript.jsonl", lines...)
			// The full variant holds a finished turn: a pending first
			// variant must not fall through to it.
			f.write("transcript_full.jsonl", agyTurn("old", "full answer")...)
			_, err := f.read()
			if !errors.Is(err, domain.ErrReplyPending) || !errors.Is(err, domain.ErrNoReply) {
				t.Fatalf("err = %v, want ErrReplyPending", err)
			}
			f.assertLogClean()
		})
	}
}

func TestAgyLastReplyMidRecordIsPending(t *testing.T) {
	f := newAgyFixture(t)
	f.writeRaw("transcript.jsonl", strings.Join(agyTurn("hi", "the answer"), "\n")+"\n"+`{"type":"PLANNER_RESPONSE","source":"MO`)
	if _, err := f.read(); !errors.Is(err, domain.ErrReplyPending) {
		t.Fatalf("err = %v, want ErrReplyPending", err)
	}
}

func TestAgyLastReplyFullVariant(t *testing.T) {
	t.Run("only full exists", func(t *testing.T) {
		f := newAgyFixture(t)
		f.write("transcript_full.jsonl", agyTurn("hi", "from full")...)
		r, err := f.read()
		if err != nil || r.Text != "from full" {
			t.Fatalf("reply = %q, %v", r.Text, err)
		}
		if !strings.Contains(f.logBuf.String(), `"file":"transcript_full"`) {
			t.Fatalf("log does not name the variant: %s", f.logBuf.String())
		}
	})
	t.Run("first has no known record", func(t *testing.T) {
		f := newAgyFixture(t)
		f.write("transcript.jsonl", `{"type":"SOMETHING_ELSE","content":"x"}`)
		f.write("transcript_full.jsonl", agyTurn("hi", "from full")...)
		r, err := f.read()
		if err != nil || r.Text != "from full" {
			t.Fatalf("reply = %q, %v", r.Text, err)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		f := newAgyFixture(t)
		f.writeRaw("transcript.jsonl", "")
		f.writeRaw("transcript_full.jsonl", "")
		if _, err := f.read(); !errors.Is(err, domain.ErrNoReply) || errors.Is(err, domain.ErrReplyPending) {
			t.Fatalf("err = %v, want plain ErrNoReply", err)
		}
	})
}

func TestAgyLastReplyNoReply(t *testing.T) {
	other := agyTestTuple
	other.Value = "99999999-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	unsafe := func(v string) domain.SessionTuple { tu := agyTestTuple; tu.Value = v; return tu }
	tests := map[string]struct {
		mut  func(f *agyFixture)
		kind string
	}{
		"another kind":       {kind: "codex"},
		"no tuple":           {mut: func(f *agyFixture) { f.tuple = domain.SessionTuple{} }},
		"tuple of codex":     {mut: func(f *agyFixture) { f.tuple.Agent = "codex"; f.digest = f.tuple.Digest() }},
		"path kind":          {mut: func(f *agyFixture) { f.tuple.Kind = "path"; f.digest = f.tuple.Digest() }},
		"another session":    {mut: func(f *agyFixture) { f.tuple = other }},
		"no topic digest":    {mut: func(f *agyFixture) { f.digest = "" }},
		"parent in id":       {mut: func(f *agyFixture) { f.tuple = unsafe("../" + agyTestID); f.digest = f.tuple.Digest() }},
		"separator in id":    {mut: func(f *agyFixture) { f.tuple = unsafe(agyTestID + "/x"); f.digest = f.tuple.Digest() }},
		"not a uuid":         {mut: func(f *agyFixture) { f.tuple = unsafe("conversation"); f.digest = f.tuple.Digest() }},
		"no brain directory": {mut: func(f *agyFixture) { _ = os.RemoveAll(filepath.Join(f.home, ".gemini")) }},
		"no transcript":      {mut: func(f *agyFixture) { _ = os.RemoveAll(f.logsDir()) }},
		"unreadable newest": {mut: func(f *agyFixture) {
			f.write("transcript.jsonl", append(agyTurn("hi", "the answer"), `{"type":"PLANNER_RESPONSE",`)...)
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newAgyFixture(t)
			f.write("transcript.jsonl", agyTurn("hi", "the answer")...)
			if tt.mut != nil {
				tt.mut(f)
			}
			agent := f.agent()
			if tt.kind != "" {
				agent.Kind = tt.kind
			}
			r, err := f.reader().LastReply(context.Background(), agent)
			if !errors.Is(err, domain.ErrNoReply) || errors.Is(err, domain.ErrReplyPending) || r.Text != "" {
				t.Fatalf("reply = %q, err = %v; want plain ErrNoReply", r.Text, err)
			}
			if strings.Contains(err.Error(), agyTestID) || strings.Contains(err.Error(), f.home) {
				t.Fatalf("error leaks the session or path: %v", err)
			}
			f.assertLogClean()
		})
	}
}

func TestAgyLastReplyRefusesLinkedTranscript(t *testing.T) {
	f := newAgyFixture(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(outside, []byte(strings.Join(agyTurn("hi", "linked"), "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.logsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.logsDir(), "transcript.jsonl")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if r, err := f.read(); !errors.Is(err, domain.ErrNoReply) || r.Text != "" {
		t.Fatalf("reply = %q, %v; a linked transcript must be refused", r.Text, err)
	}
}

func TestAgyLastReplyBudget(t *testing.T) {
	t.Run("answer beyond the budget", func(t *testing.T) {
		f := newAgyFixture(t)
		f.write("transcript.jsonl", agyAnswer(agyT1, "too far"), agyLine(map[string]any{"type": "OTHER", "content": strings.Repeat("y", 3*blockSize)}))
		r := f.reader()
		r.maxScan = blockSize
		if _, err := r.LastReply(context.Background(), f.agent()); !errors.Is(err, domain.ErrNoReply) || errors.Is(err, domain.ErrReplyPending) {
			t.Fatalf("err = %v, want a budget miss", err)
		}
	})
	t.Run("prompt beyond the budget keeps the answer", func(t *testing.T) {
		f := newAgyFixture(t)
		f.write("transcript.jsonl", agyPrompt(agyT0, "hi"), agyToolOutput(agyT0, 3*blockSize), agyAnswer(agyT1, "near"))
		r := f.reader()
		r.maxScan = blockSize
		got, err := r.LastReply(context.Background(), f.agent())
		if err != nil || got.Text != "near" || !got.Meta.Started.IsZero() {
			t.Fatalf("reply = %+v, %v; want the answer without a start", got, err)
		}
	})
}

func TestAgyLastReplyCancelled(t *testing.T) {
	f := newAgyFixture(t)
	f.write("transcript.jsonl", agyTurn("hi", "the answer")...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.reader().LastReply(ctx, f.agent()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	session := func(ctx context.Context, _ string) (domain.SessionTuple, error) {
		return domain.SessionTuple{}, context.DeadlineExceeded
	}
	r := newAgyReader(session, func() (string, error) { return f.home, nil }, time.Now, nil)
	if _, err := r.LastReply(context.Background(), f.agent()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the lookup's deadline", err)
	}
}
