package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
	"github.com/permgps/herdr-telegram-agents/internal/testkit"
)

// fakeUsage is a UsageSource whose answer the test sets.
type fakeUsage struct {
	mu    sync.Mutex
	usage domain.Usage
	ok    bool
	err   error
	reads int
}

func (s *fakeUsage) Usage(context.Context) (domain.Usage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.usage, s.ok, s.err
}

func (s *fakeUsage) set(u domain.Usage, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage, s.ok, s.err = u, ok, err
}

func (s *fakeUsage) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

type quotaFixture struct {
	clock  *testkit.FakeClock
	opts   *Options
	claude *fakeUsage
	codex  *fakeUsage
	logs   *recordHandler
	quota  *Quota
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	f := &quotaFixture{
		clock:  testkit.NewFakeClock(tb0),
		claude: &fakeUsage{},
		codex:  &fakeUsage{},
		logs:   &recordHandler{},
	}
	f.opts = NewOptions(domain.DefaultOptions(), testkit.NewMemOptionsStore(), nil, nil)
	if err := f.opts.Set(context.Background(), domain.OptionSyncQuota, "true", 1); err != nil {
		t.Fatal(err)
	}
	f.quota = NewQuota([]QuotaSource{
		{Provider: domain.UsageClaude, Source: f.claude},
		{Provider: domain.UsageCodex, Source: f.codex},
	}, f.opts, f.clock, slog.New(f.logs))
	return f
}

func usageOf(provider string, observed time.Time, windows ...domain.UsageWindow) domain.Usage {
	return domain.Usage{Provider: provider, ObservedAt: observed, Windows: windows}
}

func TestQuotaLinesRender(t *testing.T) {
	f := newQuotaFixture(t)
	f.claude.set(usageOf(domain.UsageClaude, tb0,
		domain.UsageWindow{Label: "5h", UsedPercent: 18.4, ResetsAt: tb0.Add(2*time.Hour + 13*time.Minute)},
		domain.UsageWindow{Label: "7d", UsedPercent: 82.6, ResetsAt: tb0.Add(28*time.Hour + 40*time.Minute)},
	), true, nil)
	f.codex.set(usageOf(domain.UsageCodex, tb0.Add(-31*time.Minute),
		domain.UsageWindow{Label: "7d", UsedPercent: 3, ResetsAt: tb0.Add(59 * time.Minute)},
	), true, nil)
	want := "🔑 Claude 5h 18% ↻2 h · 7d 83% ↻1 d 4 h\n🔑 Codex 7d 3% ↻59 min · as of 11:29"
	if got := f.quota.Lines(context.Background()); got != want {
		t.Fatalf("Lines =\n%q\nwant\n%q", got, want)
	}
	if f.logs.count(slog.LevelInfo, "quota source ready") != 2 {
		t.Error("no ready line per provider")
	}
}

func TestQuotaDropsWindowsPastTheirReset(t *testing.T) {
	f := newQuotaFixture(t)
	f.claude.set(usageOf(domain.UsageClaude, tb0,
		domain.UsageWindow{Label: "5h", UsedPercent: 90, ResetsAt: tb0.Add(30 * time.Second)},
		domain.UsageWindow{Label: "7d", UsedPercent: 40, ResetsAt: tb0.Add(3 * 24 * time.Hour)},
	), true, nil)
	if got := f.quota.Lines(context.Background()); got != "🔑 Claude 5h 90% ↻1 min · 7d 40% ↻3 d 0 h" {
		t.Fatalf("before reset = %q", got)
	}
	// The 5-hour window resets; the cached numbers are not re-read yet.
	f.clock.Advance(time.Minute)
	if got := f.quota.Lines(context.Background()); got != "🔑 Claude 7d 40% ↻2 d 23 h" {
		t.Fatalf("after reset = %q", got)
	}
	f.clock.Advance(3 * 24 * time.Hour)
	if got := f.quota.Lines(context.Background()); got != "" {
		t.Fatalf("all windows past = %q", got)
	}
}

func TestQuotaReadsAtMostOncePerRefresh(t *testing.T) {
	f := newQuotaFixture(t)
	ctx := context.Background()
	f.quota.Lines(ctx)
	f.quota.Lines(ctx)
	f.clock.Advance(quotaRefresh - time.Second)
	f.quota.Lines(ctx)
	if n := f.claude.count(); n != 1 {
		t.Fatalf("reads within the refresh = %d", n)
	}
	f.clock.Advance(time.Second)
	f.quota.Lines(ctx)
	if n := f.claude.count(); n != 2 {
		t.Fatalf("reads after the refresh = %d", n)
	}
}

func TestQuotaKeepsTheLastValueOnError(t *testing.T) {
	f := newQuotaFixture(t)
	ctx := context.Background()
	f.codex.set(usageOf(domain.UsageCodex, tb0, domain.UsageWindow{Label: "7d", UsedPercent: 10, ResetsAt: tb0.Add(48 * time.Hour)}), true, nil)
	first := f.quota.Lines(ctx)
	f.codex.set(domain.Usage{}, false, errors.New("disk on fire"))
	for i := 0; i < 3; i++ {
		f.clock.Advance(quotaRefresh)
		if got := f.quota.Lines(ctx); !strings.HasPrefix(got, "🔑 Codex 7d 10%") {
			t.Fatalf("after error %d = %q (first %q)", i, got, first)
		}
	}
	if n := f.logs.count(slog.LevelWarn, "quota source failed"); n != 1 {
		t.Errorf("warn lines = %d, want 1", n)
	}
	// No data (the file was removed) drops the line.
	f.codex.set(domain.Usage{}, false, nil)
	f.clock.Advance(quotaRefresh)
	if got := f.quota.Lines(ctx); got != "" {
		t.Errorf("no data = %q", got)
	}
}

func TestQuotaOptionOffAndNil(t *testing.T) {
	f := newQuotaFixture(t)
	f.claude.set(usageOf(domain.UsageClaude, tb0, domain.UsageWindow{Label: "5h", UsedPercent: 1, ResetsAt: tb0.Add(time.Hour)}), true, nil)
	if err := f.opts.Set(context.Background(), domain.OptionSyncQuota, "false", 1); err != nil {
		t.Fatal(err)
	}
	if got := f.quota.Lines(context.Background()); got != "" || f.claude.count() != 0 {
		t.Fatalf("option off = %q, reads %d", got, f.claude.count())
	}
	var none *Quota
	if got := none.Lines(context.Background()); got != "" {
		t.Errorf("nil quota = %q", got)
	}
}

func TestQuotaLeft(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Second, "1 min"},
		{59 * time.Minute, "59 min"},
		{time.Hour, "1 h"},
		{3*time.Hour + 59*time.Minute, "3 h"},
		{24 * time.Hour, "1 d 0 h"},
		{2*24*time.Hour + 5*time.Hour + 30*time.Minute, "2 d 5 h"},
	} {
		if got := quotaLeft(c.d); got != c.want {
			t.Errorf("quotaLeft(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
