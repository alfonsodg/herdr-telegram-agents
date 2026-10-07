# This fork

`permgps/herdr-telegram-agents` is the upstream project; this fork
(`alfonsodg/herdr-telegram-agents`) carries it as its base and keeps a small
set of deltas on top. `main` here is the product: upstream v0.16.0 plus the
deltas below, all built and deployed against the operator's own Herdr.

Upstream is kept as a reference and a source of releases, never as a
prerequisite: nothing here waits for upstream to merge anything.

## Deltas on top of upstream

Each delta names the fork issue that tracks it and, when one exists, the
upstream submission.

- **OpenCode store read** (fork #17; upstream PR #46, open). On OpenCode 2.x
  the last turn is read from opencode's own SQLite store instead of the
  whole-session export (85-280 KB and ~11 ms where the export answered
  228-649 MB). Hardened: `sqlite3 -init <devnull>`, the database through a
  `file:` URI with `?mode=ro`, shape-checked rows failing closed to the CLI
  export, one debug line per read with `source=db|export`.
  Files: `internal/adapters/system/opencode_db{,_test}.go`,
  `internal/adapters/system/opencode.go`, docs, README.
- **Safe blocked-dialog delivery** (fork #23; upstream PR #44, open). A plain
  message reaches a blocked agent only through a verified free-text entry
  (same dialog on screen, same choices by number and label, the entry chosen,
  a bounded wait for the box, then type and Enter); a permission dialog, a
  changed or unrecognised screen, a blank screen or a failed read send
  nothing and reply a hint. The ✏️ wait types directly because its box is
  open by construction.
  Files: `internal/adapters/herdr/*` (SendText, `agent_blocked` mapping),
  `internal/app/outbound.go`, `internal/app/inbound.go`, tests, docs.
- **Unreadable-replies notice** (fork #21; upstream PR #45, open). After four
  delivered fallback posts whose reply could not be read, the topic gets one
  diagnostic notice (no sound, held while quiet mode is at the desk); a
  readable reply clears the streak. Expected fallbacks never count:
  `ErrUnsupportedAgent` marks kinds nobody reads (all six readers wrap it)
  and `ErrStaleTranscript` marks a reply older than the turn start.
  Files: `internal/domain/{errors,replysource}.go`, the six readers,
  `internal/app/outbound.go`, compose test, docs.
- **Quiet mode on machines without an idle source** (fork #4, #22; upstream
  issue #43, open). Upstream's chain (Mutter, xprintidle) is kept; on top,
  an unsupported or answerless source makes quiet **start at the desk**.
  `/away` releases it, a timed `/away` hands the default back, `/here`
  brings it back by hand, and an unsupported verdict is retried once a
  minute so a source that appears later recovers without a restart.
  Files: `internal/app/presence.go`, `internal/domain/presence.go`,
  `internal/domain/options.go`, `internal/app/inbound.go`, docs.
- **Codex directory fallback** (fork #14; not submitted upstream). When
  Herdr's reported Codex session went stale, the newest rollout of the
  pane's directory answers if it carries a fresher reply.
  Files: `internal/adapters/transcript/codexdir{,_test}.go`,
  `internal/compose/container.go`.
- **`/models` in the private scoped command lists** (fork #16 follow-up;
  upstream's in-house `/models` did not add it there).
  Files: `internal/app/private_dashboard.go`.
- `.markdownlint.json` is local development only (the pre-commit hook lints
  staged Markdown with default rules; these relaxations cover upstream's
  long table rows and two intentional code spans). Never proposed upstream.

## Sync routine for a new upstream release

1. `git fetch upstream --tags`. If `git log --oneline main..upstream/main`
   is empty, there is nothing to do.
2. `git switch -c sync/vX.Y.Z`; `git merge upstream/main`.
3. Resolve every conflict with the standing policy:
   - upstream implemented the same thing in-house: take **theirs** and drop
     our duplicate from the delta list above;
   - fork-only delta: keep **ours**, re-applied on the new base;
   - docs: merge both texts.
4. Loss audit before committing: compare the old and new trees for files
   that disappeared (`git diff --name-status main..HEAD` on the old branch
   vs the new one). The v0.16.0 sync lost `idle_linux.go` this way; that is
   what this step exists to catch.
5. `make test` and `make lint`. For the `internal/adapters/system` package
   the tests need `GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null`
   (the operator's global git hooks path breaks them otherwise).
6. `make build`; restart the daemon (linked plugin):
   `bin/herdr-tg action stop`, then
   `herdr plugin action invoke permgps.telegram-agents.start`.
7. Verify the daemon log: `daemon starting` with the new version,
   `telegram connected`, the `presence` line, and no new warnings.
8. Live smoke on the real topics: a long OpenCode session posts the whole
   answer; a blocked dialog takes text only through its text entry; the
   quiet workflow (`/away`, `/here`) responds as documented.
9. Update this file: deltas that moved upstream move to the upstream index
   below, and their fork issues can be closed after review.

## Upstream index

- Merged PRs: #40 (stale-transcript slack).
- Open PRs: #44 (safe blocked-dialog delivery), #45 (unreadable notice),
  #46 (OpenCode store read).
- Open issues: #37 (blocked prompt), #38 (fast turns marked stale, shipped
  in v0.16.0), #39 (long sessions vs cap), #43 (quiet on machines without a
  readable idle source).
- Upstream releases adopted here: v0.15.0 (long poll, kinds, event, /away,
  Windows installer), v0.16.0 (Muse/Agy/Pi readers, pickers, export file,
  pending wait, idle chain, quota lines).

## Local environment notes

- Plugin linked to this checkout:
  `permgps.telegram-agents [local:<repo>]`; daemon state under
  `~/.local/state/herdr/plugins/permgps.telegram-agents/`, config under
  `~/.config/herdr/plugins/config/permgps.telegram-agents/`.
- The store read needs `sqlite3` on the daemon's `PATH`; without it the CLI
  export answers.
- The operator's session is KDE Plasma on Wayland, which exposes no idle
  source; quiet mode there is manual by design (`/here`/`/away`, and it
  starts at the desk after a restart).
- `mapping.json` backups (`mapping.json.bak-*`) and `options.json` backups
  (`options.json.bak-*`) are kept next to the live files before hand edits.
