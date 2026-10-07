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

// piTestSessionID is made up; piTestSecret marks text that must never reach
// a log line.
const (
	piTestSessionID = "0199aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee"
	piTestSecret    = "PRIVATE-PI-OUTPUT"
)

var (
	piT0 = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC) // prompt
	piT1 = piT0.Add(2 * time.Minute)                    // answer
)

// piLine renders one session entry.
func piLine(rec map[string]any) string {
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func piHeader() string {
	return piLine(map[string]any{"type": "session", "version": 3, "id": piTestSessionID,
		"timestamp": piT0.Add(-time.Minute).Format(time.RFC3339Nano), "cwd": "/work/project"})
}

// piParent renders a parent id; "" is a root.
func piParent(parent string) any {
	if parent == "" {
		return nil
	}
	return parent
}

// piEntryLine renders a non-message entry such as compaction or label.
func piEntryLine(typ, id, parent string, at time.Time) string {
	return piLine(map[string]any{"type": typ, "id": id, "parentId": piParent(parent),
		"timestamp": at.Format(time.RFC3339Nano), "summary": "s"})
}

func piMessageLine(id, parent string, at time.Time, msg map[string]any) string {
	return piLine(map[string]any{"type": "message", "id": id, "parentId": piParent(parent),
		"timestamp": at.Format(time.RFC3339Nano), "message": msg})
}

func piUser(id, parent string, at time.Time, text string) string {
	return piMessageLine(id, parent, at, map[string]any{"role": "user", "content": text, "timestamp": at.UnixMilli()})
}

// piAnswer is an assistant message that ends the turn.
func piAnswer(id, parent string, at time.Time, text, model string, out int) string {
	return piMessageLine(id, parent, at, map[string]any{"role": "assistant", "provider": "anthropic", "model": model,
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "hidden " + piTestSecret},
			map[string]any{"type": "text", "text": text},
		},
		"usage":      map[string]any{"input": 10, "output": out, "cacheRead": 0, "cacheWrite": 0},
		"stopReason": "stop", "timestamp": at.UnixMilli()})
}

// piToolStep is an assistant message with commentary and one tool call.
func piToolStep(id, parent string, at time.Time, text, tool, path string, out int) string {
	return piMessageLine(id, parent, at, map[string]any{"role": "assistant", "provider": "anthropic", "model": "claude-x",
		"content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "toolCall", "id": "call-" + id, "name": tool, "arguments": map[string]any{"path": path}},
		},
		"usage":      map[string]any{"output": out},
		"stopReason": "toolUse", "timestamp": at.UnixMilli()})
}

func piToolResult(id, parent string, at time.Time, size int) string {
	return piMessageLine(id, parent, at, map[string]any{"role": "toolResult", "toolCallId": "call", "toolName": "bash",
		"content": []any{map[string]any{"type": "text", "text": piTestSecret + strings.Repeat("x", size)}},
		"isError": false, "timestamp": at.UnixMilli()})
}

// piStopped is an assistant message with only text and the given stop
// reason.
func piStopped(id, parent string, at time.Time, text, reason string) string {
	return piMessageLine(id, parent, at, map[string]any{"role": "assistant", "model": "claude-x",
		"content": []any{map[string]any{"type": "text", "text": text}}, "stopReason": reason})
}

// piTurn is a finished turn hanging off parent: prompt, a tool step that
// edits a file, its result, a write, its result, the answer. It returns the
// lines and the answer's id.
func piTurn(prefix, parent, prompt, answer string) ([]string, string) {
	return []string{
		piUser(prefix+"u", parent, piT0, prompt),
		piToolStep(prefix+"a1", prefix+"u", piT0.Add(time.Second), "let me edit", "edit", "/work/project/a.go", 7),
		piToolResult(prefix+"r1", prefix+"a1", piT0.Add(2*time.Second), 32),
		piToolStep(prefix+"a2", prefix+"r1", piT0.Add(3*time.Second), "and write", "write", "/work/project/b.go", 5),
		piToolResult(prefix+"r2", prefix+"a2", piT0.Add(4*time.Second), 32),
		piToolStep(prefix+"a3", prefix+"r2", piT0.Add(5*time.Second), "edit again", "edit", "/work/project/a.go", 3),
		piToolResult(prefix+"r3", prefix+"a3", piT0.Add(6*time.Second), 32),
		piAnswer(prefix+"a4", prefix+"r3", piT1, answer, "claude-opus-x", 100),
	}, prefix + "a4"
}

