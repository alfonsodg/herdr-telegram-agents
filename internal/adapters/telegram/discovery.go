package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const (
	// heldMax bounds the promotions kept while the owner is unknown.
	heldMax = 32
	// clockSkew is how far a promotion may predate Bind and still count;
	// Telegram's date and the local clock are not in sync.
	clockSkew = 30 * time.Second
	// setupRequestID tags the request_chat button so chat_shared replies can
	// be told apart from anything else.
	setupRequestID = 1
	// setupChooseButton is the reply keyboard label; the wizard text in
	// internal/app names it too.
	setupChooseButton = "Choose group"
	// groupAnonymousBot is the sender Telegram shows for an anonymous group
	// administrator; it can never be an operator.
	groupAnonymousBot = 1087968824
)

// Probe implements domain.SetupProbe for the setup wizard: it proves the
// token with getMe and finds the forum group two ways. The primary one is a
// private chat with the operator: /start answers with a request_chat button
// whose filters make Telegram add the bot to the chosen group as an
// administrator with the "Manage topics" right, and the resulting
// chat_shared message names the group. The fallback watches my_chat_member
// for a promotion done by hand. Pending updates are kept (deleteWebhook
// without dropping), but once the wizard binds its code a promotion dated
// before setup started is ignored (see admitMember): re-granting the right
// after setup started produces a fresh one.
type Probe struct {
	api *bot.Bot
	log *slog.Logger

	mu     sync.Mutex
	botID  int64
	code   string                      // one-time setup code from the wizard's link; "" = unbound
	owner  int64                       // the user who sent the code; only they may choose
	since  int64                       // unix seconds of Bind; older promotions never count
	held   []*models.ChatMemberUpdated // promotions seen before the owner is known
	seen   map[int64]bool
	out    chan domain.GroupCandidate
	cancel context.CancelFunc // set by Candidates
	done   chan struct{}      // closed when polling ends; nil before Candidates
}

var _ domain.SetupProbe = (*Probe)(nil)

// NewProbe builds the probe for a token. Extra opts are for tests
// (bot.WithServerURL).
func NewProbe(token string, log *slog.Logger, opts ...bot.Option) (*Probe, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Probe{log: log, seen: map[int64]bool{}, out: make(chan domain.GroupCandidate, 16)}
	api, err := NewBot(token, log, p.stop, opts...)
	if err != nil {
		return nil, err
	}
	p.api = api
	api.RegisterHandlerMatchFunc(func(u *models.Update) bool { return u.MyChatMember != nil }, p.onMember)
	api.RegisterHandlerMatchFunc(func(u *models.Update) bool {
		return u.Message != nil && u.Message.From != nil && u.Message.Chat.Type == models.ChatTypePrivate
	}, p.onPrivate)
	return p, nil
}

// Identity validates the token with getMe and clears any webhook while
// keeping pending updates.
//
// deleteWebhook is called with nil params on purpose: with
// DropPendingUpdates false every field is omitted and go-telegram/bot
// v1.25 sends a multipart body that holds nothing but the closing
// boundary, which Telegram answers with an empty body ("unexpected end of
// JSON input", seen 2026-09-02). nil params send no body at all, like getMe.
func (p *Probe) Identity(ctx context.Context) (domain.BotIdentity, error) {
	me, err := p.api.GetMe(ctx)
	if err != nil {
		return domain.BotIdentity{}, fmt.Errorf("getMe: %w", translate(err))
	}
	if _, err := p.api.DeleteWebhook(ctx, nil); err != nil {
		return domain.BotIdentity{}, fmt.Errorf("deleteWebhook: %w", translate(err))
	}
	p.mu.Lock()
	p.botID = me.ID
	p.mu.Unlock()
	p.log.Info("setup probe identified bot", slog.Int64("bot_id", me.ID), slog.String("username", me.Username))
	return domain.BotIdentity{ID: me.ID, Username: me.Username, HasTopicsEnabled: me.HasTopicsEnabled}, nil
}

