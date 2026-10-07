package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// kindOpenCode is the Herdr agent kind OpenCodeReader understands.
const kindOpenCode = "opencode"

// openCodeEditTools are the opencode tools whose input names a file they
// change.
var openCodeEditTools = map[string]bool{"edit": true, "write": true}

// OpenCodeReader implements domain.ReplySource for opencode. opencode
// keeps its sessions in a private SQLite database rather than a file per
// session, so the reader asks its session export CLI through the injected
// export function for the session Herdr reports for the pane.
// The session tuple comes from Herdr at read time and is used for that
// one lookup only: it is never stored or logged (see domain.SessionTuple),
// and a tuple whose digest differs from the topic's key means the pane now
// runs another session, so the reader answers ErrNoReply rather than post
// that session's reply into this topic.
type OpenCodeReader struct {
	session func(ctx context.Context, paneID string) (domain.SessionTuple, error)
	export  func(ctx context.Context, sessionID string) ([]byte, error)
	now     func() time.Time
	log     *slog.Logger
}

// NewOpenCodeReader wires the reader over Herdr's session lookup and the
// opencode export runner.
func NewOpenCodeReader(session func(context.Context, string) (domain.SessionTuple, error),
	export func(context.Context, string) ([]byte, error), log *slog.Logger) *OpenCodeReader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &OpenCodeReader{session: session, export: export, now: time.Now, log: log}
}

// LastReply returns the text opencode wrote after the operator's last
// prompt in the pane's session. Every failure is domain.ErrNoReply wrapped
// with the reason; the caller falls back to the screen.
func (r *OpenCodeReader) LastReply(ctx context.Context, agent domain.Agent) (domain.Reply, error) {
	if err := ctx.Err(); err != nil {
		return domain.Reply{}, err
	}
	if agent.Kind != kindOpenCode {
		return domain.Reply{}, fmt.Errorf("%w: unsupported agent %q", domain.ErrUnsupportedAgent, agent.Kind)
	}
	tuple, err := r.session(ctx, agent.PaneID)
	if err != nil {
		return domain.Reply{}, classifyOpenCodeError(ctx, err, "session lookup failed")
	}
	if err := ctx.Err(); err != nil {
		return domain.Reply{}, err
	}
	if tuple.Agent != kindOpenCode || tuple.Kind != "id" || tuple.Value == "" {
		return domain.Reply{}, fmt.Errorf("%w: herdr reports no opencode session id for the pane", domain.ErrNoReply)
	}
	digest := tuple.Digest()
	if digest == "" {
		return domain.Reply{}, fmt.Errorf("%w: herdr reports an incomplete session for the pane", domain.ErrNoReply)
	}
	if agent.SessionDigest == "" || digest != agent.SessionDigest {
		return domain.Reply{}, fmt.Errorf("%w: the pane now runs another session", domain.ErrNoReply)
	}
	out, err := r.export(ctx, tuple.Value)
	if err != nil {
		return domain.Reply{}, classifyOpenCodeError(ctx, err, "export failed")
	}
	if err := ctx.Err(); err != nil {
		return domain.Reply{}, err
	}
	var doc openCodeExport
	if err := json.Unmarshal(out, &doc); err != nil {
		return domain.Reply{}, fmt.Errorf("%w: invalid export JSON", domain.ErrNoReply)
	}
	if doc.Info.ID != tuple.Value {
		return domain.Reply{}, fmt.Errorf("%w: opencode exported another session", domain.ErrNoReply)
	}
	text, meta, err := openCodeLastReply(doc.Messages)
	if err != nil {
		return domain.Reply{}, err
	}
	var age time.Duration
	if !meta.Ended.IsZero() {
		age = r.now().Sub(meta.Ended)
	}
	source := "opencode export"
	r.log.Debug("opencode reply found", slog.String("pane", agent.PaneID),
		slog.Int("bytes", len(out)), slog.Int("chars", len(text)), slog.Int64("age_ms", age.Milliseconds()),
		slog.String("model", meta.Model), slog.Int("files", len(meta.Files)), slog.Int("output_tokens", meta.OutputTokens))
	return domain.Reply{Text: text, Source: source, Age: age, Written: meta.Ended, Meta: meta}, nil
}

func classifyOpenCodeError(ctx context.Context, err error, category string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", domain.ErrNoReply, category)
}

// openCodeExport is the part of "opencode export" output the reader uses.
type openCodeExport struct {
	Info struct {
		ID string `json:"id"`
	} `json:"info"`
	Messages []openCodeMessage `json:"messages"`
}

