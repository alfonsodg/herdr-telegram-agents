package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// SharingStore never repairs or resets corrupt policy automatically. A failed
// load latches writes off for this instance, preserving evidence for repair.
type SharingStore struct {
	mu       sync.Mutex
	path     string
	log      *slog.Logger
	blocked  bool
	loaded   bool
	revision uint64
}

const maxSharingBytes = 8 << 20

// SharingFileName is the private-sharing policy file under the state dir.
const SharingFileName = "sharing.json"

func NewSharingStore(dir string, log *slog.Logger) *SharingStore {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SharingStore{path: filepath.Join(dir, SharingFileName), log: log}
}

func (s *SharingStore) Load(ctx context.Context) (domain.SharingState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.SharingState{}, err
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		return domain.NewSharingState(), nil
	}
	if err != nil {
		s.blocked = true
		return domain.SharingState{}, fmt.Errorf("open sharing state: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSharingBytes+1))
	var st domain.SharingState
	if err == nil && len(data) > maxSharingBytes {
		err = errors.New("sharing state exceeds size limit")
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		err = dec.Decode(&st)
		if err == nil {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = errors.New("trailing sharing state data")
			}
		}
	}
	if err == nil {
		err = st.Validate()
	}
	if err != nil {
		s.blocked = true
		s.log.Warn("sharing state invalid; guest access disabled")
		// Decode errors can include stored private strings. Do not return them.
		return domain.SharingState{}, errors.New("sharing state invalid or unsupported; repair required")
	}
	s.loaded = true
	s.revision = st.Revision
	s.log.Info("sharing state loaded", slog.Int("version", st.Version), slog.Int("recipients", len(st.Recipients)), slog.Int("grants", len(st.Grants)))
	return st, nil
}

func (s *SharingStore) Save(ctx context.Context, st domain.SharingState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.blocked || !s.loaded {
		return errors.New("sharing state must be loaded successfully before saving")
	}
	if err := st.Validate(); err != nil {
		return err
	}
	if st.Revision <= s.revision {
		return errors.New("stale sharing state revision")
	}
	// No HTML escaping: the file is never embedded in markup, and escaping
	// made each '<', '>' or '&' in a contact name cost six bytes. Readers
	// accept both forms, so older binaries load these files unchanged.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		return errors.New("encode sharing state failed")
	}
	// Encode ends with the newline, so this is the same bound Load applies.
	if buf.Len() > maxSharingBytes {
		s.log.Warn("[FIX] sharing state size limit reached; save refused as capacity", slog.Int("bytes", buf.Len()), slog.Int("limit", maxSharingBytes))
		return fmt.Errorf("save sharing state: %w", domain.ErrSharingStateFull)
	}
	if err := writeAtomic(s.path, buf.Bytes(), 0o600); err != nil {
		s.log.Warn("sharing state save failed", slog.Uint64("revision", st.Revision))
		return fmt.Errorf("save sharing state: %w", err)
	}
	s.revision = st.Revision
	s.log.Debug("sharing state saved", slog.Uint64("revision", st.Revision))
	return nil
}