// Candidates starts polling and streams groups until ctx is done or Close
// is called; the channel is closed afterwards. It may be called once.
func (p *Probe) Candidates(ctx context.Context) (<-chan domain.GroupCandidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done != nil {
		return nil, errors.New("setup probe already polling")
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel, p.done = cancel, make(chan struct{})
	done := p.done
	go func() {
		defer close(done)
		defer close(p.out)
		defer cancel()
		p.log.Info("setup probe polling for the group choice")
		p.api.Start(runCtx)
		p.log.Debug("setup probe polling stopped")
	}()
	return p.out, nil
}

// Close stops polling and waits for it to end. It is safe before
// Candidates and more than once.
func (p *Probe) Close() error {
	p.stop()
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

func (p *Probe) stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Bind sets the one-time code the wizard put into its link
// (domain.SetupLink). From then on only the user who sends
// "/start setup_<code>" gets the group button and may choose the group;
// everyone else who finds the bot during setup is ignored. The same holds
// for promotions by hand: until the owner is known they are held, then only
// the owner's count, and one dated before Bind (a pending update) never does.
func (p *Probe) Bind(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code, p.owner, p.held = code, 0, nil
	p.since = time.Now().Add(-clockSkew).Unix()
}

// allowPrivate reports whether a private message may drive setup, binding
// the sender as the owner when it carries the code. On binding it returns
// the promotions held so far for re-evaluation.
func (p *Probe) allowPrivate(m *models.Message) (bool, []*models.ChatMemberUpdated) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.code == "":
		return true, nil
	case p.owner != 0:
		return m.From.ID == p.owner, nil
	case m.ChatShared == nil && m.Text == "/start "+domain.SetupStartPrefix+p.code:
		p.owner = m.From.ID
		held := p.held
		p.held = nil
		return true, held
	}
	return false, nil
}

// admitMember decides whether a promotion may become a candidate now.
// Before Bind every promotion may; after Bind one older than setup is
// dropped, one seen before the owner is known is held, and afterwards only
// the owner's is admitted. Without this, anyone who promoted the bot in
// their own forum before the link holder showed up became the only
// candidate and the operator.
func (p *Probe) admitMember(cm *models.ChatMemberUpdated) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.code == "":
		return true
	case int64(cm.Date) < p.since:
		p.log.Info("[FIX] setup probe: promotion from before setup started ignored", slog.Int64("chat_id", cm.Chat.ID), slog.Int64("from_id", cm.From.ID))
		return false
	case p.owner == 0:
		if len(p.held) >= heldMax {
			p.log.Warn("[FIX] setup probe: too many promotions before the setup link was opened, one dropped", slog.Int64("chat_id", cm.Chat.ID), slog.Int64("from_id", cm.From.ID))
			return false
		}
		p.held = append(p.held, cm)
		p.log.Info("[FIX] setup probe: promotion held until the setup link is opened", slog.Int64("chat_id", cm.Chat.ID), slog.Int64("from_id", cm.From.ID))
		return false
	case cm.From.ID != p.owner:
		p.log.Info("[FIX] setup probe: promotion by another user than the one running setup ignored", slog.Int64("chat_id", cm.Chat.ID), slog.Int64("from_id", cm.From.ID))
		return false
	}
	return true
}

// Process feeds one update to the probe's handlers, bypassing polling. It
// exists for tests; the daemon never calls it.
func (p *Probe) Process(ctx context.Context, u *models.Update) {
	p.api.ProcessUpdate(ctx, u)
}

