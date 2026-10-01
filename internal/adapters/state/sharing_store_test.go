package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/adapters/state"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

func TestSharingStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := state.NewSharingStore(dir, nil)
	s, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.BotID = 42
	s.Revision = 1
	s.Recipients[7] = domain.Recipient{ID: 7, ChatID: 7, Name: "Contact", FirstSeen: time.Unix(100, 0).UTC(), LastSeen: time.Unix(100, 0).UTC()}
	s.Grants["g"] = domain.ShareGrant{ID: "g", RecipientID: 7, Key: domain.Key{PaneID: "p", TerminalID: "t", SessionDigest: "digest"}, Revision: 1, Role: domain.ShareRead, State: domain.GrantPending}
	s.Mirrors["g"] = domain.MirrorBinding{GrantID: "g", Address: domain.TopicAddress{ChatID: 7}, Creating: true}
	if err := store.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	st, err := state.NewSharingStore(dir, nil).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.BotID != 42 || !st.Mirrors["g"].Creating || st.Grants["g"].Revision != 1 || st.Recipients[7].Name != "Contact" {
		t.Fatal("roundtrip lost durable intent")
	}
	if st.MatchesBot(domain.BotIdentity{ID: 43}) {
		t.Fatal("foreign bot accepted")
	}
	info, err := os.Stat(filepath.Join(dir, "sharing.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not expose Unix permission bits through FileMode.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions %v", info.Mode())
	}
	if err := store.Save(ctx, s); err == nil {
		t.Fatal("stale revision accepted")
	}
	copy := st.Clone()
	delete(copy.Recipients, 7)
	if len(st.Recipients) != 1 {
		t.Fatal("snapshot aliases writer")
	}
}

func TestSharingStoreRejectsCorruption(t *testing.T) {
	for _, data := range []string{`{`, `{"version":900}`, `{"version":1}`, `null`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "sharing.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		store := state.NewSharingStore(dir, nil)
		if _, err := store.Load(context.Background()); err == nil {
			t.Fatal("corrupt state accepted")
		}
		s := domain.NewSharingState()
		s.Revision = 1
		if err := store.Save(context.Background(), s); err == nil {
			t.Fatal("corrupt state overwritten")
		}
		got, _ := os.ReadFile(path)
		if string(got) != data {
			t.Fatal("evidence changed")
		}
	}
}

func TestSharingStoreWriteFailure(t *testing.T) {
	dir := t.TempDir()
	store := state.NewSharingStore(dir, nil)
	ctx := context.Background()
	s, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sharing.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.Revision = 1
	if err := store.Save(ctx, s); err == nil {
		t.Fatal("write failure ignored")
	}
	if err := os.Remove(filepath.Join(dir, "sharing.json")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, s); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// fullDirectory returns a sharing state at MaxRecipients, every name made
// of the longest allowed run of one rune.
func fullDirectory(name rune) domain.SharingState {
	s := domain.NewSharingState()
	s.Revision = 1
	at := time.Unix(100, 0).UTC()
	long := strings.Repeat(string(name), domain.MaxRecipientName)
	for id := int64(1); id <= domain.MaxRecipients; id++ {
		s.Recipients[id] = domain.Recipient{ID: id, ChatID: id, Name: long, FirstSeen: at, LastSeen: at}
	}
	return s
}

// TestSharingStoreFitsFullDirectoryOfMarkupNames: a full directory whose
// names are all '<' fits the size cap. With HTML escaping each '<' took six
// bytes and Save refused the file at about 8,500 recipients.
func TestSharingStoreFitsFullDirectoryOfMarkupNames(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := state.NewSharingStore(dir, nil)
	if _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, fullDirectory('<')); err != nil {
		t.Fatalf("full directory refused: %v", err)
	}
	st, err := state.NewSharingStore(dir, nil).Load(ctx)
	if err != nil || len(st.Recipients) != domain.MaxRecipients || st.Recipients[1].Name != strings.Repeat("<", domain.MaxRecipientName) {
		t.Fatalf("roundtrip: %v", err)
	}
}

// TestSharingStoreSizeLimitIsCapacity: a state over the byte cap is a
// capacity condition callers can tell apart from a write failure.
func TestSharingStoreSizeLimitIsCapacity(t *testing.T) {
	ctx := context.Background()
	store := state.NewSharingStore(t.TempDir(), nil)
	if _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	// U+2028 is escaped to six bytes whatever the encoder settings.
	if err := store.Save(ctx, fullDirectory(' ')); !errors.Is(err, domain.ErrSharingStateFull) {
		t.Fatalf("err = %v, want ErrSharingStateFull", err)
	}
}
