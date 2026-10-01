package domain

import (
	"errors"
	"math"
	"time"
)

// TopicAddress and MessageAddress include the chat: Telegram identifiers
// are unique only inside a chat.
type TopicAddress struct {
	ChatID   int64 `json:"chat_id"`
	ThreadID int   `json:"thread_id"`
}

type MessageAddress struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int   `json:"message_id"`
}

// Recipient contains contact metadata only, never the registration message.
type Recipient struct {
	ID          int64     `json:"id"`
	ChatID      int64     `json:"chat_id"`
	Name        string    `json:"name"`
	Username    string    `json:"username,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Unavailable bool      `json:"unavailable,omitempty"`
	Hidden      bool      `json:"hidden,omitempty"`
}

type GrantState string

const (
	GrantPending     GrantState = "pending"
	GrantActive      GrantState = "active"
	GrantSuspended   GrantState = "suspended"
	GrantRevoked     GrantState = "revoked"
	GrantExpired     GrantState = "expired"
	GrantNeedsRepair GrantState = "needs-repair"
)

func (s GrantState) Valid() bool {
	switch s {
	case GrantPending, GrantActive, GrantSuspended, GrantRevoked, GrantExpired, GrantNeedsRepair:
		return true
	}
	return false
}

type ShareRole string

const (
	ShareRead    ShareRole = "read"
	ShareControl ShareRole = "control"
)

// ShareGrant binds permission to an exact session and a monotonically
// increasing revision. Each change invalidates previously admitted work.
type ShareGrant struct {
	SuspendReason  string     `json:"suspend_reason,omitempty"`
	ID             string     `json:"id"`
	RecipientID    int64      `json:"recipient_id"`
	Key            Key        `json:"key"`
	Revision       uint64     `json:"revision"`
	State          GrantState `json:"state"`
	Role           ShareRole  `json:"role"`
	RepositoryRead bool       `json:"repository_read,omitempty"`
	CloseAgent     bool       `json:"close_agent,omitempty"`
	LocalFocus     bool       `json:"local_focus,omitempty"`
	ExitedAt       time.Time  `json:"exited_at,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at,omitempty"`
	ActivatedAt    time.Time  `json:"activated_at"`
	HistoryCursor  uint64     `json:"history_cursor"`
	// ProcessID prevents incomplete session identity surviving a restart.
	ProcessID string `json:"process_id,omitempty"`
}

var ErrGrantTransition = errors.New("invalid share grant transition")

// Transition never revives a revoked revision. Reapproval starts a pending
// intent; the reconciler activates only after a destination is durable.
func (g ShareGrant) Transition(next GrantState) (ShareGrant, error) {
	if !g.State.Valid() || !next.Valid() || g.Revision == math.MaxUint64 {
		return g, ErrGrantTransition
	}
	allowed := false
	switch next {
	case GrantRevoked:
		allowed = true
	case GrantPending:
		allowed = g.State != GrantActive
	case GrantActive:
		allowed = g.State == GrantPending || g.State == GrantSuspended
	case GrantSuspended, GrantExpired:
		allowed = g.State == GrantActive || g.State == GrantPending || g.State == GrantSuspended
	case GrantNeedsRepair:
		allowed = g.State == GrantPending || g.State == GrantActive
	}
	if !allowed {
		return g, ErrGrantTransition
	}
	g.State = next
	g.Revision++
	return g, nil
}

// MirrorPreferences are local presentation choices, never permissions.
type MirrorPreferences struct {
	Alias    string `json:"alias,omitempty"`
	Paused   bool   `json:"paused,omitempty"`
	Silent   bool   `json:"silent,omitempty"`
	Display  string `json:"display,omitempty"`
	Fold     int    `json:"fold,omitempty"`
	Metadata bool   `json:"metadata,omitempty"`
}

type MirrorBinding struct {
	GrantID           string            `json:"grant_id"`
	Address           TopicAddress      `json:"address"`
	Preferences       MirrorPreferences `json:"preferences"`
	KeyboardMessageID int               `json:"keyboard_message_id,omitempty"`
	LastOutput        string            `json:"last_output,omitempty"`
	NavigationID      string            `json:"navigation_id,omitempty"`
	// Creating is persisted before createForumTopic. Unknown outcomes need
	// explicit repair; retrying creation could duplicate a private topic.
	Creating bool `json:"creating,omitempty"`
}

const SharingVersion = 1
const (
	MaxRecipients        = 10000
	MaxShareGrants       = 2000
	MaxRecipientName     = 128
	MaxRecipientUsername = 64
)

// SharingState is independent of owner mapping.json. No load failure is
// allowed to replace an existing file with an empty state.
type SharingState struct {
	Version       int                      `json:"version"`
	BotID         int64                    `json:"bot_id"`
	Revision      uint64                   `json:"revision"`
	UpdateID      int64                    `json:"update_id"`
	Recipients    map[int64]Recipient      `json:"recipients"`
	Grants        map[string]ShareGrant    `json:"grants"`
	Mirrors       map[string]MirrorBinding `json:"mirrors"`
	ServiceTopics map[int64]TopicAddress   `json:"service_topics"`
	Dashboards    map[int64]MessageAddress `json:"dashboards"`
}

func NewSharingState() SharingState {
	return SharingState{Version: SharingVersion, Recipients: map[int64]Recipient{}, Grants: map[string]ShareGrant{}, Mirrors: map[string]MirrorBinding{}, ServiceTopics: map[int64]TopicAddress{}, Dashboards: map[int64]MessageAddress{}}
}

// Validate rejects unrecognized policy instead of guessing how to enforce it.
func (s SharingState) Validate() error {
	invalid := errors.New("invalid sharing state")
	if s.Version != SharingVersion || s.BotID < 0 || len(s.Recipients) > MaxRecipients || len(s.Grants) > MaxShareGrants || len(s.Mirrors) > MaxShareGrants {
		return invalid
	}
	if s.Recipients == nil || s.Grants == nil || s.Mirrors == nil || s.ServiceTopics == nil || s.Dashboards == nil {
		return invalid
	}
	for id, r := range s.Recipients {
		if id <= 0 || r.ID != id || r.ChatID != id || len([]rune(r.Name)) > MaxRecipientName || len([]rune(r.Username)) > MaxRecipientUsername || r.FirstSeen.IsZero() || r.LastSeen.Before(r.FirstSeen) {
			return invalid
		}
	}
	for id, g := range s.Grants {
		if s.BotID == 0 || id == "" || len(id) > 64 || id != g.ID || g.Revision == 0 || !g.State.Valid() || (g.Role != ShareRead && g.Role != ShareControl) || g.Key.PaneID == "" || g.Key.TerminalID == "" {
			return invalid
		}
		if _, ok := s.Recipients[g.RecipientID]; !ok {
			return invalid
		}
		if g.State == GrantActive {
			m, ok := s.Mirrors[id]
			if !ok || m.Creating || m.Address.ThreadID <= 0 {
				return invalid
			}
		}
	}
	addresses := map[TopicAddress]bool{}
	for id, m := range s.Mirrors {
		g, ok := s.Grants[id]
		if !ok || m.GrantID != id || m.Address.ChatID != g.RecipientID || m.Address.ThreadID < 0 || (m.Address.ThreadID == 0 && !m.Creating) {
			return invalid
		}
		if m.Address.ThreadID > 0 {
			if addresses[m.Address] {
				return invalid
			}
			addresses[m.Address] = true
		}
	}
	for id, a := range s.ServiceTopics {
		if _, ok := s.Recipients[id]; !ok || a.ChatID != id || a.ThreadID < 0 || addresses[a] {
			return invalid
		}
		addresses[a] = true
	}
	for id, a := range s.Dashboards {
		if _, ok := s.ServiceTopics[id]; !ok || a.ChatID != id || a.MessageID <= 0 {
			return invalid
		}
	}
	return nil
}

// Clone isolates policy snapshots from future mutations by the single writer.
func (s SharingState) Clone() SharingState {
	n := s
	n.Recipients = cloneSharingMap(s.Recipients)
	n.Grants = cloneSharingMap(s.Grants)
	n.Mirrors = cloneSharingMap(s.Mirrors)
	n.ServiceTopics = cloneSharingMap(s.ServiceTopics)
	n.Dashboards = cloneSharingMap(s.Dashboards)
	return n
}

func cloneSharingMap[K comparable, V any](src map[K]V) map[K]V {
	dst := make(map[K]V, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// MatchesBot must pass before any persisted private address is used. Keep
// mismatched state quarantined for explicit repair; never rewrite its IDs.
func (s SharingState) MatchesBot(id BotIdentity) bool {
	return id.ID > 0 && (s.BotID == 0 || s.BotID == id.ID)
}

// ErrRegistrationBusy refuses a new contact because first contacts arrive
// faster than the per-minute cap; like capacity it never blocks polling.
var ErrRegistrationBusy = errors.New("new private contacts arrive too fast")

// ErrRecipientCapacity rejects a new contact without blocking owner polling.
var ErrRecipientCapacity = errors.New("private recipient capacity reached")

// ErrSharingStateFull reports that a snapshot would exceed the store's size
// cap. It is a capacity condition, not a write failure: retrying the same
// snapshot can never succeed.
var ErrSharingStateFull = errors.New("sharing state size limit reached")