// onPrivate serves the operator's private chat: a chat_shared message is
// the group choice, anything else (typically /start) gets the button.
func (p *Probe) onPrivate(ctx context.Context, b *bot.Bot, u *models.Update) {
	m := u.Message
	ok, held := p.allowPrivate(m)
	if !ok {
		p.log.Info("[FIX] setup probe: private message from a user without the setup code ignored", slog.Int64("from_id", m.From.ID))
		return
	}
	if m.ChatShared != nil {
		p.onChatShared(ctx, b, m)
		return
	}
	p.log.Info("setup probe: private message, sending the group button", slog.Int64("from_id", m.From.ID))
	p.reply(ctx, b, m.Chat.ID,
		"Choose the forum group to mirror Herdr agents into. Telegram will add me there as an administrator with the \"Manage topics\" right.",
		chooseGroupKeyboard())
	for _, cm := range held {
		if cm.From.ID != m.From.ID {
			p.log.Info("[FIX] setup probe: held promotion by another user than the one running setup ignored", slog.Int64("chat_id", cm.Chat.ID), slog.Int64("from_id", cm.From.ID))
			continue
		}
		p.evaluateMember(ctx, b, cm)
	}
}

// chooseGroupKeyboard builds the request_chat button. The bot asks for
// "Manage topics" (the sync itself), "Delete messages" (removing its own
// "changed the topic icon" notices) and "Pin messages" (the dashboard in
// General). The user rights must be a superset of the bot rights (Bot API
// rule), and promoting needs the "Add admins" right, so all four are
// required of the user.
func chooseGroupKeyboard() models.ReplyMarkup {
	return &models.ReplyKeyboardMarkup{
		ResizeKeyboard:  true,
		OneTimeKeyboard: true,
		Keyboard: [][]models.KeyboardButton{{{
			Text: setupChooseButton,
			RequestChat: &models.KeyboardButtonRequestChat{
				RequestID:               setupRequestID,
				ChatIsChannel:           false,
				ChatIsForum:             true,
				UserAdministratorRights: &models.ChatAdministratorRights{CanManageTopics: true, CanDeleteMessages: true, CanPinMessages: true, CanPromoteMembers: true},
				BotAdministratorRights:  &models.ChatAdministratorRights{CanManageTopics: true, CanDeleteMessages: true, CanPinMessages: true},
				RequestTitle:            true,
			},
		}}},
	}
}

// onChatShared confirms the chosen group with getChat and getChatMember
// before reporting it, because the client may have failed to add the bot.
func (p *Probe) onChatShared(ctx context.Context, b *bot.Bot, m *models.Message) {
	shared := m.ChatShared
	if shared.RequestID != setupRequestID {
		p.log.Debug("setup probe: chat_shared with foreign request id", slog.Int("request_id", shared.RequestID))
		return
	}
	title := shared.Title
	chat, err := b.GetChat(ctx, &bot.GetChatParams{ChatID: shared.ChatID})
	if err != nil {
		p.log.Warn("setup probe: getChat failed", slog.Int64("chat_id", shared.ChatID), slog.String("err", translate(err).Error()))
		p.reply(ctx, b, m.Chat.ID, "I cannot see that group. Add me to it first, then choose it again.", chooseGroupKeyboard())
		return
	}
	if chat.Title != "" {
		title = chat.Title
	}
	if !chat.IsForum {
		p.log.Info("setup probe: chosen chat is not a forum", slog.Int64("chat_id", shared.ChatID))
		p.reply(ctx, b, m.Chat.ID, fmt.Sprintf("%q has Topics disabled. Enable Topics in the group settings and choose it again.", title), chooseGroupKeyboard())
		return
	}
	p.mu.Lock()
	botID := p.botID
	p.mu.Unlock()
	member, err := b.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: shared.ChatID, UserID: botID})
	if err != nil {
		p.log.Warn("setup probe: getChatMember failed", slog.Int64("chat_id", shared.ChatID), slog.String("err", translate(err).Error()))
		p.reply(ctx, b, m.Chat.ID, "I could not check my rights there. Add me to the group as an administrator with \"Manage topics\" and choose it again.", chooseGroupKeyboard())
		return
	}
	if !canManageTopics(*member) {
		p.log.Info("setup probe: bot lacks topic rights in the chosen chat", slog.Int64("chat_id", shared.ChatID), slog.String("status", string(member.Type)))
		p.reply(ctx, b, m.Chat.ID, fmt.Sprintf("I am in %q but without the \"Manage topics\" right. Grant it in the group's administrators list and choose the group again.", title), chooseGroupKeyboard())
		return
	}
	p.reply(ctx, b, m.Chat.ID, fmt.Sprintf("Connected to %q. Go back to the Herdr terminal to confirm.", title), &models.ReplyKeyboardRemove{RemoveKeyboard: true})
	p.report(ctx, domain.GroupCandidate{ChatID: shared.ChatID, Title: title, FromID: m.From.ID, FromUsername: m.From.Username})
}

