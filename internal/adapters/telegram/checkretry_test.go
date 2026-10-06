package telegram

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// fastRetries shortens the backoff for tests.
func fastRetries(t *testing.T) {
	t.Helper()
	old := checkRetryBackoff
	checkRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { checkRetryBackoff = old })
}

func TestCheckWithRetryRecoversFromTransientErrors(t *testing.T) {
	fastRetries(t)
	attempts := 0
	attempt := func(context.Context) (BotIdentity, error) {
		attempts++
		if attempts < 3 {
			return BotIdentity{}, fmt.Errorf("getMe: %w", errors.New("dial tcp: network is unreachable"))
		}
		return BotIdentity{ID: 42, Username: "bot"}, nil
	}
	id, err := checkWithRetry(context.Background(), nil, attempt)
	if err != nil || id.ID != 42 || attempts != 3 {
		t.Fatalf("id = %+v, err = %v, attempts = %d", id, err, attempts)
	}
}

func TestCheckWithRetryFatalErrorsDoNotRetry(t *testing.T) {
	fastRetries(t)
	attempts := 0
	attempt := func(context.Context) (BotIdentity, error) {
		attempts++
		return BotIdentity{}, fmt.Errorf("getMe: %w", domain.ErrBotUnauthorized)
	}
	if _, err := checkWithRetry(context.Background(), nil, attempt); !errors.Is(err, domain.ErrBotUnauthorized) || attempts != 1 {
		t.Fatalf("err = %v, attempts = %d", err, attempts)
	}
}

func TestCheckWithRetryGivesUpAfterTheBackoff(t *testing.T) {
	fastRetries(t)
	attempts := 0
	attempt := func(context.Context) (BotIdentity, error) {
		attempts++
		return BotIdentity{}, errors.New("dial tcp: network is unreachable")
	}
	if _, err := checkWithRetry(context.Background(), nil, attempt); err == nil || attempts != len(checkRetryBackoff)+1 {
		t.Fatalf("err = %v, attempts = %d", err, attempts)
	}
}

func TestCheckWithRetryStopsWithTheContext(t *testing.T) {
	fastRetries(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	attempt := func(context.Context) (BotIdentity, error) {
		attempts++
		return BotIdentity{}, errors.New("dial tcp: network is unreachable")
	}
	if _, err := checkWithRetry(ctx, nil, attempt); !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err = %v, attempts = %d", err, attempts)
	}
}
