package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const (
	// gitTimeout bounds one git run.
	gitTimeout = 10 * time.Second
	// gitMaxOutput is the most stdout a run may produce; the rest is cut
	// and the result marked truncated.
	gitMaxOutput = 5 << 20
	// gitMaxProbe is the most stdout the filter probe may produce. It is
	// its own bound: the user's config (git-lfs) lists drivers too, and that
	// listing must not eat the cap of the output /git returns. A probe past
	// it fails the run, since an unread driver could not be switched off.
	gitMaxProbe = 1 << 20
	// gitWaitDelay bounds how long a run waits for its pipes once the
	// timeout killed git: a child that inherited stdout (a hook, a helper)
	// would otherwise keep Wait blocked until it exits on its own.
	gitWaitDelay = time.Second
)

// gitSafeConfig switches off every program the repository's own config can
// name for the read-only subcommands /git runs: the fsmonitor hook, hooks,
// the pager, the signature check. /git runs in whatever directory the agent
// works in, which may be a repository nobody on this machine wrote. Filter
// drivers have no global switch; gitFilterConfig adds them per name.
var gitSafeConfig = []string{
	"-c", "color.ui=never",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.pager=cat",
	"-c", "log.showSignature=false",
}

// gitDiffing are the subcommands that render diffs and therefore accept
// --no-ext-diff and --no-textconv (diff.external and textconv drivers).
var gitDiffing = map[string]bool{"diff": true, "log": true, "show": true}

// gitWorktree are the subcommands that compare the working tree. They get
// --ignore-submodules=dirty: checking a submodule for local changes starts
// a git status inside it, which reads the submodule's own config (and its
// filter drivers) that the filter probe of the outer repository never sees.
// A moved submodule commit is still reported.
var gitWorktree = map[string]bool{"diff": true, "status": true}

// gitFilterProbe lists the filter driver keys every config scope sets, one
// "<scope>\t<key>" line each (--show-scope needs git 2.26). It only reads
// config, so it runs no program the repository could name.
var gitFilterProbe = []string{"config", "--show-scope", "--name-only", "--get-regexp", `^filter\.`}

// gitUserScopes are the config scopes the machine's user wrote; their filter
// drivers (git-lfs, typically) stay on. Every other scope, the repository's
// .git/config and anything it includes in the first place, is neutralised.
var gitUserScopes = map[string]bool{"system": true, "global": true, "command": true}

// errGitFilterName refuses a filter name that cannot be passed through -c.
var errGitFilterName = errors.New("git filter name not representable")

// gitArgv builds the full argv for one run; filters are the repository's
// filter driver names (gitFilterNames).
func gitArgv(args []string, filters []string) []string {
	argv := append([]string(nil), gitSafeConfig...)
	argv = append(argv, gitFilterConfig(filters)...)
	if len(args) == 0 {
		return argv
	}
	argv = append(argv, args[0])
	if gitDiffing[args[0]] {
		argv = append(argv, "--no-ext-diff", "--no-textconv")
	}
	if gitWorktree[args[0]] {
		argv = append(argv, "--ignore-submodules=dirty")
	}
	return append(argv, args[1:]...)
}

// gitFilterConfig empties the clean, smudge and process commands of each
// filter driver, so status and diff hash the working tree as it is instead
// of running the driver; required=false keeps a required driver from
// failing the run instead.
func gitFilterConfig(filters []string) []string {
	var argv []string
	for _, name := range filters {
		for _, key := range []string{"clean", "smudge", "process"} {
			argv = append(argv, "-c", "filter."+name+"."+key+"=")
		}
		argv = append(argv, "-c", "filter."+name+".required=false")
	}
	return argv
}

