package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/app"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
	"github.com/permgps/herdr-telegram-agents/internal/testkit"
)

func TestSharingRegistrationPersistence(t *testing.T) {
	ctx := context.Background()
	store := testkit.NewMemSharingStore()
	s := app.NewSharing(ctx, store, nil)
	now := time.Unix(1000, 0)
	first, err := s.Register(ctx, 7, 7, "name", "", now)
	if !first || err != nil {
		t.Fatalf("first=%v err=%v", first, err)
	}
	st, _ := store.Load(ctx)
	revision := st.Revision
	if len(st.Recipients) != 1 {
		t.Fatal("first contact not durable")
	}
	first, err = s.Register(ctx, 7, 7, "new name", "handle", now.Add(time.Second))
	if first || err != nil {
		t.Fatal("refresh treated as first contact")
	}
	st, _ = store.Load(ctx)
	if st.Revision != revision || st.Recipients[7].Name != "name" {
		t.Fatal("refresh not coalesced")
	}
	if err := s.Flush(ctx, now.Add(31*time.Second), false); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Load(ctx)
	if st.Recipients[7].Name != "new name" {
		t.Fatal("refresh not flushed")
	}
	store.Fail(errors.New("disk full"))
	if first, err := s.Register(ctx, 8, 8, "other", "", now); err == nil || first {
		t.Fatal("failed first contact acknowledged")
	}
	snapshot, _ := s.Snapshot()
	if _, ok := snapshot.Recipients[8]; ok {
		t.Fatal("failed contact retained as registered")
	}
}

func TestSharingCorruptionDisablesGuests(t *testing.T) {
	store := testkit.NewMemSharingStore()
	store.Fail(errors.New("corrupt"))
	s := app.NewSharing(context.Background(), store, nil)
	if _, ok := s.Snapshot(); ok {
		t.Fatal("corrupt policy enabled")
	}
	if _, err := s.Register(context.Background(), 7, 7, "name", "", time.Unix(1000, 0)); !errors.Is(err, app.ErrSharingUnavailable) {
		t.Fatal(err)
	}
}

// TestSharingRegistrationStateFullIsCapacity: a first contact that would push
// sharing.json past its size cap is refused as capacity, which the admission
// path acknowledges. A generic error left the update unacknowledged and every
// later getUpdates refetched and failed on it, owner updates included.
func TestSharingRegistrationStateFullIsCapacity(t *testing.T) {
	ctx := context.Background()
	store := testkit.NewMemSharingStore()
	s := app.NewSharing(ctx, store, nil)
	store.Fail(fmt.Errorf("save: %w", domain.ErrSharingStateFull))
	first, err := s.Register(ctx, 8, 8, strings.Repeat("<", domain.MaxRecipientName), "", time.Unix(1000, 0))
	if first || !errors.Is(err, domain.ErrRecipientCapacity) {
		t.Fatalf("first=%v err=%v, want ErrRecipientCapacity", first, err)
	}
	snapshot, _ := s.Snapshot()
	if _, ok := snapshot.Recipients[8]; ok {
		t.Fatal("refused contact retained as registered")
	}
}

// TestSharingRegistrationRate: each first contact is a durable write on the
// polling path. A flood of new accounts is capped per minute; the excess is
// refused (not registered, not written) and polling goes on.
func TestSharingRegistrationRate(t *testing.T) {
	ctx := context.Background()
	store := testkit.NewMemSharingStore()
	s := app.NewSharing(ctx, store, nil)
	now := time.Unix(1000, 0)
	for id := int64(1); id <= app.MaxNewContactsPerMinute; id++ {
		if _, err := s.Register(ctx, id, id, "n", "", now); err != nil {
			t.Fatalf("contact %d: %v", id, err)
		}
	}
	saved, _ := store.Load(ctx)
	if _, err := s.Register(ctx, 999, 999, "n", "", now); !errors.Is(err, domain.ErrRegistrationBusy) {
		t.Fatalf("contact over the rate = %v", err)
	}
	if after, _ := store.Load(ctx); after.Revision != saved.Revision {
		t.Fatal("refused contact was written")
	}
	// Known contacts are never refused.
	if _, err := s.Register(ctx, 1, 1, "again", "", now); err != nil {
		t.Fatalf("known contact refused: %v", err)
	}
	if _, err := s.Register(ctx, 999, 999, "n", "", now.Add(time.Minute)); err != nil {
		t.Fatalf("rate did not recover: %v", err)
	}
}
