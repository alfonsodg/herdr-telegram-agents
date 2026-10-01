package compose

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestTightenPermissionsOwnedPaths: the daemon start tightens the state and
// config directories and the plugin's own files there, and leaves a file it
// does not own as it is.
func TestTightenPermissionsOwnedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	base := t.TempDir()
	env := PluginEnv{StateDir: filepath.Join(base, "state"), ConfigDir: filepath.Join(base, "config")}
	paths := map[string]os.FileMode{
		env.StateDir:  0o755,
		env.ConfigDir: 0o755,
	}
	for dir := range paths {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]os.FileMode{
		filepath.Join(env.ConfigDir, "config.json"):  0o600,
		filepath.Join(env.ConfigDir, "options.json"): 0o600,
		filepath.Join(env.StateDir, "mapping.json"):  0o600,
		filepath.Join(env.StateDir, "daemon.log.2"):  0o600,
		filepath.Join(env.StateDir, "notes.txt"):     0o644, // not the plugin's
	}
	for path := range files {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for path := range paths {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path := range files {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n := TightenPermissions(env, nil); n != 6 {
		t.Errorf("tightened = %d, want 6", n)
	}
	for path := range paths {
		files[path] = 0o700
	}
	for path, want := range files {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}
