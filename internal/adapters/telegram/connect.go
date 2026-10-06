package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const (
	// pollTimeout is the long-poll setting handed to the library, which asks
	// Telegram to hold getUpdates for pollTimeout minus one second. The hold
	// must end well inside httpTimeout: a held poll that Telegram answers a
	// little late on a slow network otherwise dies with "Client.Timeout
	// exceeded while awaiting headers", which a 59 s hold against a 60 s
	// deadline did in the field.
	pollTimeout = 50 * time.Second
	// httpTimeout bounds every request of the one HTTP client the bot
	// shares between the poller, the queue and the startup calls.
	httpTimeout = time.Minute
)

// StartRetry is how Connect keeps trying Telegram at startup. A daemon
// started before the network is up (login, wake from sleep) waits instead
// of exiting: nothing would start it again. Zero durations take the
// DefaultStartRetry values.
type StartRetry struct {
	// Initial is the first wait; each failure doubles it up to Max.
	Initial time.Duration
	Max     time.Duration
	// AttemptTimeout bounds one attempt (getMe, deleteWebhook and the icon
	// stickers together), so a stalled connection is retried rather than
	// waited on for the full HTTP deadline.
	AttemptTimeout time.Duration
	// OnWait is called after every retryable failure, before the wait.
	OnWait func(StartWait)
	// OnReady is called when an attempt succeeds after at least one failure.
	OnReady func(attempts int, waited time.Duration)
	// Sleep waits between attempts and returns early with the context's
	// error; nil waits on a timer. Tests pass a recording one.
	Sleep func(context.Context, time.Duration) error
}

// StartWait describes one failed startup attempt.
type StartWait struct {
	Attempt int
	// Wait is how long Connect sleeps before the next attempt.
	Wait time.Duration
	// Since is when the first attempt began.
	Since time.Time
	// Reason is the failure with the token redacted, fit for a notice.
	Reason string
}

// DefaultStartRetry waits 2 s, 4 s, … up to a minute between attempts of at
// most 15 s each.
func DefaultStartRetry() StartRetry {
	return StartRetry{Initial: 2 * time.Second, Max: time.Minute, AttemptTimeout: 15 * time.Second}
}

func (r StartRetry) withDefaults() StartRetry {
	def := DefaultStartRetry()
	if r.Initial <= 0 {
		r.Initial = def.Initial
	}
	if r.Max <= 0 {
		r.Max = def.Max
	}
	if r.Max < r.Initial {
		r.Max = r.Initial
	}
	if r.AttemptTimeout <= 0 {
		r.AttemptTimeout = def.AttemptTimeout
	}
	if r.Sleep == nil {
		r.Sleep = sleep
	}
	return r
}

// Connect builds the daemon's Telegram side from the saved config: bot
// client, token check (retaining pending updates for private contacts), icon pack, command menu, serial queue and gateway. The returned
// run function polls and serves the queue until its context ends. fatal is
// invoked by the poller on 401/409. The token check and the icon pack are
// retried per retry until they succeed, fail for good (401, 400, 403, 404)
// or ctx ends. Extra opts are for tests.
func Connect(ctx context.Context, cfg domain.Config, log *slog.Logger, fatal context.CancelFunc, retry StartRetry, opts ...bot.Option) (*Gateway, func(context.Context), error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	admission := &admissionClient{client: &http.Client{Timeout: httpTimeout}}
	opts = append(opts, bot.WithHTTPClient(pollTimeout, admission))
	log.Debug("telegram http configured",
		slog.Int("poll_hold_s", int((pollTimeout-time.Second).Seconds())),
		slog.Int("http_timeout_s", int(httpTimeout.Seconds())))
	api, err := NewBot(cfg.BotToken, log, fatal, opts...)
	if err != nil {
		return nil, nil, err
	}
	identity, icons, err := startupChecks(ctx, api, cfg.BotToken, retry.withDefaults(), log)
	if err != nil {
		return nil, nil, err
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

// startupChecks runs startupAttempt until it succeeds, fails with an error
// that no retry can fix, or ctx ends. A 429 waits at least as long as
// Telegram asked.
func startupChecks(ctx context.Context, api *bot.Bot, token string, retry StartRetry, log *slog.Logger) (BotIdentity, IconSet, error) {
	start := time.Now()
	wait := retry.Initial
	for attempt := 1; ; attempt++ {
		log.Debug("telegram start attempt", slog.Int("attempt", attempt))
		identity, icons, err := startupAttempt(ctx, api, retry.AttemptTimeout, log)
		if err == nil {
			if attempt > 1 {
				waited := time.Since(start)
				log.Info("telegram reachable", slog.Int("attempts", attempt), slog.Int64("waited_ms", waited.Milliseconds()))
				if retry.OnReady != nil {
					retry.OnReady(attempt, waited)
				}
			}
			return identity, icons, nil
		}
		if ctx.Err() != nil {
			log.Info("telegram start cancelled", slog.Int("attempts", attempt))
			return BotIdentity{}, IconSet{}, fmt.Errorf("telegram start: %w", ctx.Err())
		}
		if !isRetryable(err) {
			log.Debug("telegram start failed for good", slog.Int("attempt", attempt), slog.String("err", redact(err, token)))
			return BotIdentity{}, IconSet{}, err
		}
		d := wait
		if after, ok := retryAfter(err); ok && after > d {
			d = after
		}
		reason := redact(err, token)
		log.Warn("telegram unreachable, retrying", slog.Int("attempt", attempt), slog.Int64("wait_ms", d.Milliseconds()),
			slog.String("err", reason), slog.Time("since", start))
		if retry.OnWait != nil {
			retry.OnWait(StartWait{Attempt: attempt, Wait: d, Since: start, Reason: reason})
		}
		if err := retry.Sleep(ctx, d); err != nil {
			log.Info("telegram start cancelled", slog.Int("attempts", attempt))
			return BotIdentity{}, IconSet{}, fmt.Errorf("telegram start: %w", err)
		}
		wait = min(wait*2, retry.Max)
	}
}

// startupAttempt verifies the token, clears the webhook and loads the icon
// pack under one bounded context. A stalled attempt ends with a deadline
// error, which is retryable; the parent context is checked by the caller.
func startupAttempt(ctx context.Context, api *bot.Bot, timeout time.Duration, log *slog.Logger) (BotIdentity, IconSet, error) {
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	identity, err := Check(actx, api, log)
	if err != nil {
		return BotIdentity{}, IconSet{}, fmt.Errorf("telegram check: %w", err)
	}
	icons, err := LoadIcons(actx, api, log)
	if err != nil {
		return BotIdentity{}, IconSet{}, fmt.Errorf("telegram icons: %w", err)
	}
	return identity, icons, nil
}
