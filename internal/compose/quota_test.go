package compose

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShellWord(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/Users/a/.config/herdr/plugins/x/bin/herdr-tg", "/Users/a/.config/herdr/plugins/x/bin/herdr-tg"},
		{"/Users/a b/state/claude-usage.json", "'/Users/a b/state/claude-usage.json'"},
		{"/tmp/it's", `'/tmp/it'\''s'`},
		{`C:\Users\a\herdr-tg.exe`, `'C:\Users\a\herdr-tg.exe'`},
		{"", "''"},
	} {
		if got := shellWord(c.in); got != c.want {
			t.Errorf("shellWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeTapCommand(t *testing.T) {
	file := filepath.Join(t.TempDir(), "claude-usage.json")
	got := claudeTapCommand(PluginEnv{}, file)
	if !strings.Contains(got, " usage-tap --out ") || !strings.HasSuffix(got, shellWord(file)) {
		t.Fatalf("tap command = %q", got)
	}
}
