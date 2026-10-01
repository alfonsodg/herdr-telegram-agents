package app

import (
	"context"
	"html"
	"log/slog"
	"regexp"
	"strings"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// redactingGateway wraps a TelegramGateway so every text that leaves the
// daemon (screen posts, pager messages, documents, button labels, panel
// edits, notices) passes the domain.Redactor first. It is the single
// insertion point for the privacy.redact option: the call sites that build
// posts never need to know about it. Everything else (pins, deletes, the
// private-chat probe) is passed through untouched.
//
// The option switches the patterns only: the bot token is masked on every
// call, since nobody needs it in the group and it controls the bot.
type redactingGateway struct {
	domain.TelegramGateway
	red     *domain.Redactor
	enabled func() bool
	log     *slog.Logger
}

// newRedactingGateway returns tg wrapped; enabled is read on every call so
// a change of the option applies to the next post. A nil enabled means
// always on.
func newRedactingGateway(tg domain.TelegramGateway, red *domain.Redactor, enabled func() bool, log *slog.Logger) domain.TelegramGateway {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if enabled == nil {
		enabled = func() bool { return true }
	}
	if red == nil {
		red = domain.NewRedactor()
	}
	return &redactingGateway{TelegramGateway: tg, red: red, enabled: enabled, log: log}
}

// NewRedactingGateway wraps tg for callers outside the bridge and daemon
// (the reconciler, the share panel), so topic names and toasts follow the
// same privacy.redact option.
func NewRedactingGateway(tg domain.TelegramGateway, botToken string, enabled func() bool, log *slog.Logger) domain.TelegramGateway {
	return newRedactingGateway(tg, domain.NewRedactor(botToken), enabled, log)
}

// redaction is one call's pass: the option is read once, so every text of
// a message is masked the same way, and the counts add up for the log.
type redaction struct {
	full  bool
	red   *domain.Redactor
	stats domain.RedactionStats
}

func (g *redactingGateway) begin() *redaction {
	return &redaction{full: g.enabled(), red: g.red, stats: domain.RedactionStats{}}
}

func (r *redaction) text(s string) string {
	redact := r.red.RedactExact
	if r.full {
		redact = r.red.Redact
	}
	out, stats := redact(s)
	for k, n := range stats {
		r.stats[k] += n
	}
	return out
}

func (r *redaction) markup(s string, html bool) string {
	if !html {
		return r.text(s)
	}
	return redactHTML(s, r.text)
}

// buttons copies the slice so the caller's keyboard (kept by outbound for
// later edits) is never rewritten in place.
func (r *redaction) buttons(buttons []domain.Button) []domain.Button {
	if len(buttons) == 0 {
		return buttons
	}
	out := make([]domain.Button, len(buttons))
	copy(out, buttons)
	for i := range out {
		out[i].Text = r.text(out[i].Text)
	}
	return out
}

func (g *redactingGateway) Send(ctx context.Context, out domain.Outgoing) (int, error) {
	r := g.begin()
	out.Text = r.markup(out.Text, out.HTML)
	out.Footer = r.text(out.Footer)
	out.Buttons = r.buttons(out.Buttons)
	g.report(out.ThreadID, "send", r)
	return g.TelegramGateway.Send(ctx, out)
}

// SendDirect carries the pager's HTML, so it is masked between tags like
// Send.
func (g *redactingGateway) SendDirect(ctx context.Context, userID int64, out domain.Outgoing) (int, error) {
	r := g.begin()
	out.Text = r.markup(out.Text, out.HTML)
	out.Footer = r.text(out.Footer)
	out.Buttons = r.buttons(out.Buttons)
	g.report(0, "direct", r)
	return g.TelegramGateway.SendDirect(ctx, userID, out)
}

func (g *redactingGateway) SendDocument(ctx context.Context, doc domain.Document) error {
	r := g.begin()
	doc.Data = []byte(r.text(string(doc.Data)))
	doc.Caption = r.text(doc.Caption)
	doc.Name = r.text(doc.Name)
	g.report(doc.ThreadID, "document", r)
	return g.TelegramGateway.SendDocument(ctx, doc)
}

func (g *redactingGateway) EditText(ctx context.Context, messageID int, text string, html bool, buttons []domain.Button) error {
	r := g.begin()
	text = r.markup(text, html)
	buttons = r.buttons(buttons)
	g.report(0, "edittext", r)
	return g.TelegramGateway.EditText(ctx, messageID, text, html, buttons)
}

func (g *redactingGateway) EditButtons(ctx context.Context, messageID int, buttons []domain.Button) error {
	r := g.begin()
	buttons = r.buttons(buttons)
	g.report(0, "buttons", r)
	return g.TelegramGateway.EditButtons(ctx, messageID, buttons)
}

// CreateTopic masks the topic name: it is built from the agent label.
func (g *redactingGateway) CreateTopic(ctx context.Context, name string, status domain.Status) (domain.Topic, error) {
	r := g.begin()
	name = domain.DisplayName(r.text(name))
	g.report(0, "create_topic", r)
	return g.TelegramGateway.CreateTopic(ctx, name, status)
}

// EditTopic masks a renamed topic; the caller's patch is not modified.
func (g *redactingGateway) EditTopic(ctx context.Context, threadID int, patch domain.TopicPatch) error {
	if patch.Name == nil {
		return g.TelegramGateway.EditTopic(ctx, threadID, patch)
	}
	r := g.begin()
	name := domain.DisplayName(r.text(*patch.Name))
	patch.Name = &name
	g.report(threadID, "edit_topic", r)
	return g.TelegramGateway.EditTopic(ctx, threadID, patch)
}

// AnswerButton masks the toast text.
func (g *redactingGateway) AnswerButton(ctx context.Context, callbackID, text string) error {
	r := g.begin()
	text = r.text(text)
	g.report(0, "answer", r)
	return g.TelegramGateway.AnswerButton(ctx, callbackID, text)
}

// htmlTag matches one tag of Telegram's HTML subset.
var htmlTag = regexp.MustCompile(`<[^<>]*>`)

// redactHTML applies redact to the text between tags only, so a value
// pattern can never consume a closing tag and break the message. A secret
// split by a tag is missed; the renderers never put tags inside a word.
func redactHTML(text string, redact func(string) string) string {
	tags := htmlTag.FindAllStringIndex(text, -1)
	if len(tags) == 0 {
		return redactHTMLText(text, redact)
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, t := range tags {
		b.WriteString(redactHTMLText(text[last:t[0]], redact))
		b.WriteString(text[t[0]:t[1]])
		last = t[1]
	}
	b.WriteString(redactHTMLText(text[last:], redact))
	return b.String()
}

// redactHTMLText masks one escaped text run. The patterns see the text as
// it reads (`API_KEY='…'`, not `API_KEY=&#39;…&#39;`); a run with nothing
// masked is returned byte for byte, a masked one is escaped again.
func redactHTMLText(escaped string, redact func(string) string) string {
	plain := html.UnescapeString(escaped)
	masked := redact(plain)
	if masked == plain {
		return escaped
	}
	return html.EscapeString(masked)
}

// report logs the kinds and counts of what was masked; the values never
// reach the log.
func (g *redactingGateway) report(threadID int, via string, r *redaction) {
	if r.stats.Total() == 0 {
		return
	}
	if !r.full {
		g.log.Warn("[FIX] bot token masked while redaction is off", slog.Int("thread_id", threadID), slog.String("via", via), slog.String("kinds", r.stats.String()))
		return
	}
	g.log.Info("secrets redacted", slog.Int("thread_id", threadID), slog.String("via", via), slog.String("kinds", r.stats.String()))
}
