package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// museFixture writes a miniature Muse state tree: one runtime session file
// and the durable log of one session.
type museFixture struct {
	home    string
	session string
	lines   []string
	noLog   bool
}

const museSessionID = "01a10885-b7fb-7ef0-95cd-4fb8af9067ca"

func newMuseFixture(t *testing.T, workspace string, lines []string) *museFixture {
	t.Helper()
	f := &museFixture{home: t.TempDir(), session: museSessionID, lines: lines}
	rt := filepath.Join(f.home, ".local", "share", "muse", "runtime", "muse", "sessions")
	if err := os.MkdirAll(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schema_version":1,"session_id":%q,"session_name":null,"endpoint_hint":"x","workspace_label":%q,"target_eligibility":"message_capable","process_generation_hint":"pid=1"}`, museSessionID, workspace)
	if err := os.WriteFile(filepath.Join(rt, museSessionID+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(lines) > 0 {
		dir := filepath.Join(f.home, ".local", "share", "muse", "sessions", "2026", "10", "06", museSessionID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *museFixture) reader() *MuseReader {
	return newMuseReader(func() (string, error) { return f.home, nil }, func() time.Time { return time.Unix(1_791_000_000, 0) }, nil)
}

func museAgent(cwd string) domain.Agent {
	return domain.Agent{Key: domain.Key{PaneID: "p1"}, Kind: "muse", Cwd: cwd}
}

// museMessage is one assistant_message_committed record line.
func museMessage(atUS int64, text string) string {
	return fmt.Sprintf(`{"schema_version":1,"recorded_at":%d,"payload_type":"runtime.session","payload":{"kind":"run","run_id":"r1","event":{"kind":"assistant_message_committed","phase":"commentary","text":%q}}}`, atUS, text)
}

// museNoise is any other durable-log record with long tool text in it.
func museNoise(atUS int64) string {
	return fmt.Sprintf(`{"schema_version":1,"recorded_at":%d,"payload_type":"runtime.session","payload":{"kind":"task","run_id":"r1","event":{"kind":"output","task_id":"t","text":%q}}}`, atUS, strings.Repeat("tool output ", 40))
}

func TestMuseLastReplyReturnsLastMessage(t *testing.T) {
	f := newMuseFixture(t, "iucia", []string{
		museNoise(1_790_999_000_000_000),
		museMessage(1_790_999_500_000_000, "first step done"),
		museNoise(1_790_999_900_000_000),
		museMessage(1_790_999_990_000_000, "the final answer"),
	})
	r, err := f.reader().LastReply(context.Background(), museAgent("/home/u/Devel/ccvass/iucia"))
	if err != nil || r.Text != "the final answer" || r.Source != "muse session log" {
		t.Fatalf("LastReply = %+v, %v", r, err)
	}
	if want := time.UnixMicro(1_790_999_990_000_000); !r.Written.Equal(want) {
		t.Fatalf("Written = %v, want %v", r.Written, want)
	}
}

func TestMuseLastReplyNoReply(t *testing.T) {
	base := func() *museFixture {
		return newMuseFixture(t, "iucia", []string{museMessage(1_790_999_990_000_000, "answer")})
	}
	cases := map[string]struct {
		agent domain.Agent
		mut   func(*museFixture)
	}{
		"unsupported kind":             {agent: domain.Agent{Key: domain.Key{PaneID: "p1"}, Kind: "claude", Cwd: "/u/iucia"}},
		"no working directory":         {agent: museAgent("")},
		"no session for the directory": {agent: museAgent("/u/other"), mut: func(f *museFixture) {}},
		"no log lines":                 {agent: museAgent("/u/iucia"), mut: func(f *museFixture) {}},
		"no assistant message":         {agent: museAgent("/u/iucia"), mut: func(f *museFixture) {}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := base()
			if tc.mut != nil {
				tc.mut(f)
			}
			switch name {
			case "no log lines":
				_ = os.RemoveAll(filepath.Join(f.home, ".local", "share", "muse", "sessions"))
			case "no assistant message":
				mdir := filepath.Join(f.home, ".local", "share", "muse", "sessions", "2026", "10", "06", museSessionID)
				if err := os.WriteFile(filepath.Join(mdir, "session.jsonl"), []byte(museNoise(1)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.reader().LastReply(context.Background(), tc.agent)
			if !errors.Is(err, domain.ErrNoReply) {
				t.Fatalf("err = %v, want ErrNoReply", err)
			}
		})
	}
}

func TestMuseLastReplyTruncatedTail(t *testing.T) {
	lines := []string{museMessage(1_790_999_000_000_000, "old answer")}
	for i := 0; i < 60; i++ {
		lines = append(lines, museNoise(1_790_999_500_000_000+int64(i)))
	}
	lines = append(lines, museMessage(1_790_999_990_000_000, "newest answer"))
	f := newMuseFixture(t, "iucia", lines)
	r := f.reader()
	r.maxScan = 3_000 // force the scan to start inside a line
	got, err := r.LastReply(context.Background(), museAgent("/u/iucia"))
	if err != nil || got.Text != "newest answer" {
		t.Fatalf("LastReply = %+v, %v", got, err)
	}
}
