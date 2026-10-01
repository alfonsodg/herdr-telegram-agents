package state_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/permgps/herdr-telegram-agents/internal/adapters/state"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// TestStateFilesArePrivate: the mapping holds working directories and topic
// names, the pid file the daemon's pid; the state directory is created for
// the user alone.
func TestStateFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "state")
	m := state.NewMappingStore(dir, nil)
	if err := m.Save(context.Background(), domain.NewMapping(-100)); err != nil {
		t.Fatal(err)
	}
	p := state.NewPidFile(filepath.Join(t.TempDir(), "pid"), nil, nil)
	if err := p.Acquire(42); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		dir:                                0o700,
		filepath.Join(dir, "mapping.json"): 0o600,
		p.Path():                           0o600,
		filepath.Dir(p.Path()):             0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

// TestTightenPermissions: MkdirAll and writeAtomic never change an existing
// directory, and installs from older builds left the state and config dirs
// at 0755 and some files at 0644. The daemon tightens what the plugin owns
// at start; symlinks, missing paths and already private ones are left alone.
func TestTightenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	base := t.TempDir()
	loose := filepath.Join(base, "state")
	tight := filepath.Join(base, "config")
	for dir, mode := range map[string]os.FileMode{loose: 0o755, tight: 0o700} {
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]os.FileMode{
		filepath.Join(loose, "mapping.json"): 0o644,
		filepath.Join(tight, "config.json"):  0o640,
		filepath.Join(tight, "options.json"): 0o600,
	}
	for path, mode := range files {
		if err := os.WriteFile(path, []byte("{}"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink in the state dir points at a file the plugin does not own.
	foreign := filepath.Join(base, "foreign.json")
	if err := os.WriteFile(foreign, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(loose, "sharing.json")
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for path := range files {
		paths = append(paths, path)
	}
	paths = append(paths, link, filepath.Join(loose, "missing.json"))
	n := state.TightenPermissions([]string{loose, tight, filepath.Join(base, "nodir")}, paths, nil)
	if n != 3 {
		t.Errorf("tightened = %d, want 3 (state dir, mapping.json, config.json)", n)
	}
	for path, want := range map[string]os.FileMode{
		loose:                                0o700,
		tight:                                0o700,
		filepath.Join(loose, "mapping.json"): 0o600,
		filepath.Join(tight, "config.json"):  0o600,
		filepath.Join(tight, "options.json"): 0o600,
		foreign:                              0o644,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}
