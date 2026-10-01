package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Redactor masks secrets in text before it leaves the machine: API keys,
// tokens, passwords, private keys and the bot token itself. It is pure and
// safe for concurrent use; the daemon builds one per process.
//
// A matched key keeps a recognisable prefix and its last four characters
// (`sk-…a1b2`), key=value pairs keep the key (`password=[redacted]`), and
// the bot token and private-key blocks vanish whole (`[redacted]`). Every
// pattern carries a minimum length so ordinary words are left alone.
type Redactor struct {
	exact []*regexp.Regexp
}

// RedactionStats counts replacements per pattern name.
type RedactionStats map[string]int

// Total is the number of replacements across every pattern.
func (s RedactionStats) Total() int {
	n := 0
	for _, c := range s {
		n += c
	}
	return n
}

// String renders the counts as `name=count` pairs in name order, for logs.
func (s RedactionStats) String() string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, s[name]))
	}
	return strings.Join(parts, " ")
}

// redactedMark replaces a secret that must not stay recognisable.
const redactedMark = "[redacted]"

// exactMinLen is the shortest exact secret worth replacing; anything
// shorter would mask ordinary text.
const exactMinLen = 8

// keepEndsTail is how many trailing characters a masked key keeps.
const keepEndsTail = 4

type redactMode int

const (
	// redactWhole replaces the whole match with redactedMark.
	redactWhole redactMode = iota
	// redactKeepEnds keeps group 1 (the prefix) and the last four
	// characters of the match, with an ellipsis between.
	redactKeepEnds
	// redactKeyValue replaces the last group (the value) with redactedMark
	// and keeps the rest of the match: the key and separator before it and
	// anything after it (the `@` of URL credentials).
	redactKeyValue
)

type redactRule struct {
	name string
	re   *regexp.Regexp
	mode redactMode
	// plausible, when set, keeps only the matches whose value (the last
	// group) it accepts; the others are left alone.
	plausible func(value string) bool
}

// softWrap is the break a terminal inserts when it wraps a long line: a
// newline with the pane's indentation around it. Token patterns allow it
// between any two characters.
const softWrap = `(?:[ \t]*\r?\n[ \t]*)?`

// secretKey is a key whose value is masked; the keyword may sit inside a
// longer name (DB_PASSWORD, client_secret, access_token).
const secretKey = `[A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)[A-Za-z0-9_.-]*`

// mdDecor is the Markdown an agent wraps a key or value in: bold, italics
// or a code span. Redaction runs on the raw Markdown, before rendering.
const mdDecor = "[*_`]{0,2}"

// secretValue is a masked value: eight characters or more, not starting
// with `[` (a mark already placed). Quotes, `&` (the next query parameter)
// and a backtick (the end of a code span) end it.
const secretValue = "[^\\s'\"\\[&`][^\\s'\"&`]{7,}"

