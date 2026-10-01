package app

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// Sharing owns policy writes. Readers receive copies; network I/O never holds
// its mutex. Permission changes publish denial before attempting persistence.
type Sharing struct {
	BotID     int64
	ProcessID string
	Now       func() time.Time
	Agent     func(domain.Key) (domain.Agent, bool)
	inflight  map[string]map[string]context.CancelFunc
	mu        sync.Mutex
	store     domain.SharingStore
	state     domain.SharingState
	log       *slog.Logger
	disabled  bool
	dirty     bool
	lastSave  time.Time
	// contactWindow and contactCount meter first contacts per minute.
	contactWindow time.Time
	contactCount  int
}

func NewSharing(ctx context.Context, store domain.SharingStore, log *slog.Logger) *Sharing {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s, err := store.Load(ctx)
	if err != nil {
		log.Warn("private sharing disabled: state unavailable")
	}
	return &Sharing{ProcessID: rand.Text(), inflight: map[string]map[string]context.CancelFunc{}, store: store, state: s, log: log, disabled: err != nil}
}

var ErrSharingUnavailable = errors.New("private sharing unavailable; state needs repair")
var ErrSharingCapacity = domain.ErrRecipientCapacity

// Snapshot never returns the writer's maps. Disabled policy has no grants.
func (s *Sharing) Snapshot() (domain.SharingState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return domain.NewSharingState(), false
	}
	return s.state.Clone(), true
}

// Grantee reports whether the actor holds a grant the owner has not revoked
// and that has not expired. The private ingress limiter gives such actors
// the larger budget.
func (s *Sharing) Grantee(actor int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return false
	}
	for _, g := range s.state.Grants {
		if g.RecipientID == actor && g.State != domain.GrantRevoked && g.State != domain.GrantExpired {
			return true
		}
	}
	return false
}

// MaxNewContactsPerMinute caps first-contact registrations. Each one is a
// durable write on the Telegram polling path, so a flood of new accounts
// must not turn into a write per account.
const MaxNewContactsPerMinute = 30

// newContactAllowed counts a first contact against the per-minute cap.
// Called with s.mu held.
func (s *Sharing) newContactAllowed(now time.Time) bool {
	if now.Sub(s.contactWindow) >= time.Minute {
		s.contactWindow, s.contactCount = now, 0
	}
	if s.contactCount >= MaxNewContactsPerMinute {
		return false
	}
	s.contactCount++
	return true
}

// Register saves first contact before onboarding can be acknowledged. Later
// metadata refreshes retain owner-hidden state and are coalesced by Flush.
func (s *Sharing) Register(ctx context.Context, id, chatID int64, name, username string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return false, ErrSharingUnavailable
	}
	if id <= 0 || chatID != id || now.IsZero() {
		return false, errors.New("invalid private contact")
	}
	r, exists := s.state.Recipients[id]
	if !exists && len(s.state.Recipients) >= domain.MaxRecipients {
		s.log.Warn("private recipient capacity reached")
		return false, ErrSharingCapacity
	}
	if !exists && !s.newContactAllowed(now) {
		s.log.Warn("[FIX] new private contact refused: first contacts arrive too fast", slog.Int("per_minute", MaxNewContactsPerMinute))
		return false, domain.ErrRegistrationBusy
	}
	if !exists {
		r = domain.Recipient{ID: id, ChatID: chatID, FirstSeen: now}
	}
	r.Name = boundedContact(name, domain.MaxRecipientName)
	r.Username = boundedContact(username, domain.MaxRecipientUsername)
	if now.After(r.LastSeen) {
		r.LastSeen = now
	}
	r.Unavailable = false
	old := s.state.Clone()
	s.state.Recipients[id] = r
	s.dirty = true
	if !exists {
		if err := s.saveLocked(ctx, now); err != nil {
			s.state = old
			if errors.Is(err, domain.ErrSharingStateFull) {
				// Retrying this contact can never fit; a plain error would
				// leave its update unacknowledged and stall polling.
				s.log.Warn("[FIX] new private contact refused: sharing state size limit reached")
				return false, ErrSharingCapacity
			}
			return false, err
		}
		s.log.Info("private recipient registered", slog.Int64("recipient_id", id))
	} else {
		s.log.Debug("private contact refresh coalesced", slog.Int64("recipient_id", id))
	}
	return !exists, nil
}

