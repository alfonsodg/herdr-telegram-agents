package transcript

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const (
	codexRevertA = "01a0de3a-dddd-7ddd-8ddd-dddddddddddd"
	codexRevertB = "01a0de3a-eeee-7eee-8eee-eeeeeeeeeeee"
)

func (f *codexFixture) sessions() string { return filepath.Join(f.home, ".codex", "sessions") }

// dayDirs are the shortcut's day directories for the test thread.
func (f *codexFixture) dayDirs() []string {
	var dirs []string
	clock := f.clock
	if clock.IsZero() {
		clock = codexCreated
	}
	days, _ := codexDayDirs(codexTestID, clock)
	for _, d := range days {
		dirs = append(dirs, filepath.Join(f.sessions(), d))
	}
	return dirs
}

// codexLink makes link point at target, or skips the test where the platform
// or the user may not create symbolic links.
func codexLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

func (f *codexFixture) wantNoReply(what string) {
	f.t.Helper()
	reply, err := f.reader().LastReply(context.Background(), f.agent())
	if !errors.Is(err, domain.ErrNoReply) || reply.Text != "" {
		f.t.Fatalf("%s: reply = %q, err = %v; want ErrNoReply and no text", what, reply.Text, err)
	}
	noLeak(f.t, "error", err.Error())
	if strings.Contains(err.Error(), "OUTSIDE") || strings.Contains(err.Error(), "STALE") {
		f.t.Fatalf("%s: error %q carries rollout text", what, err)
	}
}

// TestCodexLinkedDirectoriesAreNotFollowed covers a link where the sessions
// tree has a directory: the day directory, one above it, or one the walk
// would visit. Each points outside the tree at a rollout with the right name
// and content, which must never be read.
func TestCodexLinkedDirectoriesAreNotFollowed(t *testing.T) {
	answer := codexTurn("turn-a", "m", "OUTSIDE ANSWER", 1)
	cases := map[string]func(f *codexFixture){
		"day directory": func(f *codexFixture) {
			outside := filepath.Join(f.home, "outside")
			f.writeIn(outside, codexTestID, answer...)
			codexLink(f.t, outside, f.dayDirs()[0])
		},
		"year directory": func(f *codexFixture) {
			rel, _ := filepath.Rel(f.sessions(), f.dayDirs()[0])
			parts := strings.Split(rel, string(filepath.Separator))
			outside := filepath.Join(f.home, "outside")
			f.writeIn(filepath.Join(outside, parts[1], parts[2]), codexTestID, answer...)
			codexLink(f.t, outside, filepath.Join(f.sessions(), parts[0]))
		},
		"directory only the walk visits": func(f *codexFixture) {
			outside := filepath.Join(f.home, "outside")
			f.writeIn(filepath.Join(outside, "01", "01"), codexTestID, answer...)
			codexLink(f.t, outside, filepath.Join(f.sessions(), "2000"))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCodexFixture(t)
			setup(f)
			f.wantNoReply(name)
		})
	}
}

// TestCodexLinkedSessionsDirectoryIsFollowed covers the one link that is the
// user's own choice: the sessions directory itself.
func TestCodexLinkedSessionsDirectoryIsFollowed(t *testing.T) {
	f := newCodexFixture(t)
	real := filepath.Join(f.home, "moved-sessions")
	rel, _ := filepath.Rel(f.sessions(), f.dayDirs()[0])
	f.writeIn(filepath.Join(real, rel), codexTestID, codexTurn("turn-a", "m", "MOVED ANSWER", 1)...)
	codexLink(t, real, f.sessions())
	reply, err := f.reader().LastReply(context.Background(), f.agent())
	if err != nil || reply.Text != "MOVED ANSWER" {
		t.Fatalf("reply = %q, err = %v", reply.Text, err)
	}
}

