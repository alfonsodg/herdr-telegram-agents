package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/permgps/herdr-telegram-agents/internal/compose"
)

// usageTapUsage is printed when --out is missing.
const usageTapUsage = "usage: herdr-tg usage-tap --out <file>"

// runUsageTap is the Claude Code status line tap: Claude Code runs it on
// every status line refresh with the status line JSON on stdin, chained
// before the operator's own command
// (`herdr-tg usage-tap --out <state>/claude-usage.json | ~/.claude/statusline.sh`).
// It copies stdin to stdout whatever happens and stores the rate-limit
// windows for the daemon's quota line. Claude Code sets no HERDR_PLUGIN_*
// variables, so the file comes from --out and the plugin env is never
// read. Nothing goes to daemon.log: this runs in Claude Code's process,
// many times a minute. Only a missing --out is a failure; anything else
// is one stderr line and exit 0, so the status line keeps working.
func runUsageTap(rc *runContext, args []string) int {
	fs := flag.NewFlagSet("usage-tap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "file the rate-limit windows are written to")
	parseErr := fs.Parse(args)
	if parseErr != nil || *out == "" || fs.NArg() > 0 {
		if _, err := io.Copy(rc.stdout, rc.stdin); err != nil {
			fmt.Fprintf(rc.stderr, "usage-tap: %v\n", err)
		}
		fmt.Fprintln(rc.stderr, usageTapUsage)
		return exitUsage
	}
	if err := compose.TapClaudeUsage(rc.stdin, rc.stdout, *out); err != nil {
		fmt.Fprintf(rc.stderr, "usage-tap: %v\n", err)
	}
	return exitOK
}