func boundedContact(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	r := []rune(strings.TrimSpace(value))
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

// Flush coalesces metadata for 30 seconds. Force is for shutdown and explicit
// checkpoints. Permission mutations always call saveLocked immediately.
func (s *Sharing) Flush(ctx context.Context, now time.Time, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return ErrSharingUnavailable
	}
	if !s.dirty || (!force && now.Sub(s.lastSave) < 30*time.Second) {
		return nil
	}
	return s.saveLocked(ctx, now)
}

func (s *Sharing) saveLocked(ctx context.Context, now time.Time) error {
	if s.state.Revision == math.MaxUint64 {
		s.disabled = true
		return ErrSharingUnavailable
	}
	next := s.state.Clone()
	next.Revision++
	if err := s.store.Save(ctx, next); err != nil {
		s.log.Warn("sharing policy persistence failed")
		return err
	}
	s.state = next
	s.lastSave = now
	s.dirty = false
	return nil
}

// ObserveUpdate coalesces the ingestion checkpoint with metadata. It is not
// proof that any Herdr side effect completed and must never trigger retries.
func (s *Sharing) ObserveUpdate(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.disabled && id > s.state.UpdateID {
		s.state.UpdateID = id
		s.dirty = true
	}
}

func (s *Sharing) Reachability(ctx context.Context, id int64, unavailable bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return ErrSharingUnavailable
	}
	r, ok := s.state.Recipients[id]
	if !ok || r.Unavailable == unavailable {
		return nil
	}
	r.Unavailable = unavailable
	s.state.Recipients[id] = r
	s.dirty = true
	s.log.Info("private recipient reachability changed", slog.Int64("recipient_id", id), slog.Bool("unavailable", unavailable))
	return s.saveLocked(ctx, now)
}

// GrantRequest is the owner-confirmed choice. The panel validates the owner
// route and exact session again immediately before submitting it.
type GrantRequest struct {
	ExpectedRevision                       *uint64
	RecipientID                            int64
	Key                                    domain.Key
	Role                                   domain.ShareRole
	RepositoryRead, CloseAgent, LocalFocus bool
	ExpiresAt                              time.Time
	Bot                                    domain.BotIdentity
}

func (s *Sharing) Grant(ctx context.Context, r GrantRequest, now time.Time) (domain.ShareGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || !r.Bot.PrivateTopicsReady() || !s.state.MatchesBot(r.Bot) {
		return domain.ShareGrant{}, ErrSharingUnavailable
	}
	if _, ok := s.state.Recipients[r.RecipientID]; !ok {
		return domain.ShareGrant{}, errors.New("recipient has not contacted this bot")
	}
	if r.Key.PaneID == "" || r.Key.TerminalID == "" || (r.Role != domain.ShareRead && r.Role != domain.ShareControl) {
		return domain.ShareGrant{}, errors.New("invalid grant")
	}
	g := domain.ShareGrant{}
	for _, existing := range s.state.Grants {
		if existing.RecipientID == r.RecipientID && existing.Key == r.Key {
			g = existing
			break
		}
	}
	if r.ExpectedRevision != nil && g.Revision != *r.ExpectedRevision {
		return g, errors.New("grant changed; refresh the owner panel")
	}
	if g.ID == "" {
		if len(s.state.Grants) >= domain.MaxShareGrants {
			return g, ErrSharingCapacity
		}
		g = domain.ShareGrant{ID: rand.Text(), RecipientID: r.RecipientID, Key: r.Key, State: domain.GrantPending}
	}
	if g.Revision == math.MaxUint64 {
		return g, ErrSharingUnavailable
	}
	g.Revision++
	g.Role = r.Role
	g.RepositoryRead = r.RepositoryRead
	g.CloseAgent = r.CloseAgent
	g.LocalFocus = r.LocalFocus
	g.ExpiresAt = r.ExpiresAt
	g.ProcessID = s.ProcessID
	if g.State != domain.GrantActive {
		g.State = domain.GrantPending
		g.ActivatedAt = now
		g.HistoryCursor = 0
	}
	old := s.state.Clone()
	s.state.BotID = r.Bot.ID
	s.state.Grants[g.ID] = g
	s.dirty = true
	s.cancelLocked(g.ID)
	if err := s.saveLocked(ctx, now); err != nil {
		s.state = old
		if _, existed := old.Grants[g.ID]; existed {
			g.State = domain.GrantSuspended
			s.state.Grants[g.ID] = g
			s.dirty = true
		}
		return domain.ShareGrant{}, err
	}
	s.log.Info("share grant saved", slog.String("grant_id", g.ID), slog.Int64("recipient_id", g.RecipientID), slog.Uint64("revision", g.Revision), slog.String("role", string(g.Role)))
	return g, nil
}