// redactRules run in order on the output of the previous rule; a
// replacement never matches a later rule because the ellipsis and the
// mark are outside every character class. Names may repeat: their counts
// merge in the stats.
var redactRules = []redactRule{
	{"privatekey", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`), redactWhole, nil},
	{"telegram", regexp.MustCompile(`\b\d(?:` + softWrap + `\d){7,9}` + softWrap + `:(?:` + softWrap + `[A-Za-z0-9_-]){35}\b`), redactWhole, nil},
	// URL credentials run before keyvalue so a key-like user name does not
	// swallow the host.
	{"urlcreds", regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@]+:)([^\s@/\[][^\s@/]*)@`), redactKeyValue, nil},
	// A JSON key's closing quote or the Markdown around a key
	// (**API_KEY**:, `DB_PASSWORD`=) may precede the separator.
	{"keyvalue", regexp.MustCompile(`(?i)\b(` + secretKey + `)(["']?` + mdDecor + `\s*[=:]\s*['"]?` + mdDecor + `)(` + secretValue + `)`), redactKeyValue, nil},
	// A Markdown table row `| API_TOKEN | value |`. Header rows name such
	// keys too (`| Token | Description |`), so a value of letters only is
	// left alone.
	{"keyvalue", regexp.MustCompile(`(?i)(\|[ \t]*` + mdDecor + secretKey + mdDecor + `[ \t]*\|[ \t]*` + mdDecor + `)(` + secretValue + `)` + mdDecor + `[ \t]*\|`), redactKeyValue, notOnlyLetters},
	{"openai", regexp.MustCompile(`\b(sk-)[A-Za-z0-9_-]{20,}`), redactKeepEnds, nil},
	{"github", regexp.MustCompile(`\b(gh[poushr]_)[A-Za-z0-9]{36,}`), redactKeepEnds, nil},
	{"github", regexp.MustCompile(`\b(github_pat_)[A-Za-z0-9_]{22,}`), redactKeepEnds, nil},
	{"aws", regexp.MustCompile(`\b((?:AKIA|ASIA))[A-Z0-9]{16}\b`), redactKeepEnds, nil},
	{"slack", regexp.MustCompile(`\b(xox[abprs]-)[A-Za-z0-9-]{10,}`), redactKeepEnds, nil},
	{"gitlab", regexp.MustCompile(`\b(glpat-)[A-Za-z0-9_-]{20,}`), redactKeepEnds, nil},
	{"google", regexp.MustCompile(`\b(AIza)[0-9A-Za-z_-]{35}\b`), redactKeepEnds, nil},
	{"jwt", regexp.MustCompile(`\b(eyJ)[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), redactKeepEnds, nil},
	{"bearer", regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]{16,}`), redactKeepEnds, nil},
	{"basic", regexp.MustCompile(`(?i)\b(Basic\s+)[A-Za-z0-9+/=]{12,}`), redactKeepEnds, nil},
	{"stripe", regexp.MustCompile(`\b([sr]k_(?:live|test)_)[A-Za-z0-9]{16,}`), redactKeepEnds, nil},
}

// notOnlyLetters accepts a value with a digit or a symbol in it.
func notOnlyLetters(value string) bool {
	return strings.IndexFunc(value, func(c rune) bool { return !unicode.IsLetter(c) }) >= 0
}

// NewRedactor returns a redactor that also replaces the given exact
// strings (the bot token), also when a terminal wrapped one across lines;
// empty or short ones are ignored.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) < exactMinLen {
			continue
		}
		chars := make([]string, 0, len(s))
		for _, c := range s {
			chars = append(chars, regexp.QuoteMeta(string(c)))
		}
		r.exact = append(r.exact, regexp.MustCompile(strings.Join(chars, softWrap)))
	}
	return r
}

// Redact returns text with every secret masked and the counts per pattern.
// An empty stats map means the text came back unchanged.
func (r *Redactor) Redact(text string) (string, RedactionStats) {
	text, stats := r.RedactExact(text)
	if text == "" {
		return text, stats
	}
	for _, rule := range redactRules {
		text = rule.apply(text, stats)
	}
	return text, stats
}

// RedactExact masks only the exact secrets given to NewRedactor (the bot
// token). It runs even when the owner turned pattern redaction off.
func (r *Redactor) RedactExact(text string) (string, RedactionStats) {
	stats := RedactionStats{}
	if text == "" {
		return text, stats
	}
	for _, re := range r.exact {
		if n := len(re.FindAllStringIndex(text, -1)); n > 0 {
			text = re.ReplaceAllLiteralString(text, redactedMark)
			stats["exact"] += n
		}
	}
	return text, stats
}

// apply rewrites every match of the rule in one pass so a replacement is
// never re-matched by the same rule.
func (rule redactRule) apply(text string, stats RedactionStats) string {
	matches := rule.re.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, m := range matches {
		if rule.plausible != nil && !rule.plausible(text[m[len(m)-2]:m[len(m)-1]]) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(rule.replacement(text, m))
		last = m[1]
		stats[rule.name]++
	}
	b.WriteString(text[last:])
	return b.String()
}

func (rule redactRule) replacement(text string, m []int) string {
	whole := text[m[0]:m[1]]
	switch rule.mode {
	case redactKeepEnds:
		prefix := text[m[2]:m[3]]
		if len(whole) < len(prefix)+2*keepEndsTail {
			return redactedMark
		}
		return prefix + "…" + whole[len(whole)-keepEndsTail:]
	case redactKeyValue:
		start, end := m[len(m)-2], m[len(m)-1]
		return text[m[0]:start] + redactedMark + text[end:m[1]]
	default:
		return redactedMark
	}
}
