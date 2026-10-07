package domain

import (
	"context"
	"errors"
	"fmt"
)

// ErrReplyPending reports a turn that has not settled yet: its newest
// record is tool work or it has no assistant text after the last prompt,
// so a read taken now would return a fragment. It wraps ErrNoReply, so
// callers that cannot wait treat it as "no reply"; the done-post path
// skips the post entirely and lets the real turn end produce the answer.
var ErrReplyPending = fmt.Errorf("%w: reply not ready yet", ErrNoReply)

// MultiReplySource tries each source in order and returns the first reply
// that isn't ErrNoReply. Every source is expected to reject agent kinds it
// doesn't understand with ErrNoReply, so this never needs to dispatch by
// kind itself; order among sources that could both answer does not matter
// in practice because no two sources currently claim the same kind.
type MultiReplySource []ReplySource

// LastReply implements ReplySource.
func (m MultiReplySource) LastReply(ctx context.Context, agent Agent) (Reply, error) {
	lastErr := error(ErrNoReply)
	var lastReal error
	for _, s := range m {
		if err := ctx.Err(); err != nil {
			return Reply{}, err
		}
		r, err := s.LastReply(ctx, agent)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Reply{}, ctxErr
		}
		if err == nil {
			return r, nil
		}
		// A pending reply must reach the caller: it is not "this source
		// cannot answer", it is "the turn is not over yet", and the
		// done-post path must skip and retry instead of falling back to
		// the screen of a fragment.
		if errors.Is(err, ErrReplyPending) {
			return Reply{}, err
		}
		if !errors.Is(err, ErrNoReply) {
			return Reply{}, err
		}
		// A source that understands the kind but could not read it knows
		// more than the later sources' "not my kind": keep it so the
		// caller can tell a broken read from a kind nobody reads.
		if !errors.Is(err, ErrUnsupportedAgent) {
			lastReal = err
		}
		lastErr = err
	}
	if lastReal != nil {
		return Reply{}, lastReal
	}
	return Reply{}, lastErr
}
