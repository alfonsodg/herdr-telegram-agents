package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// admissionClient completes durable contact admission before the library can
// advance getUpdates' offset. WithNotAsyncHandlers alone is insufficient:
// go-telegram/bot fetches into a buffered channel and advances the offset first.
// This wraps the same client's getUpdates response; there is only one poller.
type admissionClient struct {
	client bot.HttpClient
	admit  func(context.Context, *models.Update) error
}

func (c *admissionClient) Do(req *http.Request) (*http.Response, error) {
	res, err := c.client.Do(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/getUpdates") || res.StatusCode != http.StatusOK {
		return res, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, errors.New("read Telegram updates failed")
	}
	var envelope struct {
		OK     bool             `json:"ok"`
		Result []*models.Update `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errors.New("decode Telegram updates failed")
	}
	if envelope.OK && c.admit != nil {
		for _, u := range envelope.Result {
			if err := c.admit(req.Context(), u); err != nil {
				return nil, errors.New("private contact persistence unavailable; updates not acknowledged")
			}
		}
	}
	res.Body = io.NopCloser(bytes.NewReader(data))
	return res, nil
}

// PrivateRegistration is installed before polling starts. It must persist a
// first contact synchronously and return errors without private content.
type PrivateRegistration func(context.Context, domain.PrivateContact) (bool, error)

// Private ingress budgets per 10-second window. Contacts without an active
// grant share their own small budget, so they cannot starve grantees.
const (
	privateWindow        = 10 * time.Second
	privatePerActor      = 8
	privateTrustedTotal  = 32
	privateStrangerTotal = 8
)

// Capacity notices are sent on the synchronous poller, up to a second each.
// Each actor gets one per window and all actors together at most
// capacityNoticeTotal, so a full directory cannot become a polling stall.
const (
	capacityNoticeWindow = 10 * time.Minute
	capacityNoticeTotal  = 8
)

type privateIngress struct {
	window   time.Time
	counts   map[int64]int
	total    int
	strange  int
	trusted  func(int64) bool
	busy     map[int64]bool
	mu       sync.Mutex
	register PrivateRegistration
	started  time.Time
	seen     map[int64]bool
	first    map[int64]bool
	order    []int64
	// noticeWindow and noticed meter capacity notices; noticed holds at
	// most capacityNoticeTotal actors.
	noticeWindow time.Time
	noticed      map[int64]bool
}

func (g *Gateway) SetPrivateRegistration(register PrivateRegistration) {
	g.private.mu.Lock()
	defer g.private.mu.Unlock()
	g.private.register = register
}

// SetPrivateTrust tells the limiter which actors hold an active grant. nil
// (the default) treats every actor as trusted.
func (g *Gateway) SetPrivateTrust(trusted func(int64) bool) {
	g.private.mu.Lock()
	defer g.private.mu.Unlock()
	g.private.trusted = trusted
}

// trustedActor reports whether actor holds an active grant.
func (g *Gateway) trustedActor(actor int64) bool {
	g.private.mu.Lock()
	trusted := g.private.trusted
	g.private.mu.Unlock()
	return trusted == nil || trusted(actor)
}

func privateContact(u *models.Update, now time.Time) (domain.PrivateContact, bool) {
	m := u.Message
	if m == nil || m.Chat.Type != models.ChatTypePrivate || m.From == nil || m.From.IsBot || m.From.ID <= 0 || m.Chat.ID != m.From.ID || m.SenderChat != nil {
		return domain.PrivateContact{}, false
	}
	// Only content fields count. Unknown service messages fail closed.
	if m.Text == "" && m.Caption == "" && m.RichMessage == nil && m.Animation == nil && m.Audio == nil && m.Document == nil && m.PaidMedia == nil && len(m.Photo) == 0 && m.Sticker == nil && m.Story == nil && m.Video == nil && m.VideoNote == nil && m.Voice == nil && m.Checklist == nil && m.Contact == nil && m.Dice == nil && m.Game == nil && m.Poll == nil && m.Venue == nil && m.Location == nil {
		return domain.PrivateContact{}, false
	}
	return domain.PrivateContact{UpdateID: u.ID, ActorID: m.From.ID, ChatID: m.Chat.ID, Name: strings.TrimSpace(m.From.FirstName + " " + m.From.LastName), Username: m.From.Username, At: now}, true
}

func (g *Gateway) admitPrivate(ctx context.Context, u *models.Update) error {
	contact, ok := privateContact(u, g.queue.cfg.Now())
	if !ok {
		return nil
	}
	g.private.mu.Lock()
	register := g.private.register
	g.private.mu.Unlock()
	if register == nil {
		return nil
	}
	first, err := register(ctx, contact)
	if first {
		g.private.mu.Lock()
		if g.private.first == nil {
			g.private.first = map[int64]bool{}
		}
		g.private.first[u.ID] = true
		g.private.mu.Unlock()
	}
	if errors.Is(err, domain.ErrRecipientCapacity) || errors.Is(err, domain.ErrRegistrationBusy) {
		// Refused, not failed: acknowledge the update and keep polling.
		return nil
	}
	return err
}

func (g *Gateway) matchPrivate(u *models.Update) bool {
	if _, ok := privateContact(u, g.queue.cfg.Now()); ok {
		return true
	}
	if cm := u.MyChatMember; cm != nil && cm.Chat.Type == models.ChatTypePrivate && cm.Chat.ID > 0 {
		return true
	}
	q := u.CallbackQuery
	if q == nil || q.From.IsBot {
		return false
	}
	m := callbackMessage(q)
	return m != nil && m.Chat.Type == models.ChatTypePrivate && m.Chat.ID == q.From.ID
}

func (g *Gateway) onPrivate(ctx context.Context, _ *bot.Bot, u *models.Update) {
	if cm := u.MyChatMember; cm != nil {
		g.emit(ctx, "private_reachability", 0, domain.PrivateReachability{RecipientID: cm.Chat.ID, Unavailable: cm.NewChatMember.Type == models.ChatMemberTypeBanned || cm.NewChatMember.Type == models.ChatMemberTypeLeft})
		return
	}
	g.private.mu.Lock()
	if g.private.seen[u.ID] {
		g.private.mu.Unlock()
		return
	}
	register := g.private.register
	g.private.mu.Unlock()
	if register == nil {
		return
	}
	contact, ordinary := privateContact(u, g.queue.cfg.Now())
	if ordinary {
		if first, err := register(ctx, contact); err != nil {
			g.log.Warn("private contact registration failed")
			if errors.Is(err, domain.ErrRecipientCapacity) && g.capacityNoticeAllowed(contact.ActorID) {
				// A bounded best-effort notice; never block owner polling.
				noticeCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				_, _ = g.SendAt(noticeCtx, domain.TopicAddress{ChatID: contact.ChatID}, domain.Outgoing{Text: "The recipient directory is full. Ask the owner to review its capacity.", MaxParts: 1}, nil)
			}
			return
		} else if first {
			g.private.mu.Lock()
			if g.private.first == nil {
				g.private.first = map[int64]bool{}
			}
			g.private.first[u.ID] = true
			g.private.mu.Unlock()
		}
	}
	g.private.mu.Lock()
	g.private.seen[u.ID] = true
	g.private.order = append(g.private.order, u.ID)
	if len(g.private.order) > 4096 {
		delete(g.private.seen, g.private.order[0])
		g.private.order = g.private.order[1:]
	}
	first := g.private.first[u.ID]
	delete(g.private.first, u.ID)
	started := g.private.started
	g.private.mu.Unlock()
	var ev domain.PrivateMessage
	if ordinary {
		m := u.Message
		ev = domain.PrivateMessage{Contact: contact, Address: domain.TopicAddress{ChatID: m.Chat.ID, ThreadID: m.MessageThreadID}, MessageID: m.ID, Text: m.Text, Attachment: attachmentOf(m), SentAt: time.Unix(int64(m.Date), 0)}
	} else {
		q := u.CallbackQuery
		m := callbackMessage(q)
		ev = domain.PrivateMessage{Contact: domain.PrivateContact{ActorID: q.From.ID, ChatID: m.Chat.ID, UpdateID: u.ID}, Address: domain.TopicAddress{ChatID: m.Chat.ID, ThreadID: m.MessageThreadID}, MessageID: m.ID, CallbackID: q.ID, CallbackData: q.Data, SentAt: time.Unix(int64(m.Date), 0)}
	}
	ev.FirstContact = first
	ev.Stale = ev.SentAt.Before(started)
	if !g.admitPrivateEvent(ev.Contact.ActorID) || len(g.events) >= cap(g.events)/2 {
		if !g.trustedActor(ev.Contact.ActorID) {
			// No busy reply for strangers: a reply is work they could
			// make the bot do at will.
			g.log.Debug("[FIX] private stranger event dropped", "recipient_id", ev.Contact.ActorID)
			return
		}
		g.privateBusy(ev.Contact.ActorID)
		return
	}
	g.emit(ctx, "private_message", ev.Address.ThreadID, ev)
}

// rejectRetained prevents mutating owner messages from being replayed when
// routine startup retains Telegram updates for contact discovery.
func (g *Gateway) rejectRetained(ctx context.Context, m *models.Message, callback bool) bool {
	if g.rejectBefore.IsZero() || !time.Unix(int64(m.Date), 0).Before(g.rejectBefore) {
		return false
	}
	if !callback {
		cmd := domain.ParseCommand(m.Text, "")
		switch cmd.Kind {
		case domain.CmdHelp, domain.CmdStatus, domain.CmdScreen:
			return false
		}
	}
	g.log.Debug("retained mutation refused")
	return true
}

// Registration is durable before this limiter. Only control/event ingress is
// rejected under load, reserving half the event buffer for owner traffic.
func (g *Gateway) admitPrivateEvent(actor int64) bool {
	g.private.mu.Lock()
	defer g.private.mu.Unlock()
	now := g.queue.cfg.Now()
	if g.private.counts == nil || now.Sub(g.private.window) >= privateWindow {
		g.private.window = now
		g.private.counts = map[int64]int{}
		g.private.busy = map[int64]bool{}
		g.private.total = 0
		g.private.strange = 0
	}
	if g.private.counts[actor] >= privatePerActor {
		return false
	}
	// The trust lookup reads sharing state under its own lock; it never
	// calls back into the gateway.
	if g.private.trusted != nil && !g.private.trusted(actor) {
		if g.private.strange >= privateStrangerTotal {
			return false
		}
		g.private.counts[actor]++
		g.private.strange++
		return true
	}
	if g.private.total >= privateTrustedTotal {
		return false
	}
	g.private.counts[actor]++
	g.private.total++
	return true
}

// capacityNoticeAllowed counts a capacity notice for actor against the
// per-actor and global limits of the current window.
func (g *Gateway) capacityNoticeAllowed(actor int64) bool {
	g.private.mu.Lock()
	defer g.private.mu.Unlock()
	now := g.queue.cfg.Now()
	if g.private.noticed == nil || now.Sub(g.private.noticeWindow) >= capacityNoticeWindow {
		g.private.noticeWindow = now
		g.private.noticed = map[int64]bool{}
	}
	if g.private.noticed[actor] || len(g.private.noticed) >= capacityNoticeTotal {
		g.log.Debug("[FIX] capacity notice suppressed", "recipient_id", actor, "sent", len(g.private.noticed))
		return false
	}
	g.private.noticed[actor] = true
	return true
}

// PrivateBusy queues a bounded congestion notice without blocking the daemon.
func (g *Gateway) PrivateBusy(actor int64) { g.privateBusy(actor) }

func (g *Gateway) privateBusy(actor int64) {
	g.private.mu.Lock()
	defer g.private.mu.Unlock()
	if g.private.busy == nil {
		g.private.busy = map[int64]bool{}
	}
	if g.private.busy[actor] || len(g.private.busy) >= 32 {
		return
	}
	g.private.busy[actor] = true
	select {
	case g.privateNotices <- actor:
	default:
	}
	g.log.Warn("private ingress busy; contact retained", "recipient_id", actor)
}

func (g *Gateway) runPrivateNotices(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case actor := <-g.privateNotices:
			_, _ = g.SendAt(ctx, domain.TopicAddress{ChatID: actor}, domain.Outgoing{Text: "Your contact is registered. Private requests are busy; please retry after 10 seconds. No agent action was sent for the rejected request.", MaxParts: 1}, nil)
		}
	}
}
