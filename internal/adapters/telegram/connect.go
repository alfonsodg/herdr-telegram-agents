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
	admission := &admissionClient{client: &http.Client{Timeout: time.Minute}}
	opts = append(opts, bot.WithHTTPClient(time.Minute, admission))
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
