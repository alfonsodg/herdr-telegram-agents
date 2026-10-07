package system

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// gitRepo builds a repository with one commit and a dirty tracked file;
// tests skip when git is not on PATH.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "first")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGitRunnerStatusDiffLog(t *testing.T) {
	dir := gitRepo(t)
	r := NewGitRunner(nil)
	ctx := context.Background()
	status, err := r.Run(ctx, dir, []string{"status", "--short", "--branch"})
	if err != nil || !strings.HasPrefix(status.Output, "## main") || !strings.Contains(status.Output, " M a.txt") || status.Truncated {
		t.Fatalf("status = %+v, %v", status, err)
	}
	diff, err := r.Run(ctx, dir, []string{"diff", "HEAD"})
	if err != nil || !strings.Contains(diff.Output, "+two") || strings.Contains(diff.Output, "\x1b[") {
		t.Fatalf("diff = %+v, %v", diff, err)
	}
	log, err := r.Run(ctx, dir, []string{"log", "--oneline", "--decorate", "-n", "10"})
	if err != nil || !strings.Contains(log.Output, "first") || strings.Count(log.Output, "\n") != 0 {
		t.Fatalf("log = %+v, %v", log, err)
	}
	staged, err := r.Run(ctx, dir, []string{"diff", "--cached"})
	if err != nil || staged.Output != "" {
		t.Fatalf("staged diff = %+v, %v", staged, err)
	}
}

func TestGitRunnerNotRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	r := NewGitRunner(nil)
	_, err := r.Run(context.Background(), dir, []string{"status", "--short", "--branch"})
	if !errors.Is(err, domain.ErrNotRepository) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitRunnerMissingBinary(t *testing.T) {
	r := NewGitRunner(nil)
	r.bin = "git-definitely-missing-binary"
	if _, err := r.Run(context.Background(), t.TempDir(), []string{"status"}); !errors.Is(err, domain.ErrGitMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitRunnerBadArgsCarryStderr(t *testing.T) {
	dir := gitRepo(t)
	r := NewGitRunner(nil)
	_, err := r.Run(context.Background(), dir, []string{"log", "--no-such-flag"})
	if err == nil || errors.Is(err, domain.ErrNotRepository) || !strings.Contains(err.Error(), "no-such-flag") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitRunnerTruncates(t *testing.T) {
	dir := gitRepo(t)
	big := strings.Repeat("a line that makes the diff long enough to pass the cap\n", 400)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewGitRunner(nil)
	r.maxBytes = 64
	res, err := r.Run(context.Background(), dir, []string{"diff", "HEAD"})
	if err != nil || !res.Truncated || len(res.Output) > 64 || res.Output == "" {
		t.Fatalf("truncated run = %+v, %v", res, err)
	}
}

// TestGitRunnerTruncatesWithUserFilters covers a user whose own config
// names filter drivers (git-lfs, as on CI runners): the filter probe lists
// them, and that listing must not count against the cap of the output /git
// returns.
func TestGitRunnerTruncatesWithUserFilters(t *testing.T) {
	dir := gitRepo(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	cfg := "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n"
	if err := os.WriteFile(global, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	big := strings.Repeat("a line that makes the diff long enough to pass the cap\n", 400)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewGitRunner(nil)
	r.maxBytes = 64
	res, err := r.Run(context.Background(), dir, []string{"diff", "HEAD"})
	if err != nil || !res.Truncated || len(res.Output) > 64 || res.Output == "" {
		t.Fatalf("truncated run = %+v, %v", res, err)
	}
}

func TestGitRunnerTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in is Unix only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow-git")
	// Two commands keep the shell from exec-ing sleep in its place, so the
	// child survives the kill and holds stdout, as a git helper would.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\necho done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewGitRunner(nil)
	r.bin = script
	r.timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := r.Run(context.Background(), dir, []string{"status"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout did not stop the process: %v", time.Since(start))
	}
}

// TestGitRunnerIgnoresRepositoryCommands: the repository's own config can
// name programs (fsmonitor hook, external diff, textconv driver). /git runs
// in whatever directory the agent works in, so none of them may execute.
func TestGitRunnerIgnoresRepositoryCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$0 $*\" >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt diff=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"core.fsmonitor", script}, {"diff.external", script}, {"diff.evil.textconv", script}} {
		cmd := exec.Command("git", "config", kv[0], kv[1])
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %v: %v\n%s", kv, err, out)
		}
	}
	r := NewGitRunner(nil)
	ctx := context.Background()
	for _, args := range [][]string{{"status", "--short", "--branch"}, {"diff", "HEAD"}, {"diff", "--cached"}, {"log", "--oneline", "--decorate", "-n", "10"}} {
		if _, err := r.Run(ctx, dir, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("repository command ran:\n%s", data)
	}
}

// TestGitRunnerIgnoresFilterDrivers: git status and git diff HEAD run the
// filter driver of a modified working-tree file when .gitattributes assigns
// one, and a filter driver is a program named by the repository's config.
// The long-running process driver takes precedence over clean and smudge,
// so each kind gets its own repository. b.txt keeps its size, so status
// has to hash it (through the filter) to tell it changed.
func TestGitRunnerIgnoresFilterDrivers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	for _, keys := range [][]string{{"clean", "smudge"}, {"process"}} {
		t.Run(strings.Join(keys, "+"), func(t *testing.T) {
			dir := gitRepo(t)
			marker := filepath.Join(t.TempDir(), "ran")
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "b.txt")
			git("commit", "-q", "-m", "second")
			if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				git("config", "filter.evil."+key, "sh -c 'echo "+key+" >> "+marker+"; cat'")
			}
			r := NewGitRunner(nil)
			ctx := context.Background()
			for _, args := range [][]string{{"status", "--short", "--branch"}, {"diff", "HEAD"}, {"diff", "--cached"}, {"log", "--oneline", "--decorate", "-n", "10"}} {
				res, err := r.Run(ctx, dir, args)
				if data, rerr := os.ReadFile(marker); rerr == nil {
					t.Fatalf("%v ran a filter driver:\n%s", args, data)
				}
				if err != nil {
					t.Fatalf("%v: %v", args, err)
				}
				if args[0] == "status" && !strings.Contains(res.Output, " M b.txt") {
					t.Fatalf("status without filters = %q", res.Output)
				}
				if args[0] == "diff" && args[1] == "HEAD" && !strings.Contains(res.Output, "+two") {
					t.Fatalf("diff without filters = %q", res.Output)
				}
			}
		})
	}
}

