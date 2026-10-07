package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// errTurnRunning is what the OpenCode reader answers while the newest
// record of the turn is tool work.
var errTurnRunning = fmt.Errorf("%w: the turn is still running", domain.ErrReplyPending)

// pendingReplyFixture starts an OpenCode turn from an operator prompt with
// reactions on and the done mode set, and scripts the reply source to say
// the turn is still running.
func pendingReplyFixture(t *testing.T, mode domain.DoneMode) (*bridgeFixture, domain.Agent) {
	t.Helper()
	f := newBridgeFixture(t)
	f.reactionsOn(t)
	if err := f.opts.Set(f.ctx, domain.OptionPostsDone, string(mode), 1); err != nil {
		t.Fatal(err)
	}
	a := f.add(t, "p1", "t1", "coder", domain.StatusIdle)
	a.Kind = "opencode"
	f.agents[a.Key] = a
	f.herdr.SetScreen("p1", "opencode screen tail")
	if err := f.out.PromptSent(f.ctx, a.Key, 101, 2); err != nil {
		t.Fatal(err)
	}
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusWorking)})
	f.replies.Fail(a.Key, errTurnRunning)
	return f, f.agents[a.Key]
}

// TestOutboundPendingReplyRetriesThenPostsReply: a done post whose reply is
// still pending posts nothing and reads no screen; the retry posts the
// finished reply once, and the 👌 is paid once.
func TestOutboundPendingReplyRetriesThenPostsReply(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneFormatted)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
	f.fire(t, 1)
	assertCallsEqual(t, f.tg, "react:101:2:👀", "react:101:2:👌")
	if len(f.herdr.Reads()) != 0 {
		t.Fatalf("pending reply read the screen: %v", f.herdr.Reads())
	}
	if !strings.Contains(f.logBuf.String(), `"msg":"reply pending, retry scheduled","key":"p1/t1","attempt":1,"max":3`) {
		t.Fatalf("log lacks the scheduled retry: %s", f.logBuf.String())
	}
	f.replies.Set(a.Key, "The **final** answer.")
	f.fireAfter(t, replyPendingDelay, 1)
	sent := f.tg.Sent()
	if len(sent) != 1 || sent[0].Text != "The **final** answer." || !sent[0].Markdown || len(f.herdr.Reads()) != 0 {
		t.Fatalf("retry: sent %+v, reads %v; want the finished reply", sent, f.herdr.Reads())
	}
	assertCallsEqual(t, f.tg, "react:101:2:👀", "react:101:2:👌", "send:101:The **final** answer.:markdown")
	if _, left := f.out.pendingReplies[a.Key]; left || !strings.Contains(f.logBuf.String(), `"retries":1`) {
		t.Fatalf("retry state left = %v; log: %s", left, f.logBuf.String())
	}
}

// TestOutboundPendingReplyFallsBackToScreen: a reply still pending after
// replyPendingRetries retries posts the screen exactly once.
func TestOutboundPendingReplyFallsBackToScreen(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneReply)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
	f.fire(t, 1)
	for i := 0; i < replyPendingRetries; i++ {
		if len(f.tg.Sent()) != 0 {
			t.Fatalf("posted before the retries ran out (retry %d): %+v", i, f.tg.Sent())
		}
		f.fireAfter(t, replyPendingDelay, 1)
	}
	sent := f.tg.Sent()
	if len(sent) != 1 || sent[0].Text != "opencode screen tail" || !sent[0].Code {
		t.Fatalf("fallback: sent %+v; want the screen once", sent)
	}
	if got := len(f.replies.Calls()); got != replyPendingRetries+1 {
		t.Fatalf("reply lookups = %d, want %d", got, replyPendingRetries+1)
	}
	if f.out.deb.Pending() != 0 || len(f.out.pendingReplies) != 0 {
		t.Fatalf("retry still armed after the fallback: timers %d, state %v", f.out.deb.Pending(), f.out.pendingReplies)
	}
	if !strings.Contains(f.logBuf.String(), `"msg":"reply still pending, screen posted","key":"p1/t1","mode":"reply","attempts":3`) {
		t.Fatalf("log lacks the fallback line: %s", f.logBuf.String())
	}
}

// TestOutboundPendingReplyScreenModeUnchanged: posts.done=screen posts the
// screen at once; the pending reply only costs the summary line.
func TestOutboundPendingReplyScreenModeUnchanged(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneScreen)
	if err := f.opts.Set(f.ctx, domain.OptionPostsMeta, "true", 1); err != nil {
		t.Fatal(err)
	}
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
	f.fire(t, 1)
	sent := f.tg.Sent()
	if len(sent) != 1 || sent[0].Text != "opencode screen tail" || sent[0].Footer != "" {
		t.Fatalf("screen mode: sent %+v; want the screen without footer", sent)
	}
	if f.out.deb.Pending() != 0 || len(f.out.pendingReplies) != 0 {
		t.Fatalf("screen mode scheduled a retry: timers %d, state %v", f.out.deb.Pending(), f.out.pendingReplies)
	}
}

