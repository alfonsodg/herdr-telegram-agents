package domain_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// stubReplySource returns a scripted reply or error, and records whether
// it was called.
type stubReplySource struct {
	reply  domain.Reply
	err    error
	called bool
	onCall func()
}

func (s *stubReplySource) LastReply(context.Context, domain.Agent) (domain.Reply, error) {
	s.called = true
	if s.onCall != nil {
		s.onCall()
	}
	return s.reply, s.err
}

func TestMultiReplySourceFirstSucceeds(t *testing.T) {
	first := &stubReplySource{reply: domain.Reply{Text: "from first"}}
	second := &stubReplySource{reply: domain.Reply{Text: "from second"}}
	m := domain.MultiReplySource{first, second}

	r, err := m.LastReply(context.Background(), domain.Agent{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if r.Text != "from first" {
		t.Fatalf("text = %q, want %q", r.Text, "from first")
	}
	if second.called {
		t.Fatal("second source was called though the first succeeded")
	}
}

func TestMultiReplySourceFallsThrough(t *testing.T) {
	first := &stubReplySource{err: domain.ErrNoReply}
	second := &stubReplySource{reply: domain.Reply{Text: "from second"}}
	m := domain.MultiReplySource{first, second}

	r, err := m.LastReply(context.Background(), domain.Agent{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if r.Text != "from second" {
		t.Fatalf("text = %q, want %q", r.Text, "from second")
	}
	if !first.called || !second.called {
		t.Fatal("both sources should have been called")
	}
}

func TestMultiReplySourceAllFail(t *testing.T) {
	first := &stubReplySource{err: domain.ErrNoReply}
	second := &stubReplySource{err: domain.ErrNoReply}
	m := domain.MultiReplySource{first, second}

	_, err := m.LastReply(context.Background(), domain.Agent{})
	if !errors.Is(err, domain.ErrNoReply) {
		t.Fatalf("err = %v, want ErrNoReply", err)
	}
}

func TestMultiReplySourceStopsOnFailure(t *testing.T) {
	want := errors.New("source failed")
	first := &stubReplySource{err: want}
	second := &stubReplySource{reply: domain.Reply{Text: "wrong"}}
	_, err := (domain.MultiReplySource{first, second}).LastReply(context.Background(), domain.Agent{})
	if !errors.Is(err, want) || second.called {
		t.Fatalf("err = %v, second called = %v", err, second.called)
	}
}

// TestMultiReplySourceStopsOnPending: a pending reply means "the turn is
// not over yet", not "this source cannot answer", so later sources must
// not overwrite it with their own ErrNoReply.
func TestMultiReplySourceStopsOnPending(t *testing.T) {
	first := &stubReplySource{err: fmt.Errorf("%w: the turn is still running", domain.ErrReplyPending)}
	second := &stubReplySource{err: domain.ErrNoReply}
	_, err := (domain.MultiReplySource{first, second}).LastReply(context.Background(), domain.Agent{})
	if !errors.Is(err, domain.ErrReplyPending) || second.called {
		t.Fatalf("err = %v, second called = %v", err, second.called)
	}
}

// TestMultiReplySourcePrefersTheRealFailure: when one source understands
// the kind and fails while the rest reject the kind, the caller sees the
// real failure, not the last "unsupported agent" wrap.
func TestMultiReplySourcePrefersTheRealFailure(t *testing.T) {
	real := fmt.Errorf("%w: export failed", domain.ErrNoReply)
	first := &stubReplySource{err: real}
	second := &stubReplySource{err: fmt.Errorf("%w: unsupported agent %q", domain.ErrUnsupportedAgent, "opencode")}
	_, err := (domain.MultiReplySource{first, second}).LastReply(context.Background(), domain.Agent{})
	if !errors.Is(err, real) || errors.Is(err, domain.ErrUnsupportedAgent) {
		t.Fatalf("err = %v, want the real failure", err)
	}
}

func TestMultiReplySourceStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first := &stubReplySource{}
	_, err := (domain.MultiReplySource{first}).LastReply(ctx, domain.Agent{})
	if !errors.Is(err, context.Canceled) || first.called {
		t.Fatalf("pre-cancel err = %v, called = %v", err, first.called)
	}
	ctx, cancel = context.WithCancel(context.Background())
	first = &stubReplySource{err: domain.ErrNoReply, onCall: cancel}
	second := &stubReplySource{}
	_, err = (domain.MultiReplySource{first, second}).LastReply(ctx, domain.Agent{})
	if !errors.Is(err, context.Canceled) || second.called {
		t.Fatalf("between-source err = %v, second called = %v", err, second.called)
	}
}

func TestMultiReplySourceEmpty(t *testing.T) {
	m := domain.MultiReplySource{}
	_, err := m.LastReply(context.Background(), domain.Agent{})
	if !errors.Is(err, domain.ErrNoReply) {
		t.Fatalf("err = %v, want ErrNoReply", err)
	}
}