// TestCodexRolloutMustBelongToTheThread covers a file whose name carries the
// thread id but whose first record does not say it is that thread's rollout.
func TestCodexRolloutMustBelongToTheThread(t *testing.T) {
	turn := codexTurn("turn-a", "m", "OUTSIDE ANSWER", 1)
	long := func(n int) string {
		return codexLine("session_meta", map[string]any{"id": codexTestID, "base_instructions": strings.Repeat("x", n)})
	}
	refused := map[string][]string{
		"another thread's session_meta": append([]string{codexMeta(codexHelperID)}, turn...),
		"no session_meta":               turn,
		"session_meta not first":        append(append([]string{}, turn[:1]...), append([]string{codexMeta(codexTestID)}, turn[1:]...)...),
		"unreadable first record":       append([]string{"{not json"}, turn...),
		"session_meta without an id":    append([]string{codexLine("session_meta", map[string]any{"cwd": "/work"})}, turn...),
		"first record beyond the cap":   append([]string{long(codexMetaMax)}, turn...),
		"empty file":                    nil,
	}
	for name, lines := range refused {
		t.Run(name, func(t *testing.T) {
			f := newCodexFixture(t)
			f.writeNamed(f.rolloutDir(codexTestID), "rollout-2026-09-26T09-59-58-"+codexTestID+".jsonl", lines...)
			f.wantNoReply(name)
		})
	}
	accepted := map[string][]string{
		"upper-case id": append([]string{codexLine("session_meta", map[string]any{"id": strings.ToUpper(codexTestID)})}, turn...),
		"forked thread": append([]string{codexLine("session_meta", map[string]any{"id": codexTestID, "session_id": codexHelperID, "forked_from_id": codexHelperID})}, turn...),
		"large record":  append([]string{long(200_000)}, turn...),
	}
	for name, lines := range accepted {
		t.Run(name, func(t *testing.T) {
			f := newCodexFixture(t)
			f.writeNamed(f.rolloutDir(codexTestID), "rollout-2026-09-26T09-59-58-"+codexTestID+".jsonl", lines...)
			reply, err := f.reader().LastReply(context.Background(), f.agent())
			if err != nil || reply.Text != "OUTSIDE ANSWER" {
				t.Fatalf("reply = %q, err = %v; want the answer", reply.Text, err)
			}
		})
	}
}

// TestCodexOpenRefusesAnotherFile covers a path that names another file when
// it is opened than when the search saw it.
func TestCodexOpenRefusesAnotherFile(t *testing.T) {
	f := newCodexFixture(t)
	path := f.write(codexTestID, codexTurn("turn-a", "m", "SEEN", 1)...)
	root, err := os.OpenRoot(f.sessions())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rel, seen, err := findCodexRollout(context.Background(), root, codexTestID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	other := f.writeNamed(filepath.Dir(path), "other.tmp", append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "SWAPPED", 1)...)...)
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if fl, _, err := openCodexRollout(root, rel, seen, codexTestID); !errors.Is(err, domain.ErrNoReply) {
		if fl != nil {
			fl.Close()
		}
		t.Fatalf("err = %v, want ErrNoReply for a file that is not the one seen", err)
	}
}

// TestCodexOnlyRegularFilesMatch covers directory entries that are named like
// a rollout but are not one.
func TestCodexOnlyRegularFilesMatch(t *testing.T) {
	f := newCodexFixture(t)
	day := f.rolloutDir(codexTestID)
	if err := os.MkdirAll(filepath.Join(day, "rollout-2026-09-26T09-59-58-"+codexTestID+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "OUTSIDE ANSWER", 1)...)
	for _, name := range []string{
		"rollout-2026-09-26-" + codexTestID + ".jsonl",              // not Codex's time format
		"rollout-2026-09-26T09-59-58-" + codexHelperID + ".jsonl",   // another thread
		"rollout-2026-09-26T09-59-58-" + codexTestID + ".jsonl.zst", // compressed
		"x-2026-09-26T09-59-58-" + codexTestID + ".jsonl",           // not a rollout
	} {
		f.writeNamed(day, name, body...)
	}
	f.wantNoReply("entries that are not the rollout")
}

