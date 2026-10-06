package telegram_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"

	"github.com/permgps/herdr-telegram-agents/internal/adapters/telegram"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// startTrace records what Connect's startup retry did: the waits it asked
// for (none is slept), the OnWait reports and the OnReady call.
type startTrace struct {
	mu       sync.Mutex
	sleeps   []time.Duration
	waits    []telegram.StartWait
	ready    int
	readyErr bool
}

func (s *startTrace) retry() telegram.StartRetry {
	return telegram.StartRetry{
		Initial:        2 * time.Second,
		Max:            time.Minute,
		AttemptTimeout: time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.sleeps = append(s.sleeps, d)
			return ctx.Err()
		},
		OnWait: func(w telegram.StartWait) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.waits = append(s.waits, w)
		},
		OnReady: func(attempts int, _ time.Duration) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.ready != 0 {
				s.readyErr = true
			}
			s.ready = attempts
		},
	}
}

func (s *startTrace) slept() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sleeps)
}

// startAPI is a fake Bot API on which the startup calls succeed.
func startAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := newFakeAPI(t)
	api.on("getMe", func(url.Values) apiReply { return getMeOK() })
	api.on("getForumTopicIconStickers", func(url.Values) apiReply { return iconsOK() })
	return api
}

func getMeOK() apiReply {
	return okReply(map[string]any{"id": 42, "is_bot": true, "first_name": "Agents", "username": "agents_bot"})
}

func iconsOK() apiReply {
	return okReply([]map[string]any{{"file_id": "f", "file_unique_id": "u", "type": "custom_emoji", "width": 1, "height": 1, "is_animated": false, "is_video": false, "emoji": "⚡", "custom_emoji_id": "bolt"}})
}

// sequence answers method with replies in order, then with the last one.
func sequence(api *fakeAPI, method string, replies ...apiReply) {
	var mu sync.Mutex
	n := 0
	api.on(method, func(url.Values) apiReply {
		mu.Lock()
		defer mu.Unlock()
		r := replies[min(n, len(replies)-1)]
		n++
		return r
	})
}

func connectWith(t *testing.T, ctx context.Context, api *fakeAPI, retry telegram.StartRetry) (*logBuffer, error) {
	t.Helper()
	log, buf := newTestLog(t)
	cfg := domain.Config{Version: 1, BotToken: testToken, ChatID: testChatID, OperatorIDs: []int64{testOperator}}
	_, _, err := telegram.Connect(ctx, cfg, log, func() {}, retry, bot.WithServerURL(api.server.URL))
	return buf, err
}

func TestConnectRetriesUntilTelegramAnswers(t *testing.T) {
	api := startAPI(t)
	sequence(api, "getMe", errReply(502, "Bad Gateway"), errReply(502, "Bad Gateway"), getMeOK())
	var trace startTrace
	buf, err := connectWith(t, ctxT(t), api, trace.retry())
	if err != nil {
		t.Fatalf("Connect = %v", err)
	}
	if got, want := trace.slept(), []time.Duration{2 * time.Second, 4 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	if len(trace.waits) != 2 || trace.waits[0].Attempt != 1 || trace.waits[1].Attempt != 2 || trace.waits[0].Reason == "" {
		t.Fatalf("OnWait = %+v", trace.waits)
	}
	if !trace.waits[0].Since.Equal(trace.waits[1].Since) {
		t.Fatalf("Since moved between attempts: %+v", trace.waits)
	}
	if trace.ready != 3 || trace.readyErr {
		t.Fatalf("OnReady attempts = %d (called twice: %v)", trace.ready, trace.readyErr)
	}
	want := []string{"getMe", "getMe", "getMe", "deleteWebhook", "getForumTopicIconStickers", "setMyCommands"}
	if got := api.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if !strings.Contains(buf.String(), "telegram unreachable, retrying") || !strings.Contains(buf.String(), "telegram reachable") {
		t.Fatalf("log lacks the retry lines:\n%s", buf.String())
	}
	assertNoSecret(t, buf)
}

func TestConnectFirstAttemptSuccessReportsNoWait(t *testing.T) {
	api := startAPI(t)
	var trace startTrace
	if _, err := connectWith(t, ctxT(t), api, trace.retry()); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	if len(trace.slept()) != 0 || len(trace.waits) != 0 || trace.ready != 0 {
		t.Fatalf("waits %v, OnWait %v, OnReady %d on a first-try success", trace.slept(), trace.waits, trace.ready)
	}
}

func TestConnectFinalErrorsAreNotRetried(t *testing.T) {
	cases := map[string]struct {
		reply apiReply
		want  error
	}{
		"401 rejected token": {errReply(401, "Unauthorized"), domain.ErrBotUnauthorized},
		"403 forbidden":      {errReply(403, "Forbidden: bot was blocked"), domain.ErrForbidden},
		"400 bad request":    {errReply(400, "Bad Request: something is wrong"), nil},
		"404 unknown method": {errReply(404, "Not Found"), bot.ErrorNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := startAPI(t)
			api.on("getMe", func(url.Values) apiReply { return tc.reply })
			var trace startTrace
			buf, err := connectWith(t, ctxT(t), api, trace.retry())
			if err == nil {
				t.Fatal("Connect succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Connect = %v, want %v", err, tc.want)
			}
			if n := len(api.callsOf("getMe")); n != 1 {
				t.Fatalf("getMe calls = %d, want 1", n)
			}
			if len(trace.slept()) != 0 || len(trace.waits) != 0 {
				t.Fatalf("retried a final error: waits %v", trace.slept())
			}
			assertNoSecret(t, buf)
		})
	}
}

