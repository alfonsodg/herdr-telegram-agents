package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// Timeouts are the two independent Telegram transport deadlines. Poll is the
// getUpdates long-poll window the SDK asks Telegram to hold open; HTTP is the
// deadline of the shared HTTP client, polling included. The SDK requests a
// wait of Poll-1s, so HTTP must exceed Poll by a safety margin; otherwise the
// client cancels the poll just as Telegram is about to answer (seen live as
// "Client.Timeout exceeded while awaiting headers").
type Timeouts struct {
	Poll time.Duration
	HTTP time.Duration
}

const (
	// DefaultPollWindow is the getUpdates long-poll window.
	DefaultPollWindow = time.Minute
	// DefaultHTTPDeadlineMargin is the headroom of the HTTP client deadline
	// over the poll window; it absorbs the SDK's Poll-1s wait and latency.
	DefaultHTTPDeadlineMargin = 15 * time.Second
	// MinHTTPDeadlineMargin is the least headroom NewTimeouts accepts.
	MinHTTPDeadlineMargin = 5 * time.Second
)

// NewTimeouts validates two deadlines and rejects a relationship that would
// expire the HTTP client at the end of an ordinary long poll.
func NewTimeouts(poll, httpDeadline time.Duration) (Timeouts, error) {
	switch {
	case poll <= 0:
		return Timeouts{}, fmt.Errorf("telegram poll window must be positive, got %v", poll)
	case httpDeadline <= 0:
		return Timeouts{}, fmt.Errorf("telegram HTTP deadline must be positive, got %v", httpDeadline)
	case httpDeadline <= poll:
		return Timeouts{}, fmt.Errorf("telegram HTTP deadline %v must exceed the %v poll window", httpDeadline, poll)
	case httpDeadline-poll < MinHTTPDeadlineMargin:
		return Timeouts{}, fmt.Errorf("telegram HTTP deadline %v leaves %v over the %v poll window, want at least %v", httpDeadline, httpDeadline-poll, poll, MinHTTPDeadlineMargin)
	}
	return Timeouts{Poll: poll, HTTP: httpDeadline}, nil
}

// DefaultTimeouts is the shipped deadline configuration.
func DefaultTimeouts() Timeouts {
	return Timeouts{Poll: DefaultPollWindow, HTTP: DefaultPollWindow + DefaultHTTPDeadlineMargin}
}

// client builds the admission-wrapping HTTP client with the HTTP deadline.
func (t Timeouts) client() *admissionClient {
	return &admissionClient{client: &http.Client{Timeout: t.HTTP}}
}

// checkRetryBackoff spaces the attempts that wait for a reachable Telegram
// at daemon start: a boot can race the network, and the daemon previously
// exited on the first dial error. Tests shorten the list.
var checkRetryBackoff = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute, 5 * time.Minute}

// checkWithRetry runs attempt until it succeeds, the error is final (bad
// token, another poller), the context ends, or the backoff is exhausted.
func checkWithRetry(ctx context.Context, log *slog.Logger, attempt func(context.Context) (BotIdentity, error)) (BotIdentity, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for i := 0; ; i++ {
		id, err := attempt(ctx)
		if err == nil {
			return id, nil
		}
		if errors.Is(err, domain.ErrBotUnauthorized) || errors.Is(err, domain.ErrPollerConflict) {
			return BotIdentity{}, err
		}
		if ctx.Err() != nil {
			return BotIdentity{}, ctx.Err()
		}
		if i >= len(checkRetryBackoff) {
			return BotIdentity{}, err
		}
		wait := checkRetryBackoff[i]
		log.Warn("telegram unreachable, retrying", slog.String("err", err.Error()), slog.Duration("wait", wait))
		select {
		case <-ctx.Done():
			return BotIdentity{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Connect builds the daemon's Telegram side from the saved config: bot
// client, token check (retaining pending updates for private contacts), icon pack, command menu, serial queue and gateway. The returned
// run function polls and serves the queue until its context ends. fatal is
// invoked by the poller on 401/409. Extra opts are for tests.
func Connect(ctx context.Context, cfg domain.Config, log *slog.Logger, fatal context.CancelFunc, opts ...bot.Option) (*Gateway, func(context.Context), error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeouts := DefaultTimeouts()
	admission := timeouts.client()
	opts = append(opts, bot.WithHTTPClient(timeouts.Poll, admission))
	api, err := NewBot(cfg.BotToken, log, fatal, opts...)
	if err != nil {
		return nil, nil, err
	}
	identity, err := checkWithRetry(ctx, log, func(c context.Context) (BotIdentity, error) {
		return Check(c, api, log)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("telegram check: %w", err)
	}
	icons, err := LoadIcons(ctx, api, log)
	if err != nil {
		return nil, nil, fmt.Errorf("telegram icons: %w", err)
	}
	// A missing menu is a cosmetic loss; RegisterCommands already logged it.
	_ = RegisterCommands(ctx, api, cfg.ChatID, log)
	queue := NewQueue(log, DefaultQueueConfig())
	gw := NewGateway(api, Config{RejectBefore: time.Now(), ChatID: cfg.ChatID, Operators: cfg.OperatorIDs, Observers: cfg.ObserverIDs, Icons: icons, BotID: identity.ID, NoticeDelay: NoticeDelay}, queue, log)
	admission.admit = gw.admitPrivate
	run := func(ctx context.Context) {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			Poll(ctx, api, log)
		}()
		go func() {
			defer wg.Done()
			gw.Run(ctx)
		}()
		wg.Wait()
	}
	log.Info("telegram connected", slog.Int64("bot_id", identity.ID), slog.String("username", identity.Username), slog.Int64("chat_id", cfg.ChatID))
	return gw, run, nil
}
