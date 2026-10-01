package domain_test

import (
	"strings"
	"testing"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const botToken = "1234567890:" + "AAHf3kJd9sLq2mN8pR4tV6wX0yZ1bC3dE5f" // built from parts so secret scanners ignore it

func TestRedactPatterns(t *testing.T) {
	r := domain.NewRedactor(botToken)
	cases := []struct {
		name  string
		in    string
		want  string
		stats string
	}{
		{"openai", "key sk-" + "proj-abcdefghijklmnopqrstuvwxyz0123 ok", "key sk-…0123 ok", "openai=1"},
		{"anthropic", "sk-" + "ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz012345", "sk-…2345", "openai=1"},
		{"github classic", "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "ghp_…ghij", "github=1"},
		{"github fine", "github_pat_" + "11ABCDEFG0abcdefghijklmnopqrstuvwxyz", "github_pat_…wxyz", "github=1"},
		{"aws", "AKIA" + "IOSFODNN7EXAMPLE", "AKIA…MPLE", "aws=1"},
		{"slack", "xoxb-" + "123456789012-abcdefghijklmnop", "xoxb-…mnop", "slack=1"},
		{"gitlab", "glpat-" + "abcdefghijklmnopqrstuv", "glpat-…stuv", "gitlab=1"},
		{"google", "AIza" + "SyA1234567890abcdefghijklmnopqrstuv", "AIza…stuv", "google=1"},
		{"jwt", "eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijklmnop", "eyJ…mnop", "jwt=1"},
		{"bearer", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz", "Authorization: Bearer …wxyz", "bearer=1"},
		{"password", "password=hunter2secret", "password=[redacted]", "keyvalue=1"},
		{"api key colon", `API_KEY: "abcdefghijkl"`, `API_KEY: "[redacted]"`, "keyvalue=1"},
		{"telegram shape", "token 9876543210:" + "BBGf3kJd9sLq2mN8pR4tV6wX0yZ1bC3dE5g", "token [redacted]", "telegram=1"},
		{"bot token exact", "curl https://api.telegram.org/bot" + botToken + "/getMe", "curl https://api.telegram.org/bot[redacted]/getMe", "exact=1"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\nrest", "[redacted]\nrest", "privatekey=1"},
		{"private key cut", "text\n-----BEGIN PRIVATE KEY-----\nMIIEow\nMIIB", "text\n[redacted]", "privatekey=1"},
		{"two kinds", "sk-abcdefghijklmnopqrstuvwx and Bearer 0123456789abcdefghij", "sk-…uvwx and Bearer …ghij", "bearer=1 openai=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stats := r.Redact(tc.in)
			if got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if stats.String() != tc.stats {
				t.Errorf("stats = %q, want %q", stats.String(), tc.stats)
			}
		})
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	r := domain.NewRedactor(botToken, "short")
	inputs := []string{
		"token: none",
		"password reset email sent",
		"sk- alone and sk-short",
		"12345:abc",
		"Bearer of bad news",
		"the ghost_of a key",
		"AKIA is a prefix",
		"eyJ.eyJ.eyJ",
		"plain terminal output with numbers 1234567890",
		"",
	}
	for _, in := range inputs {
		got, stats := r.Redact(in)
		if got != in || stats.Total() != 0 {
			t.Errorf("Redact(%q) = %q, stats %v; want unchanged", in, got, stats)
		}
	}
}

func TestRedactKeepsEndsAtMinimalLength(t *testing.T) {
	r := domain.NewRedactor()
	// The shortest slack body (10 characters) still leaves prefix, ellipsis
	// and a four-character tail.
	got, _ := r.Redact("xoxb-" + "abcdefghij")
	if got != "xoxb-…ghij" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	r := domain.NewRedactor(botToken)
	in := "sk-abcdefghijklmnopqrstuvwx Bearer 0123456789abcdefghij password=hunter2secret " + botToken
	once, stats := r.Redact(in)
	if stats.Total() != 4 {
		t.Fatalf("first pass stats %v", stats)
	}
	twice, again := r.Redact(once)
	if twice != once || again.Total() != 0 {
		t.Fatalf("second pass changed text: %q → %q, stats %v", once, twice, again)
	}
}

func TestRedactLargeInput(t *testing.T) {
	r := domain.NewRedactor(botToken)
	line := "some ordinary output line with a token sk-abcdefghijklmnopqrstuvwx in it\n"
	in := strings.Repeat(line, 256*1024/len(line))
	got, stats := r.Redact(in)
	if stats["openai"] == 0 || strings.Contains(got, "sk-abcdef") {
		t.Fatalf("large input not redacted: stats %v", stats)
	}
}

func TestRedactionStatsString(t *testing.T) {
	s := domain.RedactionStats{"openai": 2, "bearer": 1}
	if s.String() != "bearer=1 openai=2" || s.Total() != 3 {
		t.Fatalf("String() = %q, Total() = %d", s.String(), s.Total())
	}
	if (domain.RedactionStats{}).String() != "" {
		t.Fatal("empty stats should render empty")
	}
}

// TestRedactPrefixedKeysAndCredentials covers secrets the first rule set
// missed: env names with a prefix or suffix around the keyword, JSON keys,
// query parameters, URL credentials, HTTP Basic and Stripe keys.
func TestRedactPrefixedKeysAndCredentials(t *testing.T) {
	r := domain.NewRedactor()
	cases := []struct {
		name  string
		in    string
		want  string
		stats string
	}{
		{"db password", "DB_PASSWORD=supersecret1", "DB_PASSWORD=[redacted]", "keyvalue=1"},
		{"postgres password", "POSTGRES_PASSWORD=supersecret1", "POSTGRES_PASSWORD=[redacted]", "keyvalue=1"},
		{"aws secret", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI" + "K7MDENGbPxRfiCYEXAMPLEKEY", "AWS_SECRET_ACCESS_KEY=[redacted]", "keyvalue=1"},
		{"client secret", "client_secret=abcdefgh1234", "client_secret=[redacted]", "keyvalue=1"},
		{"secret key yaml", "secret_key: abcdefgh1234", "secret_key: [redacted]", "keyvalue=1"},
		{"github token env", "GITHUB_TOKEN=abcdefgh12345678", "GITHUB_TOKEN=[redacted]", "keyvalue=1"},
		{"query token", "https://x.example/cb?access_token=abcdefgh12345678&state=1", "https://x.example/cb?access_token=[redacted]&state=1", "keyvalue=1"},
		{"json password", `{"password": "hunter2secret"}`, `{"password": "[redacted]"}`, "keyvalue=1"},
		{"url credentials", "postgres://app:pa55word@db.local/app", "postgres://app:[redacted]@db.local/app", "urlcreds=1"},
		{"basic auth", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "Authorization: Basic …ZA==", "basic=1"},
		{"stripe live", "sk_live_" + "abcdefghijklmnopqrstuvwx", "sk_live_…uvwx", "stripe=1"},
		{"stripe restricted", "rk_test_" + "abcdefghijklmnopqrstuvwx", "rk_test_…uvwx", "stripe=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stats := r.Redact(tc.in)
			if got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if stats.String() != tc.stats {
				t.Errorf("stats = %q, want %q", stats.String(), tc.stats)
			}
			if again, s := r.Redact(got); again != got || s.Total() != 0 {
				t.Errorf("second pass changed %q to %q", got, again)
			}
		})
	}
	for _, in := range []string{`"tokens": 12`, "password_reset_url", "TOKEN_LIMIT=4000", "max_tokens=4096", "git@github.com:org/repo.git", "https://example.com:8443/path"} {
		if got, stats := r.Redact(in); got != in || stats.Total() != 0 {
			t.Errorf("Redact(%q) = %q; want unchanged", in, got)
		}
	}
}

// TestRedactMarkdownDecoratedKeys: redaction runs on the raw Markdown an
// agent wrote, so a key in bold, a code span or a table cell must still
// have its value masked.
func TestRedactMarkdownDecoratedKeys(t *testing.T) {
	r := domain.NewRedactor()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bold key", "**API_KEY**: hunter2hunter2hunter2", "**API_KEY**: [redacted]"},
		{"italic key", "*client_secret*=hunter2hunter2", "*client_secret*=[redacted]"},
		{"code span key", "`DB_PASSWORD`=hunter2hunter2", "`DB_PASSWORD`=[redacted]"},
		{"code span pair", "`DB_PASSWORD=hunter2hunter2`", "`DB_PASSWORD=[redacted]`"},
		{"bold key code value", "**password**: `hunter2hunter2`", "**password**: `[redacted]`"},
		{"table cell", "| API_TOKEN | hunter2hunter2 |", "| API_TOKEN | [redacted] |"},
		{"table cell code", "| `DB_PASSWORD` | `hunter2hunter2` |", "| `DB_PASSWORD` | `[redacted]` |"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stats := r.Redact(tc.in)
			if got != tc.want || stats.String() != "keyvalue=1" {
				t.Errorf("Redact(%q) = %q (%v), want %q", tc.in, got, stats, tc.want)
			}
			if again, s := r.Redact(got); again != got || s.Total() != 0 {
				t.Errorf("second pass changed %q to %q", got, again)
			}
		})
	}
	for _, in := range []string{
		"| Token | Description |",
		"| password | Required |",
		"| api_key | string | yes |",
		"**Token**: none",
		"echo $TOKEN | base64 -d | head -c 64",
	} {
		if got, stats := r.Redact(in); got != in || stats.Total() != 0 {
			t.Errorf("Redact(%q) = %q; want unchanged", in, got)
		}
	}
}

// TestRedactSoftWrappedTokens: a terminal soft-wraps a long line, so the
// screen text carries a newline (and the pane's indentation) in the middle
// of a token.
func TestRedactSoftWrappedTokens(t *testing.T) {
	r := domain.NewRedactor(botToken)
	for _, in := range []string{
		"TG=" + botToken[:30] + "\n" + botToken[30:],
		"TG=" + botToken[:12] + "\n  " + botToken[12:40] + "\r\n" + botToken[40:] + " ok",
	} {
		got, stats := r.Redact(in)
		if strings.Contains(got, botToken[12:30]) || stats["exact"] != 1 {
			t.Errorf("Redact(%q) = %q (%v); bot token survived", in, got, stats)
		}
	}
	other := "9876543210:" + "BBGf3kJd9sLq2mN8pR4tV6wX0yZ1bC3dE5g"
	in := "TOKEN=" + other[:25] + "\n   " + other[25:] + "\nnext line"
	if got, stats := r.Redact(in); got != "TOKEN=[redacted]\nnext line" || stats.String() != "telegram=1" {
		t.Errorf("Redact(%q) = %q (%v)", in, got, stats)
	}
	in = "id " + other[:25] + "\n" + other[25:]
	if got, stats := r.Redact(in); got != "id [redacted]" || stats.String() != "telegram=1" {
		t.Errorf("Redact(%q) = %q (%v)", in, got, stats)
	}
	// Ordinary lines that only meet at a newline stay as they are.
	for _, in := range []string{"12345678\n:abc", "line one\nline two", "2026-10-01 12:34:56\n" + strings.Repeat("a", 40)} {
		if got, stats := r.Redact(in); got != in || stats.Total() != 0 {
			t.Errorf("Redact(%q) = %q; want unchanged", in, got)
		}
	}
}

// TestRedactExactOnly: the bot token is masked even when the owner turned
// the pattern redaction off; nothing else is touched.
func TestRedactExactOnly(t *testing.T) {
	r := domain.NewRedactor(botToken)
	in := "bot " + botToken[:20] + "\n" + botToken[20:] + " key sk-abcdefghijklmnopqrstuvwx password=hunter2secret"
	got, stats := r.RedactExact(in)
	if got != "bot [redacted] key sk-abcdefghijklmnopqrstuvwx password=hunter2secret" || stats.String() != "exact=1" {
		t.Fatalf("RedactExact = %q (%v)", got, stats)
	}
}
