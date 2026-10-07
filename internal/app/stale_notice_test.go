package app

import (
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// TestOutboundStaleNeverNotices: a reply older than the turn's start is an
// expected fallback (two panes in one directory), never a broken reader,
// so it must not trip the unreadable-replies notice.
func TestOutboundStaleNeverNotices(t *testing.T) {
	f := newBridgeFixture(t)
	f.herdr.SetScreen("p1", "raw screen text")
	a := f.add(t, "p1", "t1", "reviewer", domain.StatusIdle)
	f.replies.Set(a.Key, "an older answer")
	f.replies.SetMeta(a.Key, domain.TurnMeta{}, f.clock.Now().Add(-time.Minute))
	for i := 0; i < unreadableNoticeAfter+1; i++ {
		f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusWorking)})
		f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
		f.fire(t, 1)
	}
	for _, s := range f.tg.Sent() {
		if s.Text == unreadableNotice {
			t.Fatal("stale transcripts got the unreadable notice")
		}
	}
}