func TestCodexRolloutName(t *testing.T) {
	const stamp = "2026-09-26T09-59-58"
	cases := []struct {
		name, stamp, rid string
		ok               bool
	}{
		{"rollout-" + stamp + "-" + codexTestID + ".jsonl", stamp, codexTestID, true},
		{"rollout-" + stamp + "-" + codexTestID + "_" + codexRevertA + ".jsonl", stamp, codexRevertA, true},
		{"rollout-" + stamp + "-" + codexHelperID + ".jsonl", "", "", false},
		{"rollout-" + stamp + "-" + strings.ToUpper(codexTestID) + ".jsonl", "", "", false},
		{"rollout-" + stamp + "-" + codexTestID + "_zzz.jsonl", "", "", false},
		{"rollout-" + stamp + "-" + codexTestID + "_" + codexRevertA + "_" + codexRevertB + ".jsonl", "", "", false},
		{"rollout-" + stamp + "-" + codexTestID + ".jsonl.zst", "", "", false},
		{"rollout-" + stamp + "-" + codexTestID + ".json", "", "", false},
		{"rollout-" + stamp + "X" + codexTestID + ".jsonl", "", "", false},
		{"rollout-2026-09-26-" + codexTestID + ".jsonl", "", "", false},
		{"rollout-a" + stamp + "-" + codexTestID + ".jsonl", "", "", false},
		{"session-" + stamp + "-" + codexTestID + ".jsonl", "", "", false},
		{"rollout-.jsonl", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		stamp, rid, ok := codexRolloutName(c.name, codexTestID)
		if ok != c.ok || stamp != c.stamp || rid != c.rid {
			t.Errorf("codexRolloutName(%q) = %q, %q, %v; want %q, %q, %v", c.name, stamp, rid, ok, c.stamp, c.rid, c.ok)
		}
	}
}

// TestCodexRevertedThreadUsesItsNewestRollout covers a thread Codex reverted:
// it keeps its id and continues in a new file, and the old file, which still
// holds the reverted turns, stays behind.
func TestCodexRevertedThreadUsesItsNewestRollout(t *testing.T) {
	stale := append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "STALE ANSWER", 1)...)
	current := append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "CURRENT ANSWER", 1)...)
	plain := "rollout-2026-09-26T09-59-58-" + codexTestID + ".jsonl"
	cases := map[string]func(f *codexFixture){
		"same day": func(f *codexFixture) {
			day := f.dayDirs()[0]
			f.writeNamed(day, plain, stale...)
			f.writeNamed(day, "rollout-2026-09-26T10-30-00-"+codexTestID+"_"+codexRevertA+".jsonl", current...)
		},
		"several reverts": func(f *codexFixture) {
			day := f.dayDirs()[0]
			f.writeNamed(day, "rollout-2026-09-26T10-30-00-"+codexTestID+"_"+codexRevertA+".jsonl", stale...)
			f.writeNamed(day, plain, stale...)
			f.writeNamed(day, "rollout-2026-09-26T11-00-00-"+codexTestID+"_"+codexRevertB+".jsonl", current...)
		},
		"same second": func(f *codexFixture) {
			day := f.dayDirs()[0]
			f.writeNamed(day, "rollout-2026-09-26T10-30-00-"+codexTestID+"_"+codexRevertA+".jsonl", stale...)
			f.writeNamed(day, "rollout-2026-09-26T10-30-00-"+codexTestID+"_"+codexRevertB+".jsonl", current...)
		},
		"days later": func(f *codexFixture) {
			created := time.UnixMilli(1790434781866)
			f.clock = created.AddDate(0, 0, 9)
			later := created.UTC().AddDate(0, 0, 6)
			f.writeNamed(f.dayDirs()[0], plain, stale...)
			f.writeNamed(filepath.Join(f.sessions(), later.Format("2006"), later.Format("01"), later.Format("02")),
				"rollout-"+later.Format("2006-01-02")+"T10-00-00-"+codexTestID+"_"+codexRevertA+".jsonl", current...)
		},
		"next day": func(f *codexFixture) {
			f.writeNamed(f.dayDirs()[0], plain, stale...)
			f.writeNamed(f.dayDirs()[2], "rollout-2026-09-27T00-10-00-"+codexTestID+"_"+codexRevertA+".jsonl", current...)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCodexFixture(t)
			setup(f)
			reply, err := f.reader().LastReply(context.Background(), f.agent())
			if err != nil || reply.Text != "CURRENT ANSWER" {
				t.Fatalf("reply = %q, err = %v; want the newest rollout's answer", reply.Text, err)
			}
		})
	}
}

// TestCodexDayDirectoryIsBounded covers a day directory with more entries
// than the search budget: the shortcut counts against it like the walk does.
func TestCodexDayDirectoryIsBounded(t *testing.T) {
	f := newCodexFixture(t)
	day := f.rolloutDir(codexTestID)
	for i := 0; i < 8; i++ {
		f.writeNamed(day, fmt.Sprintf("note-%d.txt", i), "x")
	}
	f.write(codexTestID, codexTurn("turn-a", "m", "OUTSIDE ANSWER", 1)...)
	old := codexWalkLimit
	codexWalkLimit = 5
	defer func() { codexWalkLimit = old }()
	_, err := f.reader().LastReply(context.Background(), f.agent())
	if !errors.Is(err, domain.ErrNoReply) || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("err = %v, want the search limit", err)
	}
}

// countdownCtx reports cancellation from its n+1th look on.
type countdownCtx struct {
	context.Context
	n int
}