func (s *Sharing) ChangeState(ctx context.Context, id string, revision uint64, next domain.GrantState, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return ErrSharingUnavailable
	}
	g, ok := s.state.Grants[id]
	if !ok || g.Revision != revision {
		return errors.New("stale grant selection")
	}
	changed, err := g.Transition(next)
	if err != nil {
		return err
	}
	if next == domain.GrantActive && (!g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt)) {
		return errors.New("grant expired; choose a new expiry")
	}
	if next == domain.GrantActive {
		if g.SuspendReason != "owner" || (g.Key.SessionDigest == "" && g.ProcessID != s.ProcessID) {
			return errors.New("session identity needs a new owner grant")
		}
		if s.Agent != nil {
			a, live := s.Agent(g.Key)
			if !live || a.Status == domain.StatusExited {
				return errors.New("shared agent is unavailable")
			}
		}
	}
	changed.SuspendReason = "owner"
	s.state.Grants[id] = changed
	s.cancelLocked(id)
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		// Revocation remains denied in memory. An unsuccessful enabling
		// change is also denied until its durable retry succeeds.
		if next == domain.GrantActive {
			changed.State = domain.GrantSuspended
			s.state.Grants[id] = changed
		}
		return errors.New("durable permission change failed; access denied locally; retry before restarting")
	}
	s.log.Info("share state changed", slog.String("grant_id", id), slog.Uint64("revision", changed.Revision), slog.String("state", string(next)))
	return nil
}

func (s *Sharing) HideRecipient(ctx context.Context, id int64, hidden bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return ErrSharingUnavailable
	}
	r, ok := s.state.Recipients[id]
	if !ok {
		return errors.New("recipient not found")
	}
	old := r
	r.Hidden = hidden
	s.state.Recipients[id] = r
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		s.state.Recipients[id] = old
		return err
	}
	return nil
}

func (s *Sharing) cancelLocked(id string) {
	for _, cancel := range s.inflight[id] {
		cancel()
	}
	delete(s.inflight, id)
}