// TestGitRunnerIgnoresSubmoduleFilters: status and diff recurse into a
// submodule with a git status of their own, and that run reads the
// submodule's config, whose filter drivers the outer probe never sees.
func TestGitRunnerIgnoresSubmoduleFilters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	inner := gitRepo(t)
	dir := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(dir, "submodule", "add", "-q", inner, "sub")
	git(dir, "commit", "-q", "-m", "submodule")
	sub := filepath.Join(dir, "sub")
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(sub, "config", "filter.evil.clean", "sh -c 'echo clean >> "+marker+"; cat'")
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewGitRunner(nil)
	ctx := context.Background()
	for _, args := range [][]string{{"status", "--short", "--branch"}, {"diff", "HEAD"}} {
		_, err := r.Run(ctx, dir, args)
		if data, rerr := os.ReadFile(marker); rerr == nil {
			t.Fatalf("%v ran a submodule filter driver:\n%s", args, data)
		}
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// TestGitRunnerDoesNotEnterSubmoduleDiffs: a repository whose own config
// sets diff.submodule=diff would make git diff run a child git diff inside
// the submodule, which reads the submodule's config (diff.external) and
// gets none of the outer run's guards.
func TestGitRunnerDoesNotEnterSubmoduleDiffs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	inner := gitRepo(t)
	dir := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(dir, "submodule", "add", "-q", inner, "sub")
	git(dir, "commit", "-q", "-m", "submodule")
	sub := filepath.Join(dir, "sub")
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(sub, "commit", "-q", "-am", "moved")
	git(dir, "config", "diff.submodule", "diff")
	script := filepath.Join(t.TempDir(), "ext.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ext >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(sub, "config", "diff.external", script)
	r := NewGitRunner(nil)
	ctx := context.Background()
	for _, args := range [][]string{{"diff", "HEAD"}, {"diff"}} {
		out, err := r.Run(ctx, dir, args)
		if data, rerr := os.ReadFile(marker); rerr == nil {
			t.Fatalf("%v ran the submodule's diff.external:\n%s", args, data)
		}
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.Output, "Subproject commit") && !strings.Contains(out.Output, "Submodule sub") {
			t.Fatalf("%v lost the submodule change:\n%s", args, out.Output)
		}
	}
	// /git diff staged: the same guard holds for a staged submodule move.
	git(dir, "add", "sub")
	spec, ok := domain.ParseGit([]string{"diff", "staged"})
	if !ok {
		t.Fatal("diff staged not parsed")
	}
	out, err := r.Run(ctx, dir, spec.Args)
	if data, rerr := os.ReadFile(marker); rerr == nil {
		t.Fatalf("%v ran the submodule's diff.external:\n%s", spec.Args, data)
	}
	if err != nil || !strings.Contains(out.Output, "Subproject commit") {
		t.Fatalf("%v: err %v, output:\n%s", spec.Args, err, out.Output)
	}
}

// TestGitFilterNames: drivers from the user's own scopes (git-lfs) stay on,
// repository scopes are neutralised, and a name -c cannot carry refuses.
func TestGitFilterNames(t *testing.T) {
	out := "global\tfilter.lfs.process\nsystem\tfilter.sys.clean\nlocal\tfilter.evil.clean\nlocal\tfilter.evil.smudge\n" +
		"worktree\tfilter.a.b.process\nlocal\tfilter.bare\ncommand\tfilter.cmd.clean\n"
	names, err := gitFilterNames(out)
	if err != nil || strings.Join(names, ",") != "evil,a.b" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if _, err := gitFilterNames("local\tfilter.x=y.clean\n"); !errors.Is(err, errGitFilterName) {
		t.Fatalf("err = %v", err)
	}
	argv := strings.Join(gitArgv([]string{"status", "--short"}, names), " ")
	if !strings.Contains(argv, "-c filter.a.b.process= -c filter.a.b.required=false status --ignore-submodules=dirty --short") {
		t.Fatalf("argv = %v", argv)
	}
}