func (c *countdownCtx) Err() error {
	if c.n--; c.n < 0 {
		return context.Canceled
	}
	return nil
}

// TestCodexDirectoriesAreReadInBatches covers cancellation in a directory
// with far more entries than one batch: the search stops after the batch it
// is in and does not load the rest.
func TestCodexDirectoriesAreReadInBatches(t *testing.T) {
	f := newCodexFixture(t)
	day := f.rolloutDir(codexTestID)
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3*codexReadBatch+10; i++ {
		if err := os.WriteFile(filepath.Join(day, fmt.Sprintf("note-%04d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(f.sessions())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rel, _ := filepath.Rel(f.sessions(), day)
	s := &codexSearch{ctx: &countdownCtx{Context: context.Background(), n: 1}, root: root, id: codexTestID, left: 100000}
	if err := s.scan(rel, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if read := 100000 - s.left; read != codexReadBatch {
		t.Fatalf("read %d entries before stopping, want one batch of %d", read, codexReadBatch)
	}
}

// codexCreated is the test thread's creation time, from its UUIDv7.
var codexCreated = time.UnixMilli(1790434781866)

// writeReplacement puts a reverted thread's newer rollout, answering
// CURRENT-ANSWER, in the UTC day directory days after the thread's creation,
// and returns that directory.
func (f *codexFixture) writeReplacement(days int) string {
	f.t.Helper()
	later := codexCreated.AddDate(0, 0, days).UTC()
	dir := filepath.Join(f.sessions(), later.Format("2006"), later.Format("01"), later.Format("02"))
	f.writeNamed(dir, "rollout-"+later.Format("2006-01-02T15-04-05")+"-"+codexTestID+"_"+codexRevertA+".jsonl",
		append([]string{codexMeta(codexTestID)}, codexTurn("current", "m", "CURRENT-ANSWER", 1)...)...)
	return dir
}

// TestSecurityRevertBeyondYearMustNotReturnRemovedAnswer covers a revert
// later than the day directories reach: the search must still find the newer
// rollout, or fall back, and never answer from the original file.
func TestSecurityRevertBeyondYearMustNotReturnRemovedAnswer(t *testing.T) {
	f := newCodexFixture(t)
	f.clock = codexCreated.AddDate(0, 0, 400)
	f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
	f.writeReplacement(399)
	r, err := f.reader().LastReply(context.Background(), f.agent())
	if r.Text == "REMOVED-ANSWER" || (err != nil && !errors.Is(err, domain.ErrNoReply)) {
		t.Fatalf("incomplete search must not return a removed answer: reply=%q err=%v", r.Text, err)
	}
}

// TestCodexDayLimitBoundary covers threads on both sides of codexLaterDays,
// with and without a newer file on the thread's last listed day or later.
func TestCodexDayLimitBoundary(t *testing.T) {
	for _, age := range []int{codexLaterDays - 1, codexLaterDays, codexLaterDays + 1, codexLaterDays + 2, 3 * codexLaterDays} {
		t.Run(fmt.Sprintf("%d days, original only", age), func(t *testing.T) {
			f := newCodexFixture(t)
			f.clock = codexCreated.AddDate(0, 0, age)
			f.write(codexTestID, codexTurn("only", "m", "ONLY-ANSWER", 1)...)
			r, err := f.reader().LastReply(context.Background(), f.agent())
			if err != nil || r.Text != "ONLY-ANSWER" {
				t.Fatalf("reply=%q err=%v; want the only rollout's answer", r.Text, err)
			}
		})
		t.Run(fmt.Sprintf("%d days, replaced that day", age), func(t *testing.T) {
			f := newCodexFixture(t)
			f.clock = codexCreated.AddDate(0, 0, age)
			f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
			f.writeReplacement(age)
			r, err := f.reader().LastReply(context.Background(), f.agent())
			if err != nil || r.Text != "CURRENT-ANSWER" {
				t.Fatalf("reply=%q err=%v; want the replacement's answer", r.Text, err)
			}
		})
	}
}

// TestCodexOldThreadWalkIsBounded covers a thread older than the day
// directories reach: the walk that replaces them runs on the same budget,
// and running out of it falls back instead of answering from the file found.
func TestCodexOldThreadWalkIsBounded(t *testing.T) {
	f := newCodexFixture(t)
	f.clock = codexCreated.AddDate(0, 0, 400)
	f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
	later := f.writeReplacement(399)
	for i := 0; i < 8; i++ {
		f.writeNamed(later, fmt.Sprintf("note-%d.txt", i), "x")
	}
	old := codexWalkLimit
	codexWalkLimit = 10
	defer func() { codexWalkLimit = old }()
	f.wantNoReply("walk over budget")
}

// TestCodexMissingDayDirectoriesStillComplete covers the usual tree: most of
// the listed days have no directory, which is no reason to fall back.
func TestCodexMissingDayDirectoriesStillComplete(t *testing.T) {
	f := newCodexFixture(t)
	f.clock = codexCreated.AddDate(0, 0, 30)
	f.write(codexTestID, codexTurn("only", "m", "ONLY-ANSWER", 1)...)
	if err := os.MkdirAll(filepath.Join(f.sessions(), "2026", "10"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := f.reader().LastReply(context.Background(), f.agent())
	if err != nil || r.Text != "ONLY-ANSWER" {
		t.Fatalf("reply=%q err=%v; want the answer", r.Text, err)
	}
}

func TestSecurityUnreadableNewerDayMustNotReturnRemovedAnswer(t *testing.T) {
	f := newCodexFixture(t)
	f.clock = codexCreated.AddDate(0, 0, 4)
	f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
	dir := f.writeReplacement(3)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if entries, err := os.ReadDir(dir); err == nil {
		t.Skipf("directory permissions not enforced: %d entries", len(entries))
	}
	r, err := f.reader().LastReply(context.Background(), f.agent())
	if !errors.Is(err, domain.ErrNoReply) || r.Text != "" {
		t.Fatalf("unreadable newer directory must cause fallback: reply=%q err=%v", r.Text, err)
	}
	noLeak(t, "error", err.Error())
}

// failingDir lists its directory's entries, then fails instead of ending.
type failingDir struct {
	codexDir
	done bool
	err  error
}

func (d *failingDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.done {
		return nil, d.err
	}
	d.done = true
	entries, err := d.codexDir.ReadDir(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return entries, err
	}
	return entries, nil
}

// TestCodexDirectoryFailuresMakeTheSearchIncomplete covers a directory that
// cannot be opened or whose listing fails part way, after an older rollout
// was already found: the search falls back rather than answer with it. Only
// a directory that does not exist is an empty one.
func TestCodexDirectoryFailuresMakeTheSearchIncomplete(t *testing.T) {
	ioErr := errors.New("input/output error on " + codexTestID)
	cases := map[string]func(d codexDir, err error) (codexDir, error){
		"open denied":    func(codexDir, error) (codexDir, error) { return nil, fs.ErrPermission },
		"open i/o error": func(codexDir, error) (codexDir, error) { return nil, ioErr },
		"listing fails":  func(d codexDir, err error) (codexDir, error) { return &failingDir{codexDir: d, err: ioErr}, err },
		"listing denied": func(d codexDir, err error) (codexDir, error) {
			return &failingDir{codexDir: d, err: fs.ErrPermission}, err
		},
		"listing says absent": func(d codexDir, err error) (codexDir, error) {
			return &failingDir{codexDir: d, err: fs.ErrNotExist}, err
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCodexFixture(t)
			f.clock = codexCreated.AddDate(0, 0, 4)
			f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
			later, _ := filepath.Rel(f.sessions(), f.writeReplacement(3))
			old := codexOpenDir
			codexOpenDir = func(root *os.Root, dir string) (codexDir, error) {
				d, err := old(root, dir)
				if dir != later {
					return d, err
				}
				return fail(d, err)
			}
			defer func() { codexOpenDir = old }()
			f.wantNoReply(name)
			if !strings.Contains(fmt.Sprint(f.reader().LastReply(context.Background(), f.agent())), "could not be read") {
				t.Fatal("want the incomplete-search reason")
			}
		})
	}
	t.Run("absent directory", func(t *testing.T) {
		f := newCodexFixture(t)
		f.clock = codexCreated.AddDate(0, 0, 4)
		f.write(codexTestID, codexTurn("only", "m", "ONLY-ANSWER", 1)...)
		old := codexOpenDir
		codexOpenDir = func(root *os.Root, dir string) (codexDir, error) {
			if strings.HasSuffix(dir, "28") {
				return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
			}
			return old(root, dir)
		}
		defer func() { codexOpenDir = old }()
		r, err := f.reader().LastReply(context.Background(), f.agent())
		if err != nil || r.Text != "ONLY-ANSWER" {
			t.Fatalf("reply=%q err=%v; want the answer", r.Text, err)
		}
	})
}

// TestCodexLinksThatMayHideARolloutFallBack covers links the search cannot
// look through without leaving the tree: one named like the thread's
// rollout, and, in the walk, one where a directory could be. Each may hide
// the newest rollout, so the older file found is not answered with.
func TestCodexLinksThatMayHideARolloutFallBack(t *testing.T) {
	t.Run("link named like the rollout", func(t *testing.T) {
		f := newCodexFixture(t)
		f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
		target := f.writeIn(filepath.Join(f.home, "elsewhere"), codexTestID, codexTurn("t", "m", "OUTSIDE", 1)...)
		codexLink(t, target, filepath.Join(f.rolloutDir(codexTestID), "rollout-2026-09-26T10-30-00-"+codexTestID+"_"+codexRevertA+".jsonl"))
		f.wantNoReply("rollout-named link")
	})
	t.Run("linked directory in the walk", func(t *testing.T) {
		f := newCodexFixture(t)
		f.clock = codexCreated.AddDate(0, 0, 400)
		f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
		outside := filepath.Join(f.home, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		codexLink(t, outside, filepath.Join(f.sessions(), "2027"))
		f.wantNoReply("linked directory")
	})
}

// codexRevertV4 is a rollout id that is not a UUIDv7, so it carries no time.
const codexRevertV4 = "01a0de3a-ffff-4fff-8fff-ffffffffffff"

// TestCodexRolloutOrderIgnoresWallClock covers file name stamps that go
// backwards: the hour that repeats when daylight saving ends, or a move to a
// zone further west. The rollout id's UUIDv7 time, which is UTC, decides.
func TestCodexRolloutOrderIgnoresWallClock(t *testing.T) {
	f := newCodexFixture(t)
	day := f.dayDirs()[0]
	f.writeNamed(day, "rollout-2026-09-26T10-30-00-"+codexTestID+".jsonl",
		append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "STALE ANSWER", 1)...)...)
	f.writeNamed(day, "rollout-2026-09-26T10-10-00-"+codexTestID+"_"+codexRevertA+".jsonl",
		append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "CURRENT ANSWER", 1)...)...)
	r, err := f.reader().LastReply(context.Background(), f.agent())
	if err != nil || r.Text != "CURRENT ANSWER" {
		t.Fatalf("reply = %q, err = %v; want the newer rollout by its id's time", r.Text, err)
	}
}

// TestCodexRolloutIDWithoutTime covers a rollout id that is not a UUIDv7:
// alone it is the thread's only file and is read; next to another file of
// the thread it cannot be ordered, so the screen is posted.
func TestCodexRolloutIDWithoutTime(t *testing.T) {
	odd := "rollout-2026-09-26T11-00-00-" + codexTestID + "_" + codexRevertV4 + ".jsonl"
	body := append([]string{codexMeta(codexTestID)}, codexTurn("turn-a", "m", "ODD ANSWER", 1)...)
	t.Run("alone", func(t *testing.T) {
		f := newCodexFixture(t)
		f.writeNamed(f.dayDirs()[0], odd, body...)
		r, err := f.reader().LastReply(context.Background(), f.agent())
		if err != nil || r.Text != "ODD ANSWER" {
			t.Fatalf("reply = %q, err = %v", r.Text, err)
		}
	})
	t.Run("next to the original", func(t *testing.T) {
		f := newCodexFixture(t)
		f.write(codexTestID, codexTurn("turn-a", "m", "STALE ANSWER", 1)...)
		f.writeNamed(f.dayDirs()[0], odd, body...)
		f.wantNoReply("unordered rollout ids")
	})
}

// TestCodexClockBehindCreation covers a clock set back after the thread was
// created: a revert then lands in a day before the creation day, which only
// the whole tree walk reaches.
func TestCodexClockBehindCreation(t *testing.T) {
	f := newCodexFixture(t)
	f.clock = codexCreated.AddDate(0, 0, -3)
	f.write(codexTestID, codexTurn("removed", "m", "REMOVED-ANSWER", 1)...)
	f.writeReplacement(-3)
	r, err := f.reader().LastReply(context.Background(), f.agent())
	if err != nil || r.Text != "CURRENT-ANSWER" {
		t.Fatalf("reply = %q, err = %v; want the replacement found by the walk", r.Text, err)
	}
	if dirs, complete := codexDayDirs(codexTestID, f.clock); complete || dirs != nil {
		t.Fatalf("a clock behind creation must not be a complete search: %v", dirs)
	}
}
