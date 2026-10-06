package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/compose"
)

// errTelegramFatal is the cancellation cause set when the Telegram poller
// reports 401 or 409; the daemon exits 1 with it.
var errTelegramFatal = errors.New("telegram polling stopped: invalid token or another instance is polling")

const (
	// daemonNotifyTimeout bounds the notifications sent while exiting.
	daemonNotifyTimeout = 3 * time.Second
	// telegramStopTimeout bounds the wait for the Telegram poller and queue
	// after the daemon loop returned; it exceeds the loop's own flush
	// budget so a hung poller cannot block exit.
	telegramStopTimeout = 10 * time.Second
)

// runDaemon is the long-lived process spawned by startup and the start
// action. It owns the pid file for its lifetime, logs to daemon.log and
// exits 0 on a normal stop, 1 on a fatal error.
func runDaemon(rc *runContext, _ []string) int {
	env, err := wire.env()
	if err != nil {
		fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", err)
		return exitError
	}
	ctx := context.Background()
	cfg, err := wire.loadConfig(ctx, env, rc.log)
	if err != nil {
		fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", err)
		if isNotConfigured(err) {
			notify(ctx, env, "Not configured: run the setup action to connect a Telegram group", rc.log)
		}
		return exitError
	}

	log, closer, err := wire.fileLogger(env, cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", err)
		return exitError
	}
	defer func() { _ = closer.Close() }()
	// Directories created 0755 by older builds stay 0755 (MkdirAll never
	// changes an existing one), so the daemon tightens what it owns at
	// every start.
	wire.tightenPerms(env, log)

	pid := wire.pidFile(env, log)
	if err := pid.Acquire(os.Getpid()); err != nil {
		if isAlreadyRunning(err) {
			log.Info("daemon already running, exiting", slog.String("err", err.Error()))
			fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", err)
			return exitOK
		}
		log.Error("pid file", slog.String("err", err.Error()))
		fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", err)
		return exitError
	}
	defer func() { _ = pid.Release() }()
	log.Info("daemon starting", slog.String("version", rc.version), slog.Int("pid", os.Getpid()),
		slog.String("state_dir", env.StateDir))
	if recovered, recoverErr := compose.RecoverUpdate(ctx, env, log); recoverErr != nil {
		log.Warn("update recovery failed", slog.Any("err", recoverErr))
	} else if recovered.Phase == "interrupted" || recovered.Terminal() && recovered.NotificationStatus != "sent" {
		deliveryCtx, cancelDelivery := context.WithTimeout(ctx, 10*time.Second)
		if err := compose.DeliverUpdateResult(deliveryCtx, env, cfg, log); err != nil {
			log.Warn("update result pending delivery", slog.Any("err", err))
		}
		cancelDelivery()
	}

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The control channel's stop command cancels the same context a
	// SIGTERM would, so both paths run the same graceful shutdown.
	ctlCtx, requestStop := context.WithCancel(sigCtx)
	defer requestStop()
	runCtx, cancel := context.WithCancelCause(ctlCtx)
	defer cancel(nil)
	fatal := func() { cancel(errTelegramFatal) }

	// The control channel serves stop, resync and status on every
	// platform; on Unix the signal handlers above stay as the fallback for
	// an action from an older build. It is up before the build, so stop and
	// status work while the daemon waits for Telegram.
	start := &startupState{since: time.Now(), version: rc.version, log: log}
	stopControl, err := wire.startControl(ctlCtx, env, compose.ControlHandlers{
		Stop:   requestStop,
		Resync: start.resync,
		Status: start.status,
	}, log)
	if err != nil {
		log.Warn("control channel unavailable, signals only", slog.String("err", err.Error()))
		stopControl = func() {}
	} else {
		log.Info("control channel up")
	}
	var stopControlOnce sync.Once
	closeControl := func() { stopControlOnce.Do(stopControl) }
	defer closeControl()

	d, runTelegram, closeAll, err := wire.buildDaemon(runCtx, env, cfg, log, fatal, start.retry(ctx, env))
	if err != nil {
		if ctlCtx.Err() != nil {
			// A stop action or signal while waiting for Telegram: a normal
			// stop, not a failed start.
			log.Info("daemon stopped before Telegram answered", slog.Int("attempts", start.attempts()),
				slog.String("err", err.Error()))
			return exitOK
		}
		log.Error("daemon build failed", slog.String("err", err.Error()))
		fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", err)
		nctx, ncancel := context.WithTimeout(ctx, daemonNotifyTimeout)
		notify(nctx, env, "Telegram Agents could not start: "+err.Error(), log)
		ncancel()
		return exitError
	}
	defer closeAll()
	// Deferred after closeAll so it runs first: the control channel closes
	// before the Herdr connection, as when it started after the build.
	defer closeControl()
	d.Version = rc.version
	start.ready(d)

	stopResync := watchResync(d.Resync, log)
	defer stopResync()

	runErr := runWithTelegram(ctx, runCtx, d.Run, func(ctx context.Context) {
		d.SetTelegramReady(true)
		defer d.SetTelegramReady(false)
		runTelegram(ctx)
	}, telegramStopTimeout, log)
	cancel(nil)

	code, reason := exitOK, "stopped"
	if runErr != nil {
		code, reason = exitError, runErr.Error()
		fmt.Fprintf(rc.stderr, "herdr-tg daemon: %v\n", runErr)
	}
	log.Info("daemon exit", slog.Int("code", code), slog.String("reason", reason))
	return code
}