// gitFilterNames parses the gitFilterProbe output into the distinct driver
// names outside gitUserScopes. A name with "=" cannot be overridden through
// -c (git splits key and value at the first "="), so it fails the run.
func gitFilterNames(out string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		scope, key, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || gitUserScopes[scope] {
			continue
		}
		rest, ok := strings.CutPrefix(key, "filter.")
		dot := strings.LastIndexByte(rest, '.')
		if !ok || dot <= 0 {
			continue
		}
		name := rest[:dot]
		if strings.ContainsAny(name, "=\x00") {
			return nil, fmt.Errorf("%w: %q", errGitFilterName, name)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// errOutputCapped stops the stdout copy once gitMaxOutput is reached.
var errOutputCapped = errors.New("git output capped")

// GitRunner implements domain.GitRunner over the git binary on PATH: a
// fixed argv from the domain runs in the agent's directory with colour
// and pager off, a timeout and an output cap.
type GitRunner struct {
	bin      string
	timeout  time.Duration
	maxBytes int
	log      *slog.Logger
}

var _ domain.GitRunner = (*GitRunner)(nil)

// NewGitRunner returns a runner for "git" on PATH.
func NewGitRunner(log *slog.Logger) *GitRunner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &GitRunner{bin: "git", timeout: gitTimeout, maxBytes: gitMaxOutput, log: log}
}

// Run implements domain.GitRunner.
func (r *GitRunner) Run(ctx context.Context, dir string, args []string) (domain.GitResult, error) {
	bin, err := exec.LookPath(r.bin)
	if err != nil {
		r.log.Warn("git binary not found", slog.String("bin", r.bin), slog.String("err", err.Error()))
		return domain.GitResult{}, domain.ErrGitMissing
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	filters, err := r.filters(ctx, bin, dir)
	if err != nil {
		return domain.GitResult{}, err
	}
	argv := gitArgv(args, filters)
	r.log.Debug("[FIX] git safe argv", slog.String("argv", strings.Join(argv, " ")))
	cmd := r.command(ctx, bin, dir, argv)
	stdout := &limitedWriter{max: r.maxBytes}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	start := time.Now()
	runErr := cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	r.log.Debug("git run", slog.String("dir", dir), slog.String("args", strings.Join(args, " ")),
		slog.Int64("dur_ms", time.Since(start).Milliseconds()), slog.Int("bytes", stdout.buf.Len()),
		slog.Bool("truncated", stdout.capped), slog.Int("exit", exit), slog.Any("err", runErr))
	res := domain.GitResult{Output: strings.TrimRight(stdout.buf.String(), " \t\r\n"), Truncated: stdout.capped}
	switch {
	case runErr == nil:
		return res, nil
	case stdout.capped && (errors.Is(runErr, errOutputCapped) || exit == 0 || isBrokenPipe(runErr, stderr.String())):
		// The process was cut by the cap, not by its own failure.
		return res, nil
	case ctx.Err() != nil:
		r.log.Warn("git timed out", slog.String("dir", dir), slog.String("args", strings.Join(args, " ")))
		return domain.GitResult{}, context.DeadlineExceeded
	}
	msg := strings.TrimSpace(stderr.String())
	r.log.Warn("git failed", slog.String("dir", dir), slog.String("args", strings.Join(args, " ")), slog.Int("exit", exit), slog.String("stderr", firstLine(msg)))
	if strings.Contains(msg, "not a git repository") {
		return domain.GitResult{}, fmt.Errorf("%w: %s", domain.ErrNotRepository, dir)
	}
	if msg == "" {
		return domain.GitResult{}, fmt.Errorf("git %s: %w", args[0], runErr)
	}
	return domain.GitResult{}, fmt.Errorf("git %s: %w: %s", args[0], runErr, firstLine(msg))
}

// command prepares one git child in dir with the pager, prompts and
// GIT_EXTERNAL_DIFF off.
func (r *GitRunner) command(ctx context.Context, bin, dir string, argv []string) *exec.Cmd {
	cmd := command(ctx, bin, argv...)
	cmd.Dir = dir
	cmd.WaitDelay = gitWaitDelay
	cmd.Env = append(withoutEnv(os.Environ(), "GIT_EXTERNAL_DIFF"), "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return cmd
}

// filters returns the filter drivers the repository in dir configures. Exit
// status 1 means none; any other failure fails the run, since running
// status or diff without the overrides could start a driver.
func (r *GitRunner) filters(ctx context.Context, bin, dir string) ([]string, error) {
	cmd := r.command(ctx, bin, dir, append(append([]string(nil), gitSafeConfig...), gitFilterProbe...))
	stdout := &limitedWriter{max: gitMaxProbe}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 && stdout.buf.Len() == 0:
		return nil, nil
	case ctx.Err() != nil:
		r.log.Warn("git filter probe timed out", slog.String("dir", dir))
		return nil, context.DeadlineExceeded
	default:
		msg := firstLine(stderr.String())
		r.log.Warn("[FIX] git filter probe failed; run refused", slog.String("dir", dir), slog.String("stderr", msg), slog.Any("err", runErr))
		return nil, fmt.Errorf("git config: %w: %s", runErr, msg)
	}
	names, err := gitFilterNames(stdout.buf.String())
	if err != nil {
		r.log.Warn("[FIX] git filter probe found an unusable name; run refused", slog.String("dir", dir), slog.String("err", err.Error()))
		return nil, err
	}
	if len(names) > 0 {
		r.log.Debug("[FIX] repository filter drivers neutralised", slog.String("dir", dir), slog.String("filters", strings.Join(names, " ")))
	}
	return names, nil
}

// withoutEnv drops the named variables from env.
func withoutEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			drop = drop || name == n
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// isBrokenPipe reports whether git died because its stdout was closed by
// the cap (signalled by SIGPIPE or an EPIPE line on stderr).
func isBrokenPipe(err error, stderr string) bool {
	return strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "signal: broken pipe") ||
		strings.Contains(stderr, "Broken pipe")
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// limitedWriter collects up to max bytes and refuses the rest, which stops
// exec's copy and lets the process die on a closed pipe.
type limitedWriter struct {
	buf    bytes.Buffer
	max    int
	capped bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if room <= 0 {
		w.capped = true
		return 0, errOutputCapped
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.capped = true
		return room, errOutputCapped
	}
	return w.buf.Write(p)
}