// Begin is the dispatch linearization point. Revocation and Begin serialize
// under the policy lock; the network request uses the returned context after
// releasing it. Queued retries require a fresh Begin.
func (s *Sharing) Begin(ctx context.Context, o domain.ShareOrigin, action domain.ShareAction) (context.Context, context.CancelFunc, domain.AccessDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deny := domain.AccessDecision{Reason: domain.AccessInactive}
	if s.disabled || s.Agent == nil || ctx.Err() != nil || s.BotID != s.state.BotID {
		return nil, nil, deny
	}
	g, ok := s.state.Grants[o.GrantID]
	if !ok {
		return nil, nil, deny
	}
	a, live := s.Agent(g.Key)
	if !live || a.Status == domain.StatusExited {
		return nil, nil, deny
	}
	// Wall-clock authorization is replaced by the injected clock at wiring.
	now := s.now()
	d := domain.AuthorizeShare(g, s.state.Mirrors[g.ID], o, a.Key, action, now)
	if !d.Allowed {
		return nil, nil, d
	}
	callCtx, cancel := context.WithCancel(ctx)
	token := rand.Text()
	if s.inflight[g.ID] == nil {
		s.inflight[g.ID] = map[string]context.CancelFunc{}
	}
	s.inflight[g.ID][token] = cancel
	release := func() { cancel(); s.mu.Lock(); delete(s.inflight[g.ID], token); s.mu.Unlock() }
	return callCtx, release, d
}

func (s *Sharing) Guard(o domain.ShareOrigin, action domain.ShareAction) domain.DispatchGuard {
	return func(ctx context.Context) (context.Context, context.CancelFunc, error) {
		callCtx, done, d := s.Begin(ctx, o, action)
		if !d.Allowed {
			s.log.Debug("share dispatch denied", slog.String("reason", string(d.Reason)))
			return nil, nil, errors.New("share access denied: " + string(d.Reason))
		}
		return callCtx, done, nil
	}
}

func (s *Sharing) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// PrepareMirror persists creation intent before any Telegram request.
func (s *Sharing) PrepareMirror(ctx context.Context, id string, revision uint64, now time.Time) (domain.MirrorBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok || g.Revision != revision || g.State != domain.GrantPending {
		return domain.MirrorBinding{}, errors.New("grant is not pending")
	}
	m, exists := s.state.Mirrors[id]
	if exists && m.Creating {
		return m, errors.New("topic creation outcome unknown; repair required")
	}
	if exists && m.Address.ThreadID > 0 {
		return m, nil
	}
	m = domain.MirrorBinding{GrantID: id, Address: domain.TopicAddress{ChatID: g.RecipientID}, Creating: true, NavigationID: rand.Text()}
	s.state.Mirrors[id] = m
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		return m, err
	}
	return m, nil
}

func (s *Sharing) ActivateMirror(ctx context.Context, id string, revision uint64, topic domain.Topic, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok || g.Revision != revision || g.State != domain.GrantPending {
		return errors.New("grant changed during creation")
	}
	m := s.state.Mirrors[id]
	m.Address = domain.TopicAddress{ChatID: g.RecipientID, ThreadID: topic.ThreadID}
	m.Creating = false
	g.State = domain.GrantActive
	g.Revision++
	g.ActivatedAt = now
	s.state.Mirrors[id] = m
	s.state.Grants[id] = g
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		g.State = domain.GrantNeedsRepair
		s.state.Grants[id] = g
		return err
	}
	return nil
}

func (s *Sharing) Origin(id string) (domain.ShareOrigin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok {
		return domain.ShareOrigin{}, false
	}
	m := s.state.Mirrors[id]
	return domain.ShareOrigin{ActorID: g.RecipientID, Address: m.Address, GrantID: id, Revision: g.Revision, Key: g.Key, ProcessID: s.ProcessID}, true
}