// statsWithPID fills in the pid, which the app layer does not read itself.
func statsWithPID(s compose.Stats, pid int) compose.Stats {
	s.PID = pid
	return s
}

// runWithTelegram runs the daemon loop with the Telegram poller and queue
// alongside it. The Telegram side lives on a context derived from parent,
// not from the signal context, so a SIGTERM stops the loop first and the
// loop's shutdown (final topic edits, the stopping notice) still goes out
// through a live queue; only then is the poller stopped and waited for,
// bounded by stopTimeout. A fatal poller error cancels runCtx with its
// cause, so the loop returns first there too.
func runWithTelegram(parent, runCtx context.Context, run func(context.Context) error, telegram func(context.Context),
	stopTimeout time.Duration, log *slog.Logger) error {
	tgCtx, stopTelegram := context.WithCancel(parent)
	defer stopTelegram()
	done := make(chan struct{})
	go func() {
		defer close(done)
		telegram(tgCtx)
	}()
	err := run(runCtx)
	stopTelegram()
	start := time.Now()
	select {
	case <-done:
		log.Debug("telegram runner stopped", slog.Int64("dur_ms", time.Since(start).Milliseconds()))
	case <-time.After(stopTimeout):
		log.Warn("telegram runner did not stop in time, exiting anyway", slog.Int64("dur_ms", time.Since(start).Milliseconds()))
	}
	return err
}

// startupState answers the control channel while the daemon is still
// reaching Telegram and hands over to the daemon once it is built.
type startupState struct {
	since   time.Time
	version string
	log     *slog.Logger

	mu      sync.Mutex
	attempt int
	daemon  *compose.Daemon
}

// retry is the Telegram startup policy with the daemon's reporting: the
// attempt count for status, one Herdr notice when the first attempt fails
// and one when Telegram answers after a wait.
func (s *startupState) retry(ctx context.Context, env compose.PluginEnv) compose.TelegramStartRetry {
	retry := compose.DefaultTelegramStartRetry()
	retry.OnWait = func(w compose.TelegramStartWait) {
		s.mu.Lock()
		s.attempt = w.Attempt
		s.mu.Unlock()
		s.log.Debug("daemon waiting for telegram", slog.Int("attempt", w.Attempt), slog.Int64("wait_ms", w.Wait.Milliseconds()))
		if w.Attempt == 1 {
			nctx, ncancel := context.WithTimeout(ctx, daemonNotifyTimeout)
			notify(nctx, env, "Telegram Agents is waiting for Telegram ("+w.Reason+"); retrying, see the status action", s.log)
			ncancel()
		}
	}
	retry.OnReady = func(_ int, waited time.Duration) {
		nctx, ncancel := context.WithTimeout(ctx, daemonNotifyTimeout)
		notify(nctx, env, "Telegram Agents reached Telegram after "+waited.Round(time.Second).String(), s.log)
		ncancel()
	}
	return retry
}

// attempts is the number of failed Telegram attempts so far.
func (s *startupState) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempt
}

// ready hands status and resync over to the built daemon.
func (s *startupState) ready(d *compose.Daemon) {
	s.mu.Lock()
	s.daemon = d
	attempts := s.attempt
	s.mu.Unlock()
	s.log.Debug("daemon ready, control channel handed over", slog.Int("telegram_failures", attempts))
}

// status is the control channel's status reply: the daemon's line once it
// runs, the starting line before.
func (s *startupState) status() string {
	s.mu.Lock()
	d, attempt := s.daemon, s.attempt
	s.mu.Unlock()
	now := time.Now()
	if d != nil {
		return compose.StatsLine(statsWithPID(d.Stats(), os.Getpid()), now)
	}
	return compose.StartingLine(s.version, os.Getpid(), s.since, attempt, now)
}

// resync forwards to the daemon; before it is built there is nothing to
// resync, and the build syncs everything anyway.
func (s *startupState) resync() {
	s.mu.Lock()
	d := s.daemon
	s.mu.Unlock()
	if d == nil {
		s.log.Debug("resync ignored: daemon still starting")
		return
	}
	d.Resync()
}
