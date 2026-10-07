package transcript

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

var usageNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)

// tokenCount renders a token_count line in the shape Codex writes it.
func tokenCount(stamp, limitID, primary, secondary string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{}},`+
		`"rate_limits":{"limit_id":%q,"limit_name":null,"primary":%s,"secondary":%s,"credits":null,"plan_type":"plus"}}}`,
		stamp, limitID, primary, secondary)
}

func window(pct float64, minutes int, resets int64) string {
	return fmt.Sprintf(`{"used_percent":%v,"window_minutes":%d,"resets_at":%d}`, pct, minutes, resets)
}

// writeRollout writes lines into a day directory of home's sessions tree
// and sets the file's modification time.
func writeRollout(t *testing.T, home string, day time.Time, name string, mod time.Time, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func usageFrom(t *testing.T, home string) (domain.Usage, bool) {
	t.Helper()
	c := newCodexUsage(func() (string, error) { return home, nil }, func() time.Time { return usageNow }, nil)
	u, ok, err := c.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return u, ok
}

func TestCodexUsageNewestFileWins(t *testing.T) {
	home := t.TempDir()
	resets := usageNow.Add(48 * time.Hour).Unix()
	writeRollout(t, home, usageNow.AddDate(0, 0, -1), "rollout-old.jsonl", usageNow.Add(-24*time.Hour),
		tokenCount("2026-10-06T12:00:00Z", "codex", window(50, 10080, resets), "null"))
	writeRollout(t, home, usageNow, "rollout-new.jsonl", usageNow.Add(-time.Minute),
		tokenCount("2026-10-07T09:00:00Z", "codex", window(3, 10080, resets), "null"),
		`{"timestamp":"2026-10-07T09:00:01Z","type":"response_item","payload":{"type":"message"}}`)
	u, ok := usageFrom(t, home)
	if !ok || u.Provider != domain.UsageCodex || len(u.Windows) != 1 {
		t.Fatalf("usage = %+v, %v", u, ok)
	}
	if w := u.Windows[0]; w.Label != "7d" || w.UsedPercent != 3 || w.ResetsAt.Unix() != resets {
		t.Errorf("window = %+v", w)
	}
	if !u.ObservedAt.Equal(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("observed at = %v", u.ObservedAt)
	}
}

func TestCodexUsageMapsWindowsByLength(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, usageNow, "rollout-a.jsonl", usageNow,
		tokenCount("2026-10-07T09:00:00Z", "codex", window(40, 10080, 2000000000), window(12.6, 300, 1900000000)))
	u, ok := usageFrom(t, home)
	if !ok || len(u.Windows) != 2 || u.Windows[0].Label != "5h" || u.Windows[0].UsedPercent != 12.6 || u.Windows[1].Label != "7d" {
		t.Fatalf("usage = %+v, %v", u, ok)
	}
}

func TestCodexUsageSkipsWhatIsNotTheAccountLimit(t *testing.T) {
	home := t.TempDir()
	resets := usageNow.Add(time.Hour).Unix()
	// The newest file has only an API-key style null and a scoped limit;
	// the older one has the account limit.
	writeRollout(t, home, usageNow, "rollout-new.jsonl", usageNow,
		`{"timestamp":"2026-10-07T10:00:00Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":null}}`,
		tokenCount("2026-10-07T10:01:00Z", "codex_other", window(90, 300, resets), "null"),
		`{"timestamp":"2026-10-07T10:02:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primar`)
	writeRollout(t, home, usageNow, "rollout-old.jsonl", usageNow.Add(-time.Hour),
		tokenCount("2026-10-07T08:00:00Z", "", window(7, 300, resets), "null"))
	u, ok := usageFrom(t, home)
	if !ok || len(u.Windows) != 1 || u.Windows[0].UsedPercent != 7 {
		t.Fatalf("usage = %+v, %v", u, ok)
	}
}

func TestCodexUsageNoData(t *testing.T) {
	if _, ok := usageFrom(t, t.TempDir()); ok {
		t.Error("no sessions directory gave data")
	}
	home := t.TempDir()
	writeRollout(t, home, usageNow.AddDate(0, 0, -9), "rollout-ancient.jsonl", usageNow.AddDate(0, 0, -9),
		tokenCount("2026-09-28T08:00:00Z", "codex", window(7, 300, 1), "null"))
	writeRollout(t, home, usageNow, "notes.jsonl", usageNow,
		tokenCount("2026-10-07T08:00:00Z", "codex", window(7, 300, 1), "null"))
	if u, ok := usageFrom(t, home); ok {
		t.Errorf("old or foreign file gave data: %+v", u)
	}
}

func TestCodexUsageDayDirsCoverTheWeek(t *testing.T) {
	dirs := codexUsageDayDirs(usageNow)
	want := map[string]bool{
		filepath.Join("2026", "10", "08"): true,
		filepath.Join("2026", "10", "07"): true,
		filepath.Join("2026", "09", "30"): true,
	}
	for _, d := range dirs {
		delete(want, d)
		if d == filepath.Join("2026", "09", "28") {
			t.Errorf("scans %s, more than a week back", d)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing day dirs %v in %v", want, dirs)
	}
}
