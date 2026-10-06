package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/compose"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

func TestDaemonNotConfigured(t *testing.T) {
	_, rec := testEnv(t)
	code, _, stderr := runCLI(t, "daemon")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "not configured") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := rec.Bodies(); len(got) != 1 || !strings.Contains(got[0], "setup action") {
		t.Fatalf("notifications = %q", got)
	}
}

func TestDaemonAlreadyRunningExitsZero(t *testing.T) {
	env, rec := testEnv(t)
	saveConfig(t, env)
	pid := wire.pidFile(env, nil)
	if err := pid.Acquire(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pid.Release() })

	code, _, stderr := runCLI(t, "daemon")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "already running") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := rec.Bodies(); len(got) != 0 {
		t.Fatalf("unexpected notifications %q", got)
	}
	if _, err := os.Stat(filepath.Join(env.StateDir, "daemon.log")); err != nil {
		t.Fatalf("daemon.log missing: %v", err)
	}
}

func TestDaemonHerdrUnreachable(t *testing.T) {
	env, rec := testEnv(t)
	saveConfig(t, env)
	code, _, stderr := runCLI(t, "daemon")
	if code != exitError {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "herdr connect") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := rec.Bodies(); len(got) != 1 || !strings.Contains(got[0], "could not start") {
		t.Fatalf("notifications = %q", got)
	}
	// The pid file must not linger after a failed start.
	if _, err := os.Stat(filepath.Join(env.StateDir, "daemon.pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file still present (err %v)", err)
	}
	data, err := os.ReadFile(filepath.Join(env.StateDir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"daemon starting"`) || !strings.Contains(string(data), `"daemon build failed"`) {
		t.Fatalf("daemon.log = %s", data)
	}
	if strings.Contains(string(data), "123:abc") {
		t.Fatal("daemon.log leaks the token")
	}
}

func TestDaemonOutsideHerdr(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	code, _, stderr := runCLI(t, "daemon")
	if code != exitError || !strings.Contains(stderr, "HERDR_PLUGIN_CONFIG_DIR") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestRunWithTelegramStopsPollerAfterLoop(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	tgStopped := make(chan struct{})
	telegram := func(ctx context.Context) {
		<-ctx.Done()
		record("telegram stopped")
		close(tgStopped)
	}
	run := func(ctx context.Context) error {
		<-ctx.Done()
		// The loop's shutdown still needs the Telegram side: it must not
		// have been cancelled yet.
		select {
		case <-tgStopped:
			record("flush without telegram")
		default:
			record("flush")
		}
		return context.Cause(ctx)
	}
	go cancel(errTelegramFatal)
	err := runWithTelegram(context.Background(), runCtx, run, telegram, time.Second, slog.New(slog.DiscardHandler))
	if !errors.Is(err, errTelegramFatal) {
		t.Fatalf("err = %v, want the cancellation cause", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "flush" || order[1] != "telegram stopped" {
		t.Fatalf("order = %v", order)
	}
}

func TestRunWithTelegramGivesUpOnHungPoller(t *testing.T) {
	runCtx, cancel := context.WithCancelCause(context.Background())
	cancel(nil)
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	telegram := func(context.Context) { <-hung }
	run := func(ctx context.Context) error { return nil }
	start := time.Now()
	if err := runWithTelegram(context.Background(), runCtx, run, telegram, 20*time.Millisecond, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("hung poller blocked exit")
	}
}

// stubStart replaces the control channel and the daemon build: the control
// handlers are captured, and build runs in place of compose.BuildDaemon.
func stubStart(t *testing.T, build func(ctx context.Context, retry compose.TelegramStartRetry) error) <-chan compose.ControlHandlers {
	t.Helper()
	handlers := make(chan compose.ControlHandlers, 1)
	wire.startControl = func(_ context.Context, _ compose.PluginEnv, h compose.ControlHandlers, _ *slog.Logger) (func(), error) {
		handlers <- h
		return func() {}, nil
	}
	wire.buildDaemon = func(ctx context.Context, _ compose.PluginEnv, _ domain.Config, _ *slog.Logger, _ context.CancelFunc,
		retry compose.TelegramStartRetry) (*compose.Daemon, func(context.Context), func(), error) {
		return nil, nil, nil, build(ctx, retry)
	}
	return handlers
}

func TestDaemonWaitsForTelegramAndStopsQuietly(t *testing.T) {
	env, rec := testEnv(t)
	cfg := saveConfig(t, env)
	// Debug level, so the ignored resync shows in daemon.log.
	cfg.LogLevel = "debug"
	if err := compose.ConfigStore(env, nil).Save(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	handlers := stubStart(t, func(ctx context.Context, retry compose.TelegramStartRetry) error {
		for attempt := 1; attempt <= 2; attempt++ {
			retry.OnWait(compose.TelegramStartWait{Attempt: attempt, Wait: 2 * time.Second, Since: time.Now(), Reason: "dial tcp: no route to host"})
		}
		close(waiting)
		<-ctx.Done()
		return fmt.Errorf("telegram start: %w", ctx.Err())
	})
	done := make(chan int, 1)
	go func() {
		code, _, _ := runCLI(t, "daemon")
		done <- code
	}()
	var h compose.ControlHandlers
	select {
	case h = <-handlers:
	case <-time.After(5 * time.Second):
		t.Fatal("control channel was not started before the build")
	}
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("build never waited")
	}
	if got := h.Status(); !strings.Contains(got, "telegram=waiting attempt=2") || strings.Contains(got, "telegram=ready") {
		t.Fatalf("status while waiting = %q", got)
	}
	h.Resync() // nothing to resync yet; must not panic
	h.Stop()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit = %d, want 0 for a stop while waiting", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	got := rec.Bodies()
	if len(got) != 1 || !strings.Contains(got[0], "waiting for Telegram") || !strings.Contains(got[0], "no route to host") {
		t.Fatalf("notifications = %q, want one waiting notice", got)
	}
	data, err := os.ReadFile(filepath.Join(env.StateDir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "daemon stopped before Telegram answered") || strings.Contains(string(data), "daemon build failed") {
		t.Fatalf("daemon.log = %s", data)
	}
	if !strings.Contains(string(data), "resync ignored: daemon still starting") {
		t.Fatalf("a resync before the daemon was built must be ignored and logged; daemon.log = %s", data)
	}
	if _, err := os.Stat(filepath.Join(env.StateDir, "daemon.pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file still present (err %v)", err)
	}
}

func TestDaemonFinalTelegramErrorStillFailsTheStart(t *testing.T) {
	env, rec := testEnv(t)
	saveConfig(t, env)
	stubStart(t, func(context.Context, compose.TelegramStartRetry) error {
		return fmt.Errorf("telegram check: getMe: %w", domain.ErrBotUnauthorized)
	})
	code, _, stderr := runCLI(t, "daemon")
	if code != exitError {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if got := rec.Bodies(); len(got) != 1 || !strings.Contains(got[0], "could not start") {
		t.Fatalf("notifications = %q", got)
	}
}

func TestStartupStateNotices(t *testing.T) {
	env, rec := testEnv(t)
	start := &startupState{since: time.Now(), version: "v1", log: slog.New(slog.DiscardHandler)}
	retry := start.retry(context.Background(), env)
	if got := start.status(); !strings.Contains(got, "version=v1 ") || !strings.Contains(got, "telegram=connecting") {
		t.Fatalf("status before any failure = %q", got)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		retry.OnWait(compose.TelegramStartWait{Attempt: attempt, Reason: "timeout"})
	}
	if start.attempts() != 3 {
		t.Fatalf("attempts = %d", start.attempts())
	}
	retry.OnReady(4, 5*time.Second+300*time.Millisecond)
	got := rec.Bodies()
	if len(got) != 2 || !strings.Contains(got[0], "waiting for Telegram (timeout)") || !strings.Contains(got[1], "reached Telegram after 5s") {
		t.Fatalf("notifications = %q, want one waiting and one reached notice", got)
	}
}