func TestConnectHonoursRetryAfter(t *testing.T) {
	api := startAPI(t)
	sequence(api, "getMe", tooManyReply(7), getMeOK())
	var trace startTrace
	if _, err := connectWith(t, ctxT(t), api, trace.retry()); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	if got := trace.slept(); len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("waits = %v, want [7s] (Telegram's retry_after over the 2 s backoff)", got)
	}
}

func TestConnectRetriesTheWholeAttemptWhenIconsFail(t *testing.T) {
	api := startAPI(t)
	sequence(api, "getForumTopicIconStickers", errReply(500, "Internal Server Error"), iconsOK())
	var trace startTrace
	if _, err := connectWith(t, ctxT(t), api, trace.retry()); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	if n := len(api.callsOf("getMe")); n != 2 {
		t.Fatalf("getMe calls = %d, want 2 (the attempt repeats from the token check)", n)
	}
	if len(trace.slept()) != 1 {
		t.Fatalf("waits = %v", trace.slept())
	}
}

func TestConnectRetriesAStalledAttempt(t *testing.T) {
	api := startAPI(t)
	var mu sync.Mutex
	first := true
	api.on("getMe", func(url.Values) apiReply {
		mu.Lock()
		stall := first
		first = false
		mu.Unlock()
		if stall {
			time.Sleep(300 * time.Millisecond)
		}
		return getMeOK()
	})
	var trace startTrace
	retry := trace.retry()
	retry.AttemptTimeout = 50 * time.Millisecond
	if _, err := connectWith(t, ctxT(t), api, retry); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	if len(trace.waits) != 1 || trace.ready != 2 {
		t.Fatalf("OnWait %v, OnReady %d: a stalled attempt must be retried", trace.waits, trace.ready)
	}
}

func TestConnectStopsWaitingWhenCancelled(t *testing.T) {
	api := startAPI(t)
	api.on("getMe", func(url.Values) apiReply { return errReply(502, "Bad Gateway") })
	ctx, cancel := context.WithCancel(ctxT(t))
	retry := telegram.StartRetry{Sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}
	buf, err := connectWith(t, ctx, api, retry)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect = %v, want context.Canceled", err)
	}
	if n := len(api.callsOf("getMe")); n != 1 {
		t.Fatalf("getMe calls = %d, want 1", n)
	}
	if !strings.Contains(buf.String(), "telegram start cancelled") {
		t.Fatalf("log lacks the cancel line:\n%s", buf.String())
	}
}

func TestConnectBackoffIsCapped(t *testing.T) {
	api := startAPI(t)
	fail := errReply(502, "Bad Gateway")
	sequence(api, "getMe", fail, fail, fail, fail, fail, fail, getMeOK())
	var trace startTrace
	retry := trace.retry()
	retry.Initial, retry.Max = time.Second, 4*time.Second
	if _, err := connectWith(t, ctxT(t), api, retry); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	if got := trace.slept(); !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
}
