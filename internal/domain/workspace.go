package domain

import "strings"

// DefaultAgentKind is what /new starts when no kind is given.
const DefaultAgentKind = "claude"

// AgentKinds lists the kinds `herdr agent start` accepts (Herdr 0.9.3, from
// `herdr agent start --help`; older versions reject kinds they do not know).
// /new treats its last word as a kind only when it is in this list, so a
// workspace label may end in any other word.
var AgentKinds = []string{
	"pi", "claude", "codex", "gemini", "cursor", "devin", "agy", "cline",
	"omp", "mastracode", "opencode", "copilot", "kimi", "kiro", "droid",
	"amp", "grok", "hermes", "kilo", "qodercli", "qwen", "letta", "maki",
	"muse",
}

// IsAgentKind reports whether s names a kind in AgentKinds
// (case-insensitive).
func IsAgentKind(s string) bool {
	for _, k := range AgentKinds {
		if strings.EqualFold(k, s) {
			return true
		}
	}
	return false
}

// MatchResult says how a typed workspace label matched Herdr's list.
type MatchResult int

const (
	// MatchNone means no workspace label equals or starts with the text.
	MatchNone MatchResult = iota
	// MatchOne means exactly one workspace matched.
	MatchOne
	// MatchMany means several labels start with the text and none equals it.
	MatchMany
)

// MatchWorkspaces returns the workspaces a typed label selects: the ones
// whose trimmed label equals the trimmed text (case-insensitive) when any
// does, else every workspace whose label starts with it. A workspace
// without a label is matched by its id. An empty text matches nothing.
func MatchWorkspaces(label string, workspaces []Workspace) []Workspace {
	want := strings.ToLower(strings.TrimSpace(label))
	if want == "" {
		return nil
	}
	var exact, prefixed []Workspace
	for _, ws := range workspaces {
		have := strings.ToLower(strings.TrimSpace(ws.Label))
		if have == "" {
			have = strings.ToLower(ws.ID)
		}
		switch {
		case have == want:
			exact = append(exact, ws)
		case strings.HasPrefix(have, want):
			prefixed = append(prefixed, ws)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return prefixed
}

// MatchWorkspace picks the workspace a typed label means: an exact match
// (case-insensitive, whitespace trimmed) wins, else a unique prefix; several
// prefix matches are MatchMany and no match is MatchNone.
func MatchWorkspace(label string, workspaces []Workspace) (Workspace, MatchResult) {
	found := MatchWorkspaces(label, workspaces)
	switch len(found) {
	case 0:
		return Workspace{}, MatchNone
	case 1:
		return found[0], MatchOne
	}
	return Workspace{}, MatchMany
}