// TestOutboundPendingReplyDroppedWhenAgentMovesOn: a new turn (working) or
// a question (blocked) during the wait drops the retry; nothing is posted
// for the old turn.
func TestOutboundPendingReplyDroppedWhenAgentMovesOn(t *testing.T) {
	for _, next := range []domain.Status{domain.StatusWorking, domain.StatusBlocked} {
		t.Run(string(next), func(t *testing.T) {
			f, a := pendingReplyFixture(t, domain.DoneFormatted)
			f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
			f.fire(t, 1)
			f.replies.Set(a.Key, "late answer")
			f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, next)})
			if _, left := f.out.pendingReplies[a.Key]; left {
				t.Fatal("retry state kept after the agent moved on")
			}
			if !strings.Contains(f.logBuf.String(), `"msg":"reply retry dropped","key":"p1/t1","status":"`+string(next)+`"`) {
				t.Fatalf("log lacks the dropped retry: %s", f.logBuf.String())
			}
			for _, s := range f.tg.Sent() {
				if s.Text == "late answer" {
					t.Fatalf("the old turn's answer was posted: %+v", f.tg.Sent())
				}
			}
		})
	}
}

// TestOutboundPendingReplyAfterIdleSettle: a turn that settles into idle
// (EndTurn) with a pending reply retries; a repeated idle event keeps the
// retry timer, and the retry posts the reply.
func TestOutboundPendingReplyAfterIdleSettle(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneFormatted)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusIdle)})
	f.endTurns(t, 1)
	assertCallsEqual(t, f.tg, "react:101:2:👀", "react:101:2:👌")
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusIdle)})
	if f.out.deb.Pending() != 1 {
		t.Fatalf("a repeated idle event cancelled the retry: timers %d", f.out.deb.Pending())
	}
	f.replies.Set(a.Key, "settled answer")
	f.fireAfter(t, replyPendingDelay, 1)
	assertCallsEqual(t, f.tg, "react:101:2:👀", "react:101:2:👌", "send:101:settled answer:markdown")
}

// TestOutboundPendingReplyForget: an exited agent loses its waiting done
// post.
func TestOutboundPendingReplyForget(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneFormatted)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
	f.fire(t, 1)
	if err := f.out.Forget(f.ctx, a.Key); err != nil {
		t.Fatal(err)
	}
	if len(f.out.pendingReplies) != 0 || f.out.deb.Pending() != 0 {
		t.Fatalf("Forget kept the retry: state %v, timers %d", f.out.pendingReplies, f.out.deb.Pending())
	}
}

// TestOutboundPendingReplyDoneThenIdle: a done turn whose reply is pending
// still posts it when the operator looks at the pane during the wait (Herdr
// turns done into idle once the pane is seen): the retry belongs to a turn
// that already ended.
func TestOutboundPendingReplyDoneThenIdle(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneFormatted)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
	f.fire(t, 1)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusIdle)})
	f.replies.Set(a.Key, "seen answer")
	f.fireAfter(t, replyPendingDelay, 1)
	assertCallsEqual(t, f.tg, "react:101:2:👀", "react:101:2:👌", "send:101:seen answer:markdown")
	if len(f.out.pendingReplies) != 0 {
		t.Fatalf("retry state left: %v", f.out.pendingReplies)
	}
}

// TestOutboundPendingReplyDoneIdleWorking: done, then idle (the pane seen),
// then a new turn during the wait drops the old turn's retry; nothing of the
// old turn is posted.
func TestOutboundPendingReplyDoneIdleWorking(t *testing.T) {
	f, a := pendingReplyFixture(t, domain.DoneFormatted)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusDone)})
	f.fire(t, 1)
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusIdle)})
	f.replies.Set(a.Key, "late answer")
	f.out.Observe(AgentEvent{Kind: AgentChanged, Agent: f.setStatus(a, domain.StatusWorking)})
	if _, left := f.out.pendingReplies[a.Key]; left {
		t.Fatal("retry state kept after a new turn started")
	}
	f.clock.Advance(replyPendingDelay)
	for _, s := range f.tg.Sent() {
		if s.Text == "late answer" {
			t.Fatalf("the old turn's answer was posted into the new turn: %+v", f.tg.Sent())
		}
	}
}
