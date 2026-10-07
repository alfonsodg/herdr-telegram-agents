package system

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// TestIdleSourceOnHost samples the real source once. macOS and Windows have
// one; a build agent without an input session may fail the call, which is
// allowed as long as the failure is not "unsupported". Linux answers
// ErrIdleUnsupported without a desktop session (CI) and may answer either
// way on a developer desktop. Everything else must answer
// ErrIdleUnsupported.
func TestIdleSourceOnHost(t *testing.T) {
	src := NewIdleSource(nil)
	d, err := src.Idle(context.Background())
	switch runtime.GOOS {
	case "darwin", "windows":
		if errors.Is(err, domain.ErrIdleUnsupported) {
			t.Fatalf("%s reports unsupported", runtime.GOOS)
		}
		if err != nil {
			t.Logf("idle sample failed on this host (tolerated): %v", err)
			return
		}
		if d < 0 {
			t.Errorf("negative idle %v", d)
		}
	case "linux":
		if err != nil {
			t.Logf("idle sample on this host: %v", err)
			return
		}
		if d < 0 {
			t.Errorf("negative idle %v", d)
		}
	default:
		if !errors.Is(err, domain.ErrIdleUnsupported) {
			t.Fatalf("Idle = %v, %v; want ErrIdleUnsupported", d, err)
		}
	}
}

// TestIdleSourceLogsSourceChange checks the info line naming the source:
// once on the first answer, again only when another source answers.
func TestIdleSourceLogsSourceChange(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	src := NewIdleSource(log)
	answers := []struct {
		source string
		err    error
	}{{"mutter", nil}, {"mutter", nil}, {"", errors.New("boom")}, {"mutter", nil}, {"xprintidle", nil}, {"mutter", nil}}
	i := 0
	src.sample = func(context.Context) (time.Duration, string, error) {
		a := answers[i]
		i++
		return time.Second, a.source, a.err
	}
	for range answers {
		d, err := src.Idle(context.Background())
		if err == nil && d != time.Second {
			t.Errorf("Idle = %v; want 1s", d)
		}
	}
	got := strings.Count(buf.String(), `msg="input idle source"`)
	if got != 3 {
		t.Errorf("logged the source %d times; want 3 (mutter, xprintidle, mutter):\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "source=xprintidle") {
		t.Errorf("log lacks source=xprintidle:\n%s", buf.String())
	}
}

// TestIdleSourceUnsupportedPassesThrough keeps ErrIdleUnsupported
// recognisable and unwrapped for the tracker.
func TestIdleSourceUnsupportedPassesThrough(t *testing.T) {
	src := NewIdleSource(nil)
	src.sample = func(context.Context) (time.Duration, string, error) {
		return 0, "", domain.ErrIdleUnsupported
	}
	if _, err := src.Idle(context.Background()); err != domain.ErrIdleUnsupported {
		t.Errorf("Idle err = %v; want ErrIdleUnsupported itself", err)
	}
}
