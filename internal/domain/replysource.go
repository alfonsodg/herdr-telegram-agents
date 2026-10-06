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
		if !errors.Is(err, ErrNoReply) {
			return Reply{}, err
		}
		lastErr = err
	}
	return Reply{}, lastErr
}