type piFixture struct {
	t      *testing.T
	dir    string
	path   string
	tuple  domain.SessionTuple
	digest string
	logBuf bytes.Buffer
}

func newPiFixture(t *testing.T) *piFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions", "--work-project--")
	path := filepath.Join(dir, "2026-10-07T09-00-00-000Z_"+piTestSessionID+".jsonl")
	tuple := domain.SessionTuple{Source: "herdr:pi", Agent: "pi", Kind: "path", Value: path}
	return &piFixture{t: t, dir: dir, path: path, tuple: tuple, digest: tuple.Digest()}
}

// write puts the header and lines into the session file.
func (f *piFixture) write(lines ...string) {
	f.t.Helper()
	f.writeRaw(strings.Join(append([]string{piHeader()}, lines...), "\n") + "\n")
}

func (f *piFixture) writeRaw(body string) {
	f.t.Helper()
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *piFixture) reader() *PiReader {
	session := func(context.Context, string) (domain.SessionTuple, error) { return f.tuple, nil }
	now := func() time.Time { return piT1.Add(5 * time.Second) }
	return newPiReader(session, now, slog.New(slog.NewJSONHandler(&f.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func (f *piFixture) agent() domain.Agent {
	return domain.Agent{Key: domain.Key{PaneID: "w1:p1", TerminalID: "term-1", SessionDigest: f.digest}, Kind: "pi", Cwd: "/work/project"}
}

func (f *piFixture) read() (domain.Reply, error) {
	return f.reader().LastReply(context.Background(), f.agent())
}

// assertLogClean fails when a log line carries the session id, a path or
// session text.
func (f *piFixture) assertLogClean() {
	f.t.Helper()
	logs := f.logBuf.String()
	for _, secret := range []string{piTestSessionID, piTestSecret, f.dir, "work-project", "the answer"} {
		if strings.Contains(logs, secret) {
			f.t.Fatalf("log leaks %q: %s", secret, logs)
		}
	}
}

func TestPiLastReplyReturnsAnswer(t *testing.T) {
	f := newPiFixture(t)
	older, oldID := piTurn("o", "", "old", "an older answer")
	turn, _ := piTurn("n", oldID, "hi", "the answer\n\n- one\n- two")
	f.write(append(older, turn...)...)

	r, err := f.read()
	if err != nil {
		t.Fatalf("LastReply: %v", err)
	}
	if r.Text != "the answer\n\n- one\n- two" || r.Source != "pi session" {
		t.Fatalf("reply = %q from %q", r.Text, r.Source)
	}
	if !r.Meta.Started.Equal(piT0) || !r.Meta.Ended.Equal(piT1) || !r.Written.Equal(piT1) {
		t.Fatalf("meta = %+v, written %v", r.Meta, r.Written)
	}
	if r.Age != 5*time.Second {
		t.Fatalf("age = %v, want 5s", r.Age)
	}
	if r.Meta.Model != "claude-opus-x" || r.Meta.OutputTokens != 115 {
		t.Fatalf("model %q, tokens %d; want claude-opus-x, 115 (7+5+3+100)", r.Meta.Model, r.Meta.OutputTokens)
	}
	if got := strings.Join(r.Meta.Files, ","); got != "/work/project/a.go,/work/project/b.go" {
		t.Fatalf("files = %q, want a.go then b.go once each", got)
	}
	if !strings.Contains(f.logBuf.String(), `"msg":"pi reply found"`) {
		t.Fatalf("no found line: %s", f.logBuf.String())
	}
	f.assertLogClean()
}

func TestPiLastReplyNeverPostsCommentary(t *testing.T) {
	f := newPiFixture(t)
	turn, _ := piTurn("n", "", "hi", "final")
	f.write(turn...)
	r, err := f.read()
	if err != nil || r.Text != "final" {
		t.Fatalf("reply = %q, %v; want only the final answer", r.Text, err)
	}
	if strings.Contains(r.Text, "hidden") {
		t.Fatalf("thinking posted: %q", r.Text)
	}
}

func TestPiLastReplySkipsTrailingEntries(t *testing.T) {
	f := newPiFixture(t)
	turn, last := piTurn("n", "", "hi", "final")
	f.write(append(turn,
		piEntryLine("compaction", "c1", last, piT1.Add(time.Second)),
		piEntryLine("usage", "c2", "c1", piT1.Add(2*time.Second)),
		piEntryLine("label", "c3", "c2", piT1.Add(3*time.Second)),
		piEntryLine("session_info", "c4", "c3", piT1.Add(4*time.Second)),
		piMessageLine("c5", "c4", piT1.Add(5*time.Second), map[string]any{"role": "bashExecution", "command": "ls", "output": "x"}),
	)...)
	r, err := f.read()
	if err != nil || r.Text != "final" {
		t.Fatalf("reply = %q, %v; trailing entries must not hide the answer", r.Text, err)
	}
}

// After /tree to an earlier point, pi appends the new branch off that
// point: the newer line of the abandoned branch must not be posted.
func TestPiLastReplyFollowsActiveBranch(t *testing.T) {
	t.Run("answer on the active branch", func(t *testing.T) {
		f := newPiFixture(t)
		first, firstID := piTurn("a", "", "first", "first answer")
		abandoned, _ := piTurn("b", firstID, "second", "abandoned answer")
		active, _ := piTurn("c", firstID, "second again", "active answer")
		f.write(append(append(first, abandoned...), active...)...)
		r, err := f.read()
		if err != nil || r.Text != "active answer" {
			t.Fatalf("reply = %q, %v; want the active branch's answer", r.Text, err)
		}
		if got := strings.Join(r.Meta.Files, ","); got != "/work/project/a.go,/work/project/b.go" {
			t.Fatalf("files = %q", got)
		}
	})
	t.Run("new prompt on a branch whose sibling has an answer", func(t *testing.T) {
		f := newPiFixture(t)
		first, firstID := piTurn("a", "", "first", "first answer")
		abandoned, _ := piTurn("b", firstID, "second", "abandoned answer")
		f.write(append(append(first, abandoned...), piUser("cu", firstID, piT1, "retry"))...)
		if _, err := f.read(); !errors.Is(err, domain.ErrReplyPending) {
			t.Fatalf("err = %v, want ErrReplyPending", err)
		}
	})
	t.Run("leaf moved back to an older answer", func(t *testing.T) {
		f := newPiFixture(t)
		first, firstID := piTurn("a", "", "first", "first answer")
		abandoned, _ := piTurn("b", firstID, "second", "abandoned answer")
		f.write(append(append(first, abandoned...), piEntryLine("label", "l1", firstID, piT1))...)
		r, err := f.read()
		if err != nil || r.Text != "first answer" {
			t.Fatalf("reply = %q, %v; want the answer the leaf hangs off", r.Text, err)
		}
		if !strings.Contains(f.logBuf.String(), `"off_branch":8`) {
			t.Fatalf("abandoned entries not counted: %s", f.logBuf.String())
		}
	})
}

func TestPiLastReplyLinearV1(t *testing.T) {
	f := newPiFixture(t)
	f.write(
		piLine(map[string]any{"type": "message", "timestamp": piT0.Format(time.RFC3339Nano), "message": map[string]any{"role": "user", "content": "hi"}}),
		piLine(map[string]any{"type": "message", "timestamp": piT1.Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "v1 answer"}}, "stopReason": "stop", "model": "m"}}),
	)
	r, err := f.read()
	if err != nil || r.Text != "v1 answer" || !r.Meta.Started.Equal(piT0) {
		t.Fatalf("reply = %+v, %v; want the v1 answer with its start", r, err)
	}
}

func TestPiLastReplyPending(t *testing.T) {
	older, oldID := piTurn("o", "", "old", "old answer")
	tests := map[string][]string{
		"tool step is newest":   append(append([]string{}, older...), piUser("u", oldID, piT1, "next"), piToolStep("a", "u", piT1, "looking", "read", "/x", 1)),
		"tool result is newest": append(append([]string{}, older...), piUser("u", oldID, piT1, "next"), piToolResult("r", "u", piT1, 10)),
		"prompt is newest":      append(append([]string{}, older...), piUser("u", oldID, piT1, "next")),
		"deferred answer":       append(append([]string{}, older...), piUser("u", oldID, piT1, "next"), piStopped("a", "u", piT1, "later", "deferred")),
		"tool use stop reason":  append(append([]string{}, older...), piUser("u", oldID, piT1, "next"), piStopped("a", "u", piT1, "calling", "toolUse")),
	}
	for name, lines := range tests {
		t.Run(name, func(t *testing.T) {
			f := newPiFixture(t)
			f.write(lines...)
			_, err := f.read()
			if !errors.Is(err, domain.ErrReplyPending) || !errors.Is(err, domain.ErrNoReply) {
				t.Fatalf("err = %v, want ErrReplyPending", err)
			}
			if !strings.Contains(f.logBuf.String(), `"msg":"pi reply pending"`) {
				t.Fatalf("no pending line: %s", f.logBuf.String())
			}
			f.assertLogClean()
		})
	}
}

func TestPiLastReplyStopReasons(t *testing.T) {
	for _, reason := range []string{"error", "aborted"} {
		t.Run(reason, func(t *testing.T) {
			f := newPiFixture(t)
			f.write(piUser("u", "", piT0, "hi"), piStopped("a", "u", piT1, "partial", reason))
			r, err := f.read()
			if !errors.Is(err, domain.ErrNoReply) || errors.Is(err, domain.ErrReplyPending) || r.Text != "" {
				t.Fatalf("reply = %q, err = %v; want plain ErrNoReply", r.Text, err)
			}
		})
	}
	t.Run("length", func(t *testing.T) {
		f := newPiFixture(t)
		f.write(piUser("u", "", piT0, "hi"), piStopped("a", "u", piT1, "cut short", "length"))
		r, err := f.read()
		if err != nil || r.Text != "cut short" {
			t.Fatalf("reply = %q, %v; a length stop still answers", r.Text, err)
		}
	})
}

func TestPiLastReplyMidRecordIsPending(t *testing.T) {
	f := newPiFixture(t)
	turn, _ := piTurn("n", "", "hi", "the answer")
	f.writeRaw(strings.Join(append([]string{piHeader()}, turn...), "\n") + "\n" + `{"type":"message","id":"x","parentId":"na4","mes`)
	if _, err := f.read(); !errors.Is(err, domain.ErrReplyPending) {
		t.Fatalf("err = %v, want ErrReplyPending", err)
	}
}

func TestPiLastReplyNoReply(t *testing.T) {
	retarget := func(f *piFixture, p string) { f.tuple.Value = p; f.digest = f.tuple.Digest() }
	tests := map[string]struct {
		mut  func(f *piFixture)
		kind string
	}{
		"another kind":    {kind: "codex"},
		"no tuple":        {mut: func(f *piFixture) { f.tuple = domain.SessionTuple{} }},
		"id kind":         {mut: func(f *piFixture) { f.tuple.Kind = "id"; f.tuple.Value = piTestSessionID; f.digest = f.tuple.Digest() }},
		"tuple of omp":    {mut: func(f *piFixture) { f.tuple.Agent = "omp"; f.digest = f.tuple.Digest() }},
		"another session": {mut: func(f *piFixture) { f.digest = "other" }},
		"no topic digest": {mut: func(f *piFixture) { f.digest = "" }},
		"relative path":   {mut: func(f *piFixture) { retarget(f, filepath.Join("sessions", "x.jsonl")) }},
		"unclean path": {mut: func(f *piFixture) {
			retarget(f, f.dir+string(filepath.Separator)+".."+string(filepath.Separator)+"x.jsonl")
		}},
		"not jsonl":    {mut: func(f *piFixture) { retarget(f, strings.TrimSuffix(f.path, ".jsonl")+".txt") }},
		"nul in path":  {mut: func(f *piFixture) { retarget(f, f.path+"\x00.jsonl") }},
		"missing file": {mut: func(f *piFixture) { _ = os.Remove(f.path) }},
		"missing dir":  {mut: func(f *piFixture) { _ = os.RemoveAll(f.dir) }},
		"empty file":   {mut: func(f *piFixture) { f.writeRaw("") }},
		"not a pi file": {mut: func(f *piFixture) {
			f.writeRaw("secret notes " + piTestSecret + "\n" + piStopped("a", "", piT1, "posted?", "stop") + "\n")
		}},
		"header of another kind": {mut: func(f *piFixture) {
			turn, _ := piTurn("n", "", "hi", "the answer")
			f.writeRaw(strings.Join(append([]string{`{"type":"session_meta","id":"x"}`}, turn...), "\n") + "\n")
		}},
		"header only": {mut: func(f *piFixture) { f.write() }},
		"unreadable newest": {mut: func(f *piFixture) {
			turn, _ := piTurn("n", "", "hi", "the answer")
			f.write(append(turn, `{"type":"message",`)...)
		}},
		"answer without text": {mut: func(f *piFixture) {
			f.write(piUser("u", "", piT0, "hi"), piStopped("a", "u", piT1, "  ", "stop"))
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newPiFixture(t)
			turn, _ := piTurn("n", "", "hi", "the answer")
			f.write(turn...)
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
			if strings.Contains(err.Error(), piTestSessionID) || strings.Contains(err.Error(), f.dir) {
				t.Fatalf("error leaks the session or path: %v", err)
			}
			f.assertLogClean()
		})
	}
}

func TestPiLastReplyRefusesLinkedSession(t *testing.T) {
	f := newPiFixture(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	turn, _ := piTurn("n", "", "hi", "linked")
	if err := os.WriteFile(outside, []byte(strings.Join(append([]string{piHeader()}, turn...), "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.path); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if r, err := f.read(); !errors.Is(err, domain.ErrNoReply) || r.Text != "" {
		t.Fatalf("reply = %q, %v; a linked session must be refused", r.Text, err)
	}
}

func TestPiLastReplyBudget(t *testing.T) {
	t.Run("answer beyond the budget", func(t *testing.T) {
		f := newPiFixture(t)
		f.write(piUser("u", "", piT0, "hi"), piAnswer("a", "u", piT1, "too far", "m", 1),
			piLine(map[string]any{"type": "custom", "id": "c", "parentId": "a", "data": strings.Repeat("y", 3*blockSize)}))
		r := f.reader()
		r.maxScan = blockSize
		if _, err := r.LastReply(context.Background(), f.agent()); !errors.Is(err, domain.ErrNoReply) || errors.Is(err, domain.ErrReplyPending) {
			t.Fatalf("err = %v, want a budget miss", err)
		}
	})
	t.Run("prompt beyond the budget keeps the answer", func(t *testing.T) {
		f := newPiFixture(t)
		f.write(piUser("u", "", piT0, "hi"), piToolResult("r", "u", piT0, 3*blockSize), piAnswer("a", "r", piT1, "near", "m", 1))
		r := f.reader()
		r.maxScan = blockSize
		got, err := r.LastReply(context.Background(), f.agent())
		if err != nil || got.Text != "near" || !got.Meta.Started.IsZero() {
			t.Fatalf("reply = %+v, %v; want the answer without a start", got, err)
		}
		if !strings.Contains(f.logBuf.String(), "meta partial") {
			t.Fatalf("partial meta not logged: %s", f.logBuf.String())
		}
	})
}

func TestPiLastReplyCancelled(t *testing.T) {
	f := newPiFixture(t)
	turn, _ := piTurn("n", "", "hi", "the answer")
	f.write(turn...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.reader().LastReply(ctx, f.agent()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	session := func(ctx context.Context, _ string) (domain.SessionTuple, error) {
		return domain.SessionTuple{}, context.DeadlineExceeded
	}
	r := newPiReader(session, time.Now, nil)
	if _, err := r.LastReply(context.Background(), f.agent()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the lookup's deadline", err)
	}
}
