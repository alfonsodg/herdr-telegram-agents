package telegram_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/permgps/herdr-telegram-agents/internal/adapters/telegram"
)

// botWithTimeouts points a client at the fake server with separated
// deadlines, mirroring how Connect wires the HTTP client and the SDK.
func (f *fakeAPI) botWithTimeouts(t *testing.T, log *slog.Logger, fatal context.CancelFunc, ts telegram.Timeouts) *bot.Bot {
	t.Helper()
	b, err := telegram.NewBot(testToken, log, fatal,
		bot.WithServerURL(f.server.URL),
		bot.WithHTTPClient(ts.Poll, &http.Client{Timeout: ts.HTTP}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// waitGetUpdates waits until the fake server saw n getUpdates requests.
func waitGetUpdates(t *testing.T, api *fakeAPI, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(api.callsOf("getUpdates")) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("getUpdates calls = %d, want at least %d", len(api.callsOf("getUpdates")), n)
}

// waitInFlight waits until the getUpdates handler is inside its hold.
func waitInFlight(t *testing.T, inFlight *int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(inFlight) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("in-flight polls = %d, want %d", atomic.LoadInt32(inFlight), want)
}

func TestNewTimeoutsRejectsInvalidRelationships(t *testing.T) {
	const poll = time.Minute
	for _, tc := range []struct {
		name       string
		poll, http time.Duration
	}{
		{"zero poll", 0, poll},
		{"negative poll", -time.Second, poll},
		{"zero http deadline", poll, 0},
		{"negative http deadline", poll, -time.Second},
		{"deadline below the poll window", poll, poll - time.Second},
		{"deadline equal to the poll window", poll, poll},
		{"margin below the minimum", poll, poll + telegram.MinHTTPDeadlineMargin - time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ts, err := telegram.NewTimeouts(tc.poll, tc.http); err == nil {
				t.Fatalf("accepted invalid timeouts %+v", ts)
			}
		})
	}
	ts, err := telegram.NewTimeouts(poll, poll+telegram.MinHTTPDeadlineMargin)
	if err != nil {
		t.Fatalf("rejected the minimum accepted margin: %v", err)
	}
	if ts.Poll != poll || ts.HTTP != poll+telegram.MinHTTPDeadlineMargin {
		t.Fatalf("timeouts = %+v", ts)
	}
}

func TestDefaultTimeoutsLeaveMargin(t *testing.T) {
	ts := telegram.DefaultTimeouts()
	if ts.Poll != telegram.DefaultPollWindow {
		t.Fatalf("default poll window = %v, want %v", ts.Poll, telegram.DefaultPollWindow)
	}
	if got, want := ts.HTTP-ts.Poll, telegram.DefaultHTTPDeadlineMargin; got != want {
		t.Fatalf("default margin = %v, want %v", got, want)
	}
	if telegram.DefaultHTTPDeadlineMargin < telegram.MinHTTPDeadlineMargin {
		t.Fatal("default margin is below the minimum accepted margin")
	}
	if _, err := telegram.NewTimeouts(ts.Poll, ts.HTTP); err != nil {
		t.Fatalf("defaults rejected by validation: %v", err)
	}
}

// TestLongPollOutlivesConfiguredWindow holds a getUpdates reply for the whole
// long-poll window: the HTTP deadline must not expire at its end.
func TestLongPollOutlivesConfiguredWindow(t *testing.T) {
	api := newFakeAPI(t)
	const pollWindow = 2 * time.Second
	ts, err := telegram.NewTimeouts(pollWindow, pollWindow+telegram.MinHTTPDeadlineMargin)
	if err != nil {
		t.Fatal(err)
	}
	var calls int32
	api.on("getUpdates", func(url.Values) apiReply {
		if atomic.AddInt32(&calls, 1) == 1 {
			time.Sleep(pollWindow)
		}
		return okReply([]models.Update{})
	})
	log, buf := newTestLog(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	b := api.botWithTimeouts(t, log, cancel, ts)

	done := runPoll(ctx, b, log)
	waitGetUpdates(t, api, 2, 8*time.Second)
	cancel()
	waitDone(t, done, "held poll")
	if out := buf.String(); strings.Contains(out, "telegram polling error") {
		t.Fatalf("the HTTP deadline expired inside the poll window: %s", out)
	}
	assertNoSecret(t, buf)
}

// TestOutboundCompletesWhileLongPollActive sends a Bot API request while a
// getUpdates request is held open by the server.
func TestOutboundCompletesWhileLongPollActive(t *testing.T) {
	api := newFakeAPI(t)
	const pollWindow = 2 * time.Second
	ts, err := telegram.NewTimeouts(pollWindow, pollWindow+telegram.MinHTTPDeadlineMargin)
	if err != nil {
		t.Fatal(err)
	}
	var inFlight int32
	api.on("getUpdates", func(url.Values) apiReply {
		atomic.AddInt32(&inFlight, 1)
		time.Sleep(pollWindow)
		atomic.AddInt32(&inFlight, -1)
		return okReply([]models.Update{})
	})
	api.on("sendMessage", func(url.Values) apiReply {
		return okReply(map[string]any{"message_id": 5, "date": 100, "chat": map[string]any{"id": 1, "type": "private"}})
	})
	log, buf := newTestLog(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	b := api.botWithTimeouts(t, log, cancel, ts)

	done := runPoll(ctx, b, log)
	waitInFlight(t, &inFlight, 1, 5*time.Second)
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: 1, Text: "hi"}); err != nil {
		t.Fatalf("outbound while polling: %v", err)
	}
	if got := atomic.LoadInt32(&inFlight); got != 1 {
		t.Fatalf("outbound was serialized behind the long poll (in-flight = %d)", got)
	}
	cancel()
	waitDone(t, done, "outbound")
	if n := len(api.callsOf("sendMessage")); n != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", n)
	}
	assertNoSecret(t, buf)
}

// TestGetUpdatesContract pins the request shape and offset advance the SDK
// derives from the configured poll window.
func TestGetUpdatesContract(t *testing.T) {
	api := newFakeAPI(t)
	const pollWindow = 2 * time.Second
	ts, err := telegram.NewTimeouts(pollWindow, pollWindow+telegram.MinHTTPDeadlineMargin)
	if err != nil {
		t.Fatal(err)
	}
	var calls int32
	api.on("getUpdates", func(url.Values) apiReply {
		if atomic.AddInt32(&calls, 1) == 1 {
			return okReply([]models.Update{{ID: 7}})
		}
		return okReply([]models.Update{})
	})
	log, buf := newTestLog(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	b := api.botWithTimeouts(t, log, cancel, ts)

	done := runPoll(ctx, b, log)
	waitGetUpdates(t, api, 2, 5*time.Second)
	cancel()
	waitDone(t, done, "contract")

	got := api.callsOf("getUpdates")
	if timeout := got[0].form.Get("timeout"); timeout != "1" {
		t.Fatalf("getUpdates timeout = %q, want %q (poll window - 1s)", timeout, "1")
	}
	if offset := got[1].form.Get("offset"); offset != "8" {
		t.Fatalf("second getUpdates offset = %q, want %q", offset, "8")
	}
	assertNoSecret(t, buf)
}