// Lifecycle only revives an exited grant on independently verified session
// continuity. Name/cwd matches and incomplete identity require owner approval.
func (s *Sharing) Lifecycle(ctx context.Context, e AgentEvent, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || s.BotID != s.state.BotID {
		return nil
	}
	changed := false
	for id, g := range s.state.Grants {
		if g.Key.PaneID != e.Agent.PaneID || (g.State != domain.GrantActive && !(g.State == domain.GrantSuspended && g.SuspendReason == "exited")) {
			continue
		}
		if e.Kind == AgentGone {
			g.State = domain.GrantSuspended
			g.SuspendReason = "exited"
			g.ExitedAt = now
		} else if g.Key != e.Agent.Key || (g.Key.SessionDigest == "" && g.ProcessID != s.ProcessID) {
			if g.Key.SameSession(e.Agent.Key) {
				g.Key = e.Agent.Key
				g.State = domain.GrantActive
				g.SuspendReason = ""
				g.ExitedAt = time.Time{}
			} else {
				g.State = domain.GrantSuspended
				g.SuspendReason = "identity"
			}
		} else if g.State == domain.GrantSuspended {
			if g.Key.SessionDigest == "" {
				continue
			}
			g.State = domain.GrantActive
			g.SuspendReason = ""
			g.ExitedAt = time.Time{}
		} else {
			continue
		}
		if g.Revision == math.MaxUint64 {
			s.disabled = true
			return ErrSharingUnavailable
		}
		g.Revision++
		s.state.Grants[id] = g
		s.cancelLocked(id)
		changed = true
	}
	if !changed {
		return nil
	}
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		for id, g := range s.state.Grants {
			if g.Key.PaneID == e.Agent.PaneID {
				g.State = domain.GrantSuspended
				g.SuspendReason = "persistence"
				s.state.Grants[id] = g
			}
		}
		return err
	}
	return nil
}

// RepairMirror is an explicit owner retry after an ambiguous create outcome.
// Telegram cannot enumerate the missing topic, so the owner confirms the
// possible orphan before this method forgets its pending intent.
func (s *Sharing) RepairMirror(ctx context.Context, id string, revision uint64, now time.Time) (domain.ShareGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok || g.Revision != revision || (g.State != domain.GrantNeedsRepair && g.State != domain.GrantPending) {
		return g, errors.New("mirror does not need repair")
	}
	if g.Revision == math.MaxUint64 {
		return g, ErrSharingUnavailable
	}
	g.State = domain.GrantPending
	g.Revision++
	delete(s.state.Mirrors, id)
	s.state.Grants[id] = g
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		return domain.ShareGrant{}, err
	}
	return g, nil
}

func (s *Sharing) MissingMirror(ctx context.Context, id string, revision uint64, now time.Time) (domain.ShareGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok || g.Revision != revision || g.State != domain.GrantActive || g.Revision == math.MaxUint64 {
		return g, errors.New("stale missing mirror")
	}
	g.State = domain.GrantPending
	g.Revision++
	s.cancelLocked(id)
	delete(s.state.Mirrors, id)
	s.state.Grants[id] = g
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		return domain.ShareGrant{}, err
	}
	return g, nil
}

func (s *Sharing) Preferences(ctx context.Context, o domain.ShareOrigin, p domain.MirrorPreferences, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[o.GrantID]
	m := s.state.Mirrors[o.GrantID]
	if s.disabled || !ok || g.State != domain.GrantActive || g.Revision != o.Revision || g.RecipientID != o.ActorID || m.Address != o.Address {
		return errors.New("mirror access changed")
	}
	if p.Display != "" && p.Display != "screen" && p.Display != "reply" && p.Display != "formatted" {
		return errors.New("display must be screen, reply or formatted")
	}
	if p.Fold < 0 || p.Fold > 200 {
		return errors.New("fold must be between 0 and 200")
	}
	p.Alias = boundedContact(p.Alias, 128)
	old := m
	m.Preferences = p
	s.state.Mirrors[o.GrantID] = m
	s.dirty = true
	if err := s.saveLocked(ctx, now); err != nil {
		s.state.Mirrors[o.GrantID] = old
		return err
	}
	return nil
}

// ServiceTopic records zero thread as a durable pending creation intent.
func (s *Sharing) ServiceTopic(ctx context.Context, recipient int64, address domain.TopicAddress, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || s.BotID != s.state.BotID {
		return ErrSharingUnavailable
	}
	if _, ok := s.state.Recipients[recipient]; !ok || address.ChatID != recipient || address.ThreadID < 0 {
		return errors.New("invalid service topic")
	}
	s.state.ServiceTopics[recipient] = address
	s.dirty = true
	return s.saveLocked(ctx, now)
}

