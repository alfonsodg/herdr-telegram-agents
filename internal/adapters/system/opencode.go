package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// openCodeExportTimeout bounds the export attempts together.
	openCodeExportTimeout = 10 * time.Second
	// openCodeExportMaxOutput caps the JSON a run may produce. A session
	// export is a few MB in normal use, and truncated JSON cannot be parsed,
	// so a capped run is a failure rather than a partial result.
	openCodeExportMaxOutput = 16 << 20
	// openCodeWaitDelay bounds how long a run waits for its pipes once the
	// timeout killed opencode.
	openCodeWaitDelay = time.Second
)

// OpenCodeExporter runs OpenCode's session export command from the binary on
// PATH and returns its JSON. The session id Herdr reported for the pane is
// passed as one argument, never through a shell.
type OpenCodeExporter struct {
	bin      string
	timeout  time.Duration
	maxBytes int
	log      *slog.Logger
	run      func(*exec.Cmd) error
}

// NewOpenCodeExporter returns an exporter for "opencode" on PATH.
func NewOpenCodeExporter(log *slog.Logger) *OpenCodeExporter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &OpenCodeExporter{bin: "opencode", timeout: openCodeExportTimeout, maxBytes: openCodeExportMaxOutput, log: log}
}

// Export runs the export for sessionID. A missing binary, a timeout, a
// non-zero exit and output over the cap are all errors.
func (e *OpenCodeExporter) Export(ctx context.Context, sessionID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("opencode export: %w", err)
	}
	if sessionID == "" || strings.HasPrefix(sessionID, "-") {
		return nil, fmt.Errorf("opencode export: invalid session id")
	}
	bin, err := exec.LookPath(e.bin)
	if err != nil {
		return nil, fmt.Errorf("opencode export: binary not found")
	}
	parentCtx := ctx
	childCtx, cancel := context.WithTimeout(parentCtx, e.timeout)
	defer cancel()
	start := time.Now()
	run := e.run
	if run == nil {
		run = (*exec.Cmd).Run
	}
	runExport := func(args ...string) (*limitedWriter, error) {
		cmd := command(childCtx, bin, args...)
		cmd.WaitDelay = openCodeWaitDelay
		cmd.Stderr = io.Discard
		stdout := &limitedWriter{max: e.maxBytes}
		// opencode silently truncates its JSON when stdout is a pipe (64 KiB
		// multiples, exit 0); a regular file receives the whole stream, so
		// the child writes to a private temp file that is read back under
		// the cap and removed at once.
		file, err := os.CreateTemp("", "herdr-tg-opencode-*.json")
		if err != nil {
			return stdout, fmt.Errorf("opencode export: temp file: %w", err)
		}
		defer func() { _ = os.Remove(file.Name()) }()
		cmd.Stdout = file
		runErr := run(cmd)
		_, _ = file.Seek(0, io.SeekStart)
		_, _ = io.Copy(stdout, io.LimitReader(file, int64(e.maxBytes)+1))
		_ = file.Close()
		return stdout, runErr
	}
	stdout, runErr := runExport("session", "export", sessionID)
	// OpenCode 1.x used "opencode export". Confirm the major version
	// before trying it: a failed 2.x export can mean an unavailable session,
	// and "opencode export" on 2.x can open its interactive UI.
	var firstExit *exec.ExitError
	if errors.As(runErr, &firstExit) && childCtx.Err() == nil && !stdout.capped {
		versionOut, versionErr := runExport("--version")
		if versionErr == nil && !versionOut.capped && openCodeV1Version(versionOut.buf.String()) {
			stdout, runErr = runExport("export", sessionID)
		}
	}
	category := "ok"
	if parentCtx.Err() != nil {
		category = "parent_context"
	} else if childCtx.Err() != nil {
		category = "export_timeout"
	} else if stdout.capped {
		category = "output_cap"
	} else if runErr != nil {
		category = "run_failed"
	}
	var exitErr *exec.ExitError
	exitCode := -1
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
		if category == "run_failed" {
			category = "exit_nonzero"
		}
	}
	e.log.Debug("opencode export", slog.Int64("dur_ms", time.Since(start).Milliseconds()),
		slog.Int("bytes", stdout.buf.Len()), slog.Bool("capped", stdout.capped),
		slog.String("category", category), slog.Int("exit_code", exitCode))
	switch {
	case parentCtx.Err() != nil:
		return nil, fmt.Errorf("opencode export: %w", parentCtx.Err())
	case childCtx.Err() != nil:
		return nil, errors.New("opencode export: timed out")
	case runErr == nil && !stdout.capped:
		return stdout.buf.Bytes(), nil
	case stdout.capped:
		return nil, fmt.Errorf("opencode export: output over %d bytes", e.maxBytes)
	}
	if exitErr != nil {
		return nil, fmt.Errorf("opencode export: exit code %d", exitCode)
	}
	return nil, fmt.Errorf("opencode export: start or wait failed")
}

func openCodeV1Version(version string) bool {
	for _, field := range strings.Fields(version) {
		if strings.HasPrefix(strings.TrimPrefix(field, "v"), "1.") {
			return true
		}
	}
	return false
}
