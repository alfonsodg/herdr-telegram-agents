package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/adapters/state"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

var tapNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func tapClock() time.Time { return tapNow }

// statusLineInput mirrors the shape Claude Code hands its status line
// command; the other fields must never reach the stored file.
const statusLineInput = `{"session_id":"s-1","cwd":"/home/x/secret","model":{"display_name":"Opus"},` +
	`"cost":{"total_cost_usd":1.5},"rate_limits":{"five_hour":{"used_percentage":18.4,"resets_at":1791363931.6},` +
	`"seven_day":{"used_percentage":83,"resets_at":1791540000}}}`

func tapUsage(t *testing.T, in string, path string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := state.TapClaudeUsage(strings.NewReader(in), &out, path, tapClock)
	return out.String(), err
}

func TestTapClaudeUsageStoresOnlyTheWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", state.ClaudeUsageFileName)
	out, err := tapUsage(t, statusLineInput, path)
	if err != nil {
		t.Fatal(err)
	}
	if out != statusLineInput {
		t.Fatalf("passthrough = %q", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys["observed_at"] == nil || keys["five_hour"] == nil || keys["seven_day"] == nil {
		t.Fatalf("stored keys = %s", data)
	}
	for _, leak := range []string{"secret", "s-1", "Opus", "cost"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("stored file leaks %q: %s", leak, data)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}

	u, ok, err := state.NewClaudeUsageFile(path).Usage(context.Background())
	if err != nil || !ok {
		t.Fatalf("Usage = %v, %v", ok, err)
	}
	if u.Provider != domain.UsageClaude || !u.ObservedAt.Equal(tapNow) || len(u.Windows) != 2 {
		t.Fatalf("usage = %+v", u)
	}
	five, week := u.Windows[0], u.Windows[1]
	if five.Label != "5h" || five.UsedPercent != 18.4 || five.ResetsAt.Unix() != 1791363931 || five.ResetsAt.Nanosecond() == 0 {
		t.Errorf("five hour = %+v", five)
	}
	if week.Label != "7d" || week.UsedPercent != 83 || !week.ResetsAt.Equal(time.Unix(1791540000, 0)) {
		t.Errorf("seven day = %+v", week)
	}
}

func TestTapClaudeUsageKeepsTheFileWithoutRateLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), state.ClaudeUsageFileName)
	if _, err := tapUsage(t, statusLineInput, path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, in := range []string{
		`{"cwd":"/x"}`,
		`{"rate_limits":{}}`,
		`{"rate_limits":{"five_hour":{"used_percentage":5}}}`,
		``,
	} {
		out, err := tapUsage(t, in, path)
		if err != nil {
			t.Errorf("%q: %v", in, err)
		}
		if out != in {
			t.Errorf("%q passthrough = %q", in, out)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Errorf("%q rewrote the file: %s", in, after)
		}
	}
}

func TestTapClaudeUsagePassesThroughWhatItCannotParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), state.ClaudeUsageFileName)
	out, err := tapUsage(t, "not json", path)
	if err == nil || out != "not json" {
		t.Fatalf("malformed: out %q err %v", out, err)
	}
	big := `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":2}},"pad":"` + strings.Repeat("x", 1<<20) + `"}`
	out, err = tapUsage(t, big, path)
	if err == nil || out != big {
		t.Fatalf("oversized: len(out) %d err %v", len(out), err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("file written: %v", statErr)
	}
}

func TestClaudeUsageFileMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := state.NewClaudeUsageFile(filepath.Join(dir, "none.json")).Usage(context.Background()); ok || err != nil {
		t.Errorf("missing file = %v, %v", ok, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := state.NewClaudeUsageFile(bad).Usage(context.Background()); ok || err == nil {
		t.Errorf("malformed file = %v, %v", ok, err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"observed_at":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := state.NewClaudeUsageFile(empty).Usage(context.Background()); ok || err != nil {
		t.Errorf("file without windows = %v, %v", ok, err)
	}
}
