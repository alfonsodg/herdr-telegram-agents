package compose

import (
	"context"
	"errors"
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
