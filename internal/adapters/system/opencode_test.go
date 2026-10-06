package system

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeOpenCode writes a shell stand-in for the opencode binary.
func fakeOpenCode(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in is Unix only")
	}
	script := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestOpenCodeExporterRunsExport(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = fakeOpenCode(t, `if [ "$1" = --version ]; then echo 'opencode 1.25.0'; exit 0; fi
[ "$1" = export ] && [ "$2" = ses_abc ] && [ $# -eq 2 ] || exit 3
echo '{"messages":[]}'
`)
	out, err := e.Export(context.Background(), "ses_abc")
	if err != nil || strings.TrimSpace(string(out)) != `{"messages":[]}` {
		t.Fatalf("Export = %q, %v", out, err)
	}
}

func TestOpenCodeExporterRunsV2SessionExport(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = fakeOpenCode(t, `[ "$1" = session ] && [ "$2" = export ] && [ "$3" = ses_abc ] && [ $# -eq 3 ] || exit 3
echo '{"messages":[]}'
`)
	out, err := e.Export(context.Background(), "ses_abc")
	if err != nil || strings.TrimSpace(string(out)) != `{"messages":[]}` {
		t.Fatalf("Export = %q, %v", out, err)
	}
}

func TestOpenCodeExporterV2FailureDoesNotStartLegacyCommand(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = fakeOpenCode(t, `if [ "$1" = session ] && [ "$2" = export ]; then exit 4; fi
if [ "$1" = --version ]; then echo 'opencode v2.0.18'; exit 0; fi
echo 'unexpected legacy command'
exit 0
`)
	out, err := e.Export(context.Background(), "ses_abc")
	if out != nil || err == nil || !strings.Contains(err.Error(), "exit code 4") {
		t.Fatalf("v2 failure = %q, %v", out, err)
	}
}

func TestOpenCodeExporterDiscardsStderr(t *testing.T) {
	for _, exit := range []string{"0", "1"} {
		t.Run(exit, func(t *testing.T) {
			var logs bytes.Buffer
			e := NewOpenCodeExporter(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			e.bin = fakeOpenCode(t, "printf '{\"messages\":[]}'\nprintf 'ses_private secret_private' >&2\nhead -c 1048577 /dev/zero >&2\nexit "+exit+"\n")
			out, err := e.Export(context.Background(), "ses_abc")
			if exit == "0" && (err != nil || string(out) != `{"messages":[]}`) {
				t.Fatalf("success = %q, %v", out, err)
			}
			if exit == "1" && (err == nil || !strings.Contains(err.Error(), "exit code 1") || out != nil) {
				t.Fatalf("failure = %q, %v", out, err)
			}
			for _, secret := range []string{"ses_private", "secret_private"} {
				if strings.Contains(logs.String(), secret) || err != nil && strings.Contains(err.Error(), secret) {
					t.Fatalf("secret leaked: %s", secret)
				}
			}
		})
	}
}

func TestOpenCodeExporterCapIsAnError(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = fakeOpenCode(t, "i=0; while [ $i -lt 100 ]; do echo 'padding padding padding'; i=$((i+1)); done\n")
	e.maxBytes = 64
	if _, err := e.Export(context.Background(), "ses_abc"); err == nil || !strings.Contains(err.Error(), "output over") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenCodeExporterExactCap(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = fakeOpenCode(t, "printf 12345678\n")
	e.maxBytes = 8
	if out, err := e.Export(context.Background(), "ses_abc"); err != nil || string(out) != "12345678" {
		t.Fatalf("at cap = %q, %v", out, err)
	}
	e.bin = fakeOpenCode(t, "printf 123456789\n")
	if out, err := e.Export(context.Background(), "ses_abc"); err == nil || out != nil {
		t.Fatalf("over cap = %q, %v", out, err)
	}
}

func TestOpenCodeExporterContext(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Export(ctx, "ses_abc"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled err = %v", err)
	}
	e.bin = fakeOpenCode(t, "exit 0\n")
	e.run = func(*exec.Cmd) error { cancel(); return context.Canceled }
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.Export(ctx, "ses_abc"); !errors.Is(err, context.Canceled) {
		t.Fatalf("during-run err = %v", err)
	}
	deadlineCtx, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()
	if _, err := e.Export(deadlineCtx, "ses_abc"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline err = %v", err)
	}
}

func TestOpenCodeExporterChildTimeoutKeepsParentAlive(t *testing.T) {
	var logs bytes.Buffer
	e := NewOpenCodeExporter(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	e.bin = fakeOpenCode(t, "exit 0\n")
	e.timeout = -time.Second
	e.run = func(cmd *exec.Cmd) error { return cmd.Run() }
	parent := context.Background()
	out, err := e.Export(parent, "ses_private")
	if out != nil || err == nil || errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
		t.Fatalf("child timeout = %q, %v; parent = %v", out, err, parent.Err())
	}
	if !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "ses_private") ||
		!strings.Contains(logs.String(), `"category":"export_timeout"`) || strings.Contains(logs.String(), "ses_private") {
		t.Fatalf("unsafe timeout result: %v; logs = %s", err, logs.String())
	}
}

func TestOpenCodeExporterMissingBinary(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = "opencode-definitely-missing-binary"
	if _, err := e.Export(context.Background(), "ses_abc"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenCodeExporterRejectsBadSessionID(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = "opencode-definitely-missing-binary" // never reached
	for _, id := range []string{"", "--help", "-x"} {
		if _, err := e.Export(context.Background(), id); err == nil || !strings.Contains(err.Error(), "invalid session id") {
			t.Fatalf("Export(%q) err = %v", id, err)
		}
	}
}

// TestOpenCodeExporterUsesAFileForStdout: opencode silently truncates its
// JSON when stdout is a pipe (64 KiB multiples, exit 0), so the export must
// run against a regular file that is removed afterwards.
func TestOpenCodeExporterUsesAFileForStdout(t *testing.T) {
	e := NewOpenCodeExporter(nil)
	e.bin = fakeOpenCode(t, `printf '{"messages":[]}'`)
	var name string
	e.run = func(cmd *exec.Cmd) error {
		f, ok := cmd.Stdout.(*os.File)
		if !ok {
			t.Fatalf("stdout is %T, want *os.File: opencode truncates piped output", cmd.Stdout)
		}
		name = f.Name()
		return cmd.Run()
	}
	out, err := e.Export(context.Background(), "ses_abc")
	if err != nil || string(out) != `{"messages":[]}` {
		t.Fatalf("Export = %q, %v", out, err)
	}
	if _, statErr := os.Stat(name); !os.IsNotExist(statErr) {
		t.Fatalf("temp file left behind (%s): %v", name, statErr)
	}
}
