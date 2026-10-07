package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// dirID makes a UUIDv7-shaped thread id whose encoded time is ms.
func dirID(ms int64) string {
	return fmt.Sprintf("%08x-%04x-7aaa-8aaa-aaaaaaaaaaaa", ms>>16, ms&0xffff)
}

// writeCodexDirRollout writes a miniature rollout for id under its day directory
// and returns the file path.
func writeCodexDirRollout(t *testing.T, home, id, cwd string, ts0, ts1 int64, answer string) string {
	t.Helper()
	ms, ok := codexV7Millis(id)
	if !ok {
		t.Fatalf("bad thread id %s", id)
	}
	dirs, _ := codexDayDirs(id, time.UnixMilli(ms))
	if len(dirs) == 0 {
		t.Fatalf("no day directory for %s", id)
	}
	dir := filepath.Join(home, ".codex", "sessions", dirs[0])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := func(ts int64) string { return time.Unix(ts, 0).UTC().Format(time.RFC3339Nano) }
	rec := func(ts int64, typ string, payload map[string]any) string {
		b, _ := json.Marshal(map[string]any{"timestamp": at(ts), "type": typ, "payload": payload})
		return string(b)
	}
	records := []string{
		rec(ts0, "session_meta", map[string]any{"id": id, "cwd": cwd}),
		rec(ts0, "event_msg", map[string]any{"type": "task_started", "turn_id": "t", "started_at": ts0}),
		rec(ts1, "event_msg", map[string]any{"type": "task_complete", "turn_id": "t", "last_agent_message": answer, "started_at": ts0, "completed_at": ts1, "duration_ms": 1000}),
	}
	path := filepath.Join(dir, fmt.Sprintf("rollout-%s-%s.jsonl", time.Unix(ts0, 0).UTC().Format("2006-01-02T15-04-05"), id))
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// dirReader builds the wrapper over a temp home and a scripted session.
func dirReader(t *testing.T, session func(context.Context, string) (domain.SessionTuple, error)) (*CodexDirectoryReader, string) {
	t.Helper()
	home := t.TempDir()
	exact := newCodexReader(session, func() (string, error) { return home, nil }, func() time.Time { return time.Unix(codexT1, 0) }, nil)
	r := NewCodexDirectoryReader(exact, nil)
	r.home = func() (string, error) { return home, nil }
	return r, home
}

func dirAgent(cwd string) domain.Agent {
	return domain.Agent{Key: domain.Key{PaneID: "p1", SessionDigest: codexTestTuple.Digest()}, Kind: "codex", Cwd: cwd}
}

func TestCodexDirectoryFallbackPrefersFresher(t *testing.T) {
	tuple := codexTestTuple
	session := func(context.Context, string) (domain.SessionTuple, error) { return tuple, nil }
	r, home := dirReader(t, session)
	cwd := "/u/proj"
	old := writeCodexDirRollout(t, home, tuple.Value, cwd, codexT0, codexT1, "old answer")
	// The pane's stale session still has an older answer; the conversation
	// actually in use writes a newer rollout for the same directory.
	freshID := dirID(time.Unix(codexT1+100, 0).UnixMilli())
	writeCodexDirRollout(t, home, freshID, cwd, codexT1+100, codexT1+110, "fresh answer")
	past := time.Unix(codexT0-1000, 0)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	got, err := r.LastReply(context.Background(), dirAgent(cwd))
	if err != nil || got.Text != "fresh answer" || got.Source != "codex rollout (directory)" {
		t.Fatalf("LastReply = %+v, %v", got, err)
	}
}

func TestCodexDirectoryFallbackKeepsFreshExact(t *testing.T) {
	tuple := codexTestTuple
	session := func(context.Context, string) (domain.SessionTuple, error) { return tuple, nil }
	r, home := dirReader(t, session)
	cwd := "/u/proj"
	writeCodexDirRollout(t, home, tuple.Value, cwd, codexT1-10, codexT1, "the exact answer")
	freshID := dirID(time.Unix(codexT1-50, 0).UnixMilli())
	writeCodexDirRollout(t, home, freshID, cwd, codexT0, codexT1-100, "an older answer")
	got, err := r.LastReply(context.Background(), dirAgent(cwd))
	if err != nil || got.Text != "the exact answer" || got.Source != "codex rollout" {
		t.Fatalf("LastReply = %+v, %v", got, err)
	}
}

func TestCodexDirectoryFallbackWithoutSession(t *testing.T) {
	session := func(context.Context, string) (domain.SessionTuple, error) {
		return domain.SessionTuple{}, fmt.Errorf("herdr down")
	}
	r, home := dirReader(t, session)
	cwd := "/u/proj"
	id := dirID(time.Unix(codexT1-50, 0).UnixMilli())
	writeCodexDirRollout(t, home, id, cwd, codexT1-40, codexT1-30, "the directory answer")
	got, err := r.LastReply(context.Background(), dirAgent(cwd))
	if err != nil || got.Text != "the directory answer" || got.Source != "codex rollout (directory)" {
		t.Fatalf("LastReply = %+v, %v", got, err)
	}
}

func TestCodexDirectoryFallbackIgnoresOtherDirectories(t *testing.T) {
	session := func(context.Context, string) (domain.SessionTuple, error) {
		return domain.SessionTuple{}, fmt.Errorf("herdr down")
	}
	r, home := dirReader(t, session)
	id := dirID(time.Unix(codexT1-50, 0).UnixMilli())
	writeCodexDirRollout(t, home, id, "/u/other", codexT1-40, codexT1-30, "another pane's answer")
	if _, err := r.LastReply(context.Background(), dirAgent("/u/proj")); err == nil {
		t.Fatal("expected no reply for a directory with no rollout")
	}
}
