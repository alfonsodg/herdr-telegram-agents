package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTap(t *testing.T, input string, args ...string) (code int, out, errOut string) {
	t.Helper()
	saved := stdin
	stdin = strings.NewReader(input)
	t.Cleanup(func() { stdin = saved })
	var o, e bytes.Buffer
	code = Run(append([]string{"usage-tap"}, args...), "test", &o, &e)
	return code, o.String(), e.String()
}

func TestUsageTapPassesThroughAndWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-usage.json")
	in := `{"cwd":"/x","rate_limits":{"five_hour":{"used_percentage":18,"resets_at":1791363931}}}`
	code, out, errOut := runTap(t, in, "--out", path)
	if code != exitOK || out != in || errOut != "" {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"five_hour"`) || strings.Contains(string(data), "cwd") {
		t.Fatalf("file %s, %v", data, err)
	}
}

func TestUsageTapMissingOutStillPassesThrough(t *testing.T) {
	code, out, errOut := runTap(t, "status json")
	if code != exitUsage || out != "status json" || !strings.Contains(errOut, usageTapUsage) {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestUsageTapBadInputExitsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-usage.json")
	code, out, errOut := runTap(t, "not json", "--out", path)
	if code != exitOK || out != "not json" || !strings.HasPrefix(errOut, "usage-tap: ") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}