type openCodeMessage struct {
	Type    string         `json:"type"`
	Content []openCodePart `json:"content"`
	Model   struct {
		ID string `json:"id"`
	} `json:"model"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Output int `json:"output"`
	} `json:"tokens"`
	Info struct {
		Role    string `json:"role"`
		ModelID string `json:"modelID"`
		Time    struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		} `json:"time"`
		Tokens struct {
			Output int `json:"output"`
		} `json:"tokens"`
	} `json:"info"`
	Parts []openCodePart `json:"parts"`
}

// openCodePart is one part of a message. Only "text" is prose written for
// the operator; "reasoning", "tool", "patch", "step-start", "step-finish",
// "compaction" and "agent" are opencode's bookkeeping. Edit and write tool
// parts still name the files the turn changed.
type openCodePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Status string `json:"status"`
		Input  struct {
			FilePath string `json:"filePath"`
		} `json:"input"`
	} `json:"state"`
}

// openCodeLastReply joins the text parts of the assistant messages after
// the last user message, in order, with a blank line between them: a turn
// with tool calls is several messages that read as one reply. The meta
// covers the same turn: the prompt's time, the last assistant message's
// completion, its model, the output tokens of every step and the files
// edited, in the order first met.
func openCodeLastReply(messages []openCodeMessage) (string, domain.TurnMeta, error) {
	start := len(messages)
	for start > 0 && messages[start-1].role() != "user" {
		start--
	}
	if start == 0 {
		return "", domain.TurnMeta{}, fmt.Errorf("%w: no user prompt in export", domain.ErrNoReply)
	}
	var meta domain.TurnMeta
	meta.Started = unixMilli(messages[start-1].created())
	var texts []string
	seen := map[string]bool{}
	for _, m := range messages[start:] {
		if m.role() != "assistant" {
			continue
		}
		meta.OutputTokens += m.outputTokens()
		if model := m.modelID(); model != "" {
			meta.Model = model
		}
		if t := unixMilli(m.completed()); !t.IsZero() {
			meta.Ended = t
		} else if t := unixMilli(m.created()); !t.IsZero() {
			meta.Ended = t
		}
		for _, p := range m.parts() {
			switch {
			case p.Type == "text":
				if t := strings.TrimSpace(p.Text); t != "" {
					texts = append(texts, t)
				}
			case p.Type == "tool" && p.State.Status == "completed" && openCodeEditTools[p.Tool] && p.State.Input.FilePath != "" && !seen[p.State.Input.FilePath]:
				seen[p.State.Input.FilePath] = true
				meta.Files = append(meta.Files, p.State.Input.FilePath)
			}
		}
	}
	if !openCodeTurnSettled(messages[start:]) {
		return "", domain.TurnMeta{}, fmt.Errorf("%w: the turn is still running", domain.ErrReplyPending)
	}
	if len(texts) == 0 {
		return "", domain.TurnMeta{}, fmt.Errorf("%w: no text after the last prompt", domain.ErrNoReply)
	}
	return strings.Join(texts, "\n\n"), meta, nil
}

// openCodeTurnSettled reports whether the newest activity in the window is
// text rather than tool work. opencode completes a message (and can report
// a done or idle status) at every step, so a read whose last part is a
// tool call must not post that step as the turn's answer.
func openCodeTurnSettled(ms []openCodeMessage) bool {
	textPos, toolPos := -1, -1
	pos := 0
	for _, m := range ms {
		for _, p := range m.parts() {
			pos++
			switch p.Type {
			case "text":
				if strings.TrimSpace(p.Text) != "" {
					textPos = pos
				}
			case "tool":
				toolPos = pos
			}
		}
	}
	return textPos >= 0 && textPos > toolPos
}

func (m openCodeMessage) role() string {
	if m.Type != "" {
		return m.Type
	}
	return m.Info.Role
}

func (m openCodeMessage) parts() []openCodePart {
	if m.Type != "" {
		return m.Content
	}
	return m.Parts
}

func (m openCodeMessage) modelID() string {
	if m.Type != "" {
		return m.Model.ID
	}
	return m.Info.ModelID
}

func (m openCodeMessage) created() int64 {
	if m.Type != "" {
		return m.Time.Created
	}
	return m.Info.Time.Created
}

func (m openCodeMessage) completed() int64 {
	if m.Type != "" {
		return m.Time.Completed
	}
	return m.Info.Time.Completed
}

func (m openCodeMessage) outputTokens() int {
	if m.Type != "" {
		return m.Tokens.Output
	}
	return m.Info.Tokens.Output
}

// unixMilli converts opencode's millisecond timestamps; zero stays zero.
func unixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