func (s *Sharing) Dashboard(ctx context.Context, recipient int64, address domain.MessageAddress, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || s.BotID != s.state.BotID {
		return ErrSharingUnavailable
	}
	if address.MessageID == 0 {
		delete(s.state.Dashboards, recipient)
	} else {
		s.state.Dashboards[recipient] = address
	}
	s.dirty = true
	return s.saveLocked(ctx, now)
}

// Expire publishes denial and cancels pending work before saving the sweep.
// Dispatch checks time independently, including between sweep ticks.
func (s *Sharing) Expire(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return ErrSharingUnavailable
	}
	changed := false
	for id, g := range s.state.Grants {
		if g.State != domain.GrantActive && g.State != domain.GrantSuspended && g.State != domain.GrantPending {
			continue
		}
		if g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt) {
			continue
		}
		next, err := g.Transition(domain.GrantExpired)
		if err != nil {
			continue
		}
		s.state.Grants[id] = next
		s.cancelLocked(id)
		s.dirty = true
		changed = true
		s.log.Info("share expired", "grant_id", id, "revision", next.Revision)
	}
	if changed {
		return s.saveLocked(ctx, now)
	}
	return nil
}

// BindingGuard admits only the expected durable policy revision for lifecycle
// calls. It also covers pending creation, where normal output is still denied.
func (s *Sharing) BindingGuard(id string, revision uint64) domain.DispatchGuard {
	return func(ctx context.Context) (context.Context, context.CancelFunc, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		g, ok := s.state.Grants[id]
		if s.disabled || !ok || g.Revision != revision || s.BotID != s.state.BotID || ctx.Err() != nil {
			return nil, nil, errors.New("private binding changed")
		}
		callCtx, cancel := context.WithCancel(ctx)
		token := rand.Text()
		if s.inflight[id] == nil {
			s.inflight[id] = map[string]context.CancelFunc{}
		}
		s.inflight[id][token] = cancel
		release := func() { cancel(); s.mu.Lock(); delete(s.inflight[id], token); s.mu.Unlock() }
		return callCtx, release, nil
	}
}

// RecordOutput coalesces presentation state; it never stores message content.
func (s *Sharing) RecordOutput(o domain.ShareOrigin, hash string, keyboard int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[o.GrantID]
	if s.disabled || !ok || g.Revision != o.Revision {
		return
	}
	m := s.state.Mirrors[o.GrantID]
	m.LastOutput, m.KeyboardMessageID = hash, keyboard
	s.state.Mirrors[o.GrantID] = m
	s.dirty = true
}

// ForgetMirror follows confirmed deletion; stale callbacks keep their old
// revision, while a later grant starts a fresh durable creation intent.
func (s *Sharing) ForgetMirror(ctx context.Context, id string, revision uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok || g.Revision != revision || g.State != domain.GrantRevoked {
		return errors.New("mirror changed")
	}
	delete(s.state.Mirrors, id)
	s.dirty = true
	return s.saveLocked(ctx, now)
}

func (s *Sharing) RepairService(ctx context.Context, recipient int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.ServiceTopics[recipient]
	if s.disabled || !ok || a.ThreadID != 0 {
		return errors.New("service topic does not need repair")
	}
	delete(s.state.ServiceTopics, recipient)
	delete(s.state.Dashboards, recipient)
	s.dirty = true
	return s.saveLocked(ctx, now)
}

func (s *Sharing) RetireMirror(ctx context.Context, id string, revision uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if s.disabled || !ok || g.Revision != revision || (g.SuspendReason != "exited" && g.SuspendReason != "retention") {
		return errors.New("exited mirror changed")
	}
	if g.State != domain.GrantRevoked {
		next, err := g.Transition(domain.GrantRevoked)
		if err != nil {
			return err
		}
		g = next
	}
	g.SuspendReason = "retention"
	s.state.Grants[id] = g
	s.cancelLocked(id)
	s.dirty = true
	return s.saveLocked(ctx, now)
}
