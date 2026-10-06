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

// Connect builds the daemon's Telegram side from the saved config: bot
// client, token check (retaining pending updates for private contacts), icon pack, command menu, serial queue and gateway. The returned
// run function polls and serves the queue until its context ends. fatal is
// invoked by the poller on 401/409. Extra opts are for tests.
func Connect(ctx context.Context, cfg domain.Config, log *slog.Logger, fatal context.CancelFunc, opts ...bot.Option) (*Gateway, func(context.Context), error) {
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
	identity, err := Check(ctx, api, log)
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
