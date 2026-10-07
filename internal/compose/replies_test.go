package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// TestReplySourcesKeepsOpenCodePending runs the daemon's real reader chain
// for an OpenCode pane: a turn whose newest record is tool work must reach
// the caller as ErrReplyPending, not as the Codex reader's "unsupported
// agent" that comes after it (the regression in the closed PR #30); a
// settled turn returns its reply.
func TestReplySourcesKeepsOpenCodePending(t *testing.T) {
	tuple := domain.SessionTuple{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_abc"}
	session := func(context.Context, string) (domain.SessionTuple, error) { return tuple, nil }
	agent := domain.Agent{Key: domain.Key{PaneID: "p1", TerminalID: "t1", SessionDigest: tuple.Digest()}, Kind: "opencode", Cwd: t.TempDir()}
	for name, tc := range map[string]struct {
		export    string
		wantText  string
		wantError error
	}{
		"tool last": {export: `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},
			{"info":{"role":"assistant","time":{"created":2}},"parts":[{"type":"text","text":"checking"},{"type":"tool","tool":"bash"}]}]}`,
			wantError: domain.ErrReplyPending},
		"settled": {export: `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},
			{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"tool","tool":"bash"},{"type":"text","text":"All done."}]}]}`,
			wantText: "All done."},
	} {
		t.Run(name, func(t *testing.T) {
			export := func(context.Context, string) ([]byte, error) { return []byte(tc.export), nil }
			r, err := replySources(session, export, nil).LastReply(context.Background(), agent)
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("err = %v, want %v", err, tc.wantError)
				}
				return
			}
			if err != nil || r.Text != tc.wantText {
				t.Fatalf("LastReply = %q, %v; want %q", r.Text, err, tc.wantText)
			}
		})
	}
}

// TestReplySourcesReadsAgy runs the daemon's real reader chain for an
// Antigravity pane: the agy reader is in the chain, a finished turn returns
// its answer and a running one reaches the caller as ErrReplyPending.
func TestReplySourcesReadsAgy(t *testing.T) {
	const id = "88276862-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	tuple := domain.SessionTuple{Source: "herdr:antigravity_cli", Agent: "agy", Kind: "id", Value: id}
	session := func(context.Context, string) (domain.SessionTuple, error) { return tuple, nil }
	agent := domain.Agent{Key: domain.Key{PaneID: "p1", TerminalID: "t1", SessionDigest: tuple.Digest()}, Kind: "agy", Cwd: t.TempDir()}
	noExport := func(context.Context, string) ([]byte, error) { return nil, errors.New("not opencode") }
	prompt := `{"type":"USER_INPUT","source":"USER","content":"go","created_at":"2026-10-06T13:00:00-05:00"}`
	for name, tc := range map[string]struct {
		lines     []string
		wantText  string
		wantError error
	}{
		"tool last": {lines: []string{prompt,
			`{"type":"GENERIC","source":"MODEL","content":"The command exited with code 0.","created_at":"2026-10-06T13:00:01-05:00"}`},
			wantError: domain.ErrReplyPending},
		"settled": {lines: []string{prompt,
			`{"type":"PLANNER_RESPONSE","source":"MODEL","content":"All done.","created_at":"2026-10-06T13:00:02-05:00"}`},
			wantText: "All done."},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", id, ".system_generated", "logs")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := replySources(session, noExport, nil).LastReply(context.Background(), agent)
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("err = %v, want %v", err, tc.wantError)
				}
				return
			}
			if err != nil || r.Text != tc.wantText {
				t.Fatalf("LastReply = %q, %v; want %q", r.Text, err, tc.wantText)
			}
		})
	}
}
