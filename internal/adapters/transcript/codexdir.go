package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// CodexDirectoryReader wraps the exact-session CodexReader with a directory
// fallback: Herdr's reported thread can go stale when the codex
// conversation switches inside the pane (an in-process /new, a new
// conversation in the same pane), while the real answer keeps landing in
// the newest rollout of the pane's working directory. The fresher of the
// two answers wins; a fresh exact-session answer is kept as is.
type CodexDirectoryReader struct {
	exact    *CodexReader
	home     func() (string, error)
	log      *slog.Logger
	maxFiles int
	days     int
}

// NewCodexDirectoryReader wraps exact over the current user's home.
func NewCodexDirectoryReader(exact *CodexReader, log *slog.Logger) *CodexDirectoryReader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &CodexDirectoryReader{exact: exact, home: os.UserHomeDir, log: log, maxFiles: 10, days: 5}
}

// LastReply implements domain.ReplySource.
func (r *CodexDirectoryReader) LastReply(ctx context.Context, agent domain.Agent) (domain.Reply, error) {
	if agent.Kind != kindCodex {
		return domain.Reply{}, fmt.Errorf("%w: unsupported agent %q", domain.ErrUnsupportedAgent, agent.Kind)
	}
	exact, exactErr := r.exact.LastReply(ctx, agent)
	if exactErr != nil && !errors.Is(exactErr, domain.ErrNoReply) {
		return domain.Reply{}, exactErr
	}
	if strings.TrimSpace(agent.Cwd) == "" {
		if exactErr != nil {
			return domain.Reply{}, exactErr
		}
		return exact, nil
	}
	fresh, err := r.newestForDir(ctx, agent.Cwd)
	if err != nil {
		if exactErr != nil {
			return domain.Reply{}, exactErr
		}
		return exact, nil
	}
	if exactErr != nil || fresh.Written.After(exact.Written) {
		r.log.Debug("codex directory fallback", slog.String("pane", agent.PaneID),
			slog.Int("chars", len(fresh.Text)), slog.Int64("age_ms", fresh.Age.Milliseconds()))
		return fresh, nil
	}
	return exact, nil
}

// newestForDir returns the answer of the newest rollout (by file time) of
// the last few day directories whose session_meta names this directory.
func (r *CodexDirectoryReader) newestForDir(ctx context.Context, cwd string) (domain.Reply, error) {
	home, err := r.home()
	if err != nil {
		return domain.Reply{}, fmt.Errorf("%w: no home directory", domain.ErrNoReply)
	}
	root, err := os.OpenRoot(filepath.Join(home, codexHomeDir, "sessions"))
	if err != nil {
		return domain.Reply{}, fmt.Errorf("%w: no sessions directory", domain.ErrNoReply)
	}
	defer root.Close()
	type cand struct {
		rel string
		mod time.Time
	}
	var cands []cand
	now := r.exact.now()
	for d := 0; d < r.days; d++ {
		day := now.AddDate(0, 0, -d).Format("2006/01/02")
		entries, err := fs.ReadDir(root.FS(), day)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			cands = append(cands, cand{filepath.Join(day, e.Name()), info.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	if len(cands) > r.maxFiles {
		cands = cands[:r.maxFiles]
	}
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return domain.Reply{}, err
		}
		f, err := root.Open(c.rel)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}
		reply, err := r.replyFrom(f, info, cwd)
		_ = f.Close()
		if err == nil {
			return reply, nil
		}
	}
	return domain.Reply{}, fmt.Errorf("%w: no codex rollout for the working directory", domain.ErrNoReply)
}

// replyFrom parses one candidate file: its session_meta must name the same
// directory and its last completed turn provides the answer.
func (r *CodexDirectoryReader) replyFrom(f *os.File, info os.FileInfo, cwd string) (domain.Reply, error) {
	got, err := codexMetaCwd(f)
	if err != nil || got != cwd {
		return domain.Reply{}, fmt.Errorf("%w: rollout is for another directory", domain.ErrNoReply)
	}
	text, meta, stats, err := codexLastReplyFrom(f, info.Size(), r.exact.maxScan)
	if err != nil {
		return domain.Reply{}, err
	}
	written := meta.Ended
	if written.IsZero() {
		written = info.ModTime()
	}
	age := r.exact.now().Sub(written)
	r.log.Debug("codex directory reply found", slog.String("file", filepath.Base(info.Name())),
		slog.Int("lines", stats.lines), slog.Int("chars", len(text)), slog.Int64("age_ms", age.Milliseconds()))
	return domain.Reply{Text: text, Source: "codex rollout (directory)", Age: age, Written: written, Meta: meta}, nil
}

// codexMetaCwd reads the first rollout record and returns the working
// directory its session_meta names.
func codexMetaCwd(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !sc.Scan() {
		return "", errors.New("empty rollout")
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" || rec.Payload.Cwd == "" {
		return "", errors.New("no session metadata")
	}
	return rec.Payload.Cwd, nil
}