// onMember checks one my_chat_member update. The chat's forum flag is
// taken from the update and confirmed with getChat when absent, because
// the flag is optional on the wire.
func (p *Probe) onMember(ctx context.Context, b *bot.Bot, u *models.Update) {
	cm := u.MyChatMember
	if cm.From.IsBot || cm.From.ID == groupAnonymousBot {
		p.log.Warn("[FIX] setup probe: promotion by an anonymous admin or a bot ignored; promote the bot as yourself",
			slog.Int64("chat_id", cm.Chat.ID), slog.Int64("from_id", cm.From.ID))
		return
	}
	if !p.admitMember(cm) {
		return
	}
	p.evaluateMember(ctx, b, cm)
}

// evaluateMember reports an admitted promotion when it grants topic rights
// in a forum.
func (p *Probe) evaluateMember(ctx context.Context, b *bot.Bot, cm *models.ChatMemberUpdated) {
	if !canManageTopics(cm.NewChatMember) {
		p.log.Debug("setup probe: membership change without topic rights",
			slog.Int64("chat_id", cm.Chat.ID), slog.String("status", string(cm.NewChatMember.Type)))
		return
	}
	title, isForum := cm.Chat.Title, cm.Chat.IsForum
	if !isForum {
		full, err := b.GetChat(ctx, &bot.GetChatParams{ChatID: cm.Chat.ID})
		if err != nil {
			p.log.Warn("setup probe: getChat failed", slog.Int64("chat_id", cm.Chat.ID), slog.String("err", translate(err).Error()))
			return
		}
		isForum = full.IsForum
		if full.Title != "" {
			title = full.Title
		}
	}
	if !isForum {
		p.log.Debug("setup probe: promoted in a chat that is not a forum", slog.Int64("chat_id", cm.Chat.ID))
		return
	}
	p.report(ctx, domain.GroupCandidate{ChatID: cm.Chat.ID, Title: title, FromID: cm.From.ID, FromUsername: cm.From.Username})
}

// report hands a candidate to the wizard once per chat.
func (p *Probe) report(ctx context.Context, c domain.GroupCandidate) {
	p.mu.Lock()
	dup := p.seen[c.ChatID]
	p.seen[c.ChatID] = true
	p.mu.Unlock()
	if dup {
		p.log.Debug("setup probe: candidate already reported", slog.Int64("chat_id", c.ChatID))
		return
	}
	p.log.Info("setup probe: forum group candidate", slog.Int64("chat_id", c.ChatID), slog.Int64("from_id", c.FromID))
	select {
	case p.out <- c:
	case <-ctx.Done():
		p.log.Warn("setup probe: candidate dropped, context done", slog.Int64("chat_id", c.ChatID))
	}
}

// reply sends one private message; failures are logged, never fatal.
func (p *Probe) reply(ctx context.Context, b *bot.Bot, chatID int64, text string, markup models.ReplyMarkup) {
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text, ReplyMarkup: markup}); err != nil {
		p.log.Warn("setup probe: sendMessage failed", slog.Int64("chat_id", chatID), slog.String("err", translate(err).Error()))
	}
}
