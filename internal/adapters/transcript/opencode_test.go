package transcript

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// openCodeTuple is the session Herdr reports for an opencode pane.
var openCodeTuple = domain.SessionTuple{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_abc"}

// openCodeTurn is a trimmed "opencode export" of one session: an older
// turn, then the last prompt answered in two steps, the first a tool call
// with an edit and no text, the second the reply.
const openCodeTurn = `{
  "info": {"id": "ses_abc"},
  "messages": [
    {"info": {"role": "user", "time": {"created": 500}}, "parts": [{"type": "text", "text": "old prompt"}]},
    {"info": {"role": "assistant", "modelID": "old-model", "tokens": {"output": 7}, "time": {"created": 600, "completed": 700}},
     "parts": [{"type": "text", "text": "old reply"}]},
    {"info": {"role": "user", "time": {"created": 1000}}, "parts": [{"type": "text", "text": "what's next"}]},
    {"info": {"role": "assistant", "modelID": "codigo-3", "tokens": {"output": 40}, "time": {"created": 1001, "completed": 1002}},
     "parts": [
       {"type": "step-start"},
       {"type": "reasoning", "text": "thinking about it"},
       {"type": "tool", "tool": "edit", "state": {"status": "completed", "input": {"filePath": "docs/plan.md"}}},
       {"type": "tool", "tool": "read", "state": {"input": {"filePath": "README.md"}}},
       {"type": "step-finish"}
     ]},
    {"info": {"role": "assistant", "modelID": "codigo-3", "tokens": {"output": 60}, "time": {"created": 1003, "completed": 61004}},
     "parts": [
       {"type": "tool", "tool": "edit", "state": {"status": "completed", "input": {"filePath": "docs/plan.md"}}},
       {"type": "text", "text": "Endpoint is **live** now."},
       {"type": "text", "text": "Click refresh."}
     ]}
  ]
}`

// openCodeFixture builds a reader over scripted Herdr and export answers
// and records whether the export ran.
type openCodeFixture struct {
	tuple    domain.SessionTuple
	lookErr  error
	out      string
	exportFn func(id string) ([]byte, error)
	exported []string
	logBuf   bytes.Buffer
}

func (f *openCodeFixture) reader() *OpenCodeReader {
	session := func(context.Context, string) (domain.SessionTuple, error) { return f.tuple, f.lookErr }
	export := func(_ context.Context, id string) ([]byte, error) {
		f.exported = append(f.exported, id)
		if f.exportFn != nil {
			return f.exportFn(id)
		}
		return []byte(f.out), nil
	}
	r := NewOpenCodeReader(session, export, slog.New(slog.NewJSONHandler(&f.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r.now = func() time.Time { return time.UnixMilli(61004).Add(5 * time.Second) }
	return r
}

func openCodeAgent(digest string) domain.Agent {
	return domain.Agent{Key: domain.Key{PaneID: "p1", TerminalID: "t1", SessionDigest: digest}, Kind: "opencode"}
}

func TestOpenCodeLastReply(t *testing.T) {
	f := &openCodeFixture{tuple: openCodeTuple, out: openCodeTurn}
	reply, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest()))
	if err != nil {
		t.Fatalf("LastReply: %v", err)
	}
	if want := "Endpoint is **live** now.\n\nClick refresh."; reply.Text != want {
		t.Fatalf("Text = %q, want %q", reply.Text, want)
	}
	if len(f.exported) != 1 || f.exported[0] != "ses_abc" {
		t.Fatalf("exported %v, want the session id once", f.exported)
	}
	m := reply.Meta
	if m.Model != "codigo-3" || m.OutputTokens != 100 || !m.Started.Equal(time.UnixMilli(1000)) || !m.Ended.Equal(time.UnixMilli(61004)) {
		t.Fatalf("Meta = %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0] != "docs/plan.md" {
		t.Fatalf("Files = %v, want the one edited file once", m.Files)
	}
	if d, ok := m.Duration(); !ok || d != time.Minute+4*time.Millisecond {
		t.Fatalf("Duration = %v, %v", d, ok)
	}
	if !reply.Written.Equal(m.Ended) || reply.Age != 5*time.Second {
		t.Fatalf("Written = %v, Age = %v", reply.Written, reply.Age)
	}
	// The session value is transient: never in the Reply or the log.
	if strings.Contains(reply.Source, "ses_abc") || strings.Contains(f.logBuf.String(), "ses_abc") {
		t.Fatalf("session value leaked: source %q, log %s", reply.Source, f.logBuf.String())
	}
}

func TestOpenCodeLastReplyV2Export(t *testing.T) {
	const exported = `{"info":{"id":"ses_abc"},"messages":[
		{"type":"user","time":{"created":1000},"text":"current prompt"},
		{"type":"assistant","time":{"created":1001,"completed":2000},"model":{"id":"model-v2"},
		 "tokens":{"output":12},"content":[{"type":"reasoning","text":"private"},{"type":"text","text":"Ready **now**."}]},
		{"type":"idle","time":{"created":2001}}]}`
	f := &openCodeFixture{tuple: openCodeTuple, out: exported}
	reply, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "Ready **now**." || reply.Meta.Model != "model-v2" || reply.Meta.OutputTokens != 12 ||
		!reply.Meta.Started.Equal(time.UnixMilli(1000)) || !reply.Written.Equal(time.UnixMilli(2000)) {
		t.Fatalf("v2 reply = %+v", reply)
	}
}

// A mapping entry without a session digest cannot prove the pane identity.
func TestOpenCodeLastReplySessionlessKey(t *testing.T) {
	f := &openCodeFixture{tuple: openCodeTuple, out: openCodeTurn}
	if _, err := f.reader().LastReply(context.Background(), openCodeAgent("")); !errors.Is(err, domain.ErrNoReply) || len(f.exported) != 0 {
		t.Fatalf("LastReply: %v, exported %v", err, f.exported)
	}
}

func TestOpenCodeLastReplyNoReply(t *testing.T) {
	other := openCodeTuple
	other.Value = "ses_other"
	cases := map[string]struct {
		fixture    openCodeFixture
		agent      domain.Agent
		wantExport bool
	}{
		"unsupported kind": {openCodeFixture{tuple: openCodeTuple, out: openCodeTurn},
			domain.Agent{Key: domain.Key{PaneID: "p1"}, Kind: "claude"}, false},
		"lookup fails": {openCodeFixture{lookErr: errors.New("herdr down")},
			openCodeAgent(openCodeTuple.Digest()), false},
		"no session": {openCodeFixture{},
			openCodeAgent(openCodeTuple.Digest()), false},
		"not an id session": {openCodeFixture{tuple: domain.SessionTuple{Source: "herdr:opencode", Agent: "opencode", Kind: "path", Value: "/x"}},
			openCodeAgent(openCodeTuple.Digest()), false},
		"incomplete tuple": {openCodeFixture{tuple: domain.SessionTuple{Agent: "opencode", Kind: "id", Value: "ses_abc"}},
			openCodeAgent(openCodeTuple.Digest()), false},
		"pane runs another session": {openCodeFixture{tuple: other, out: openCodeTurn},
			openCodeAgent(openCodeTuple.Digest()), false},
		"export fails": {openCodeFixture{tuple: openCodeTuple, exportFn: func(string) ([]byte, error) { return nil, errors.New("exit status 1") }},
			openCodeAgent(openCodeTuple.Digest()), true},
		"not json": {openCodeFixture{tuple: openCodeTuple, out: "Exporting session..."},
			openCodeAgent(openCodeTuple.Digest()), true},
		"another session exported": {openCodeFixture{tuple: openCodeTuple, out: `{"info":{"id":"ses_zzz"},"messages":[]}`},
			openCodeAgent(openCodeTuple.Digest()), true},
		"no text after the prompt": {openCodeFixture{tuple: openCodeTuple, out: `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"run it"}]},
			{"info":{"role":"assistant","time":{"created":2}},"parts":[{"type":"tool","tool":"bash"}]}]}`},
			openCodeAgent(openCodeTuple.Digest()), true},
		"assistant without user prompt": {openCodeFixture{tuple: openCodeTuple, out: `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"assistant"},"parts":[{"type":"text","text":"orphan"}]}]}`},
			openCodeAgent(openCodeTuple.Digest()), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := tc.fixture
			_, err := f.reader().LastReply(context.Background(), tc.agent)
			if !errors.Is(err, domain.ErrNoReply) {
				t.Fatalf("err = %v, want ErrNoReply", err)
			}
			if got := len(f.exported) > 0; got != tc.wantExport {
				t.Fatalf("export ran = %v, want %v", got, tc.wantExport)
			}
		})
	}
}

func TestOpenCodeLastReplyCancelled(t *testing.T) {
	f := &openCodeFixture{tuple: openCodeTuple, out: openCodeTurn}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.reader().LastReply(ctx, openCodeAgent("")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestOpenCodeLastReplySafeFailures(t *testing.T) {
	secret := "ses_private secret_private"
	for _, tc := range []struct {
		name string
		f    openCodeFixture
	}{
		{"lookup", openCodeFixture{lookErr: errors.New(secret)}},
		{"export", openCodeFixture{tuple: openCodeTuple, exportFn: func(string) ([]byte, error) { return nil, errors.New(secret) }}},
		{"json", openCodeFixture{tuple: openCodeTuple, out: secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			_, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest()))
			if !errors.Is(err, domain.ErrNoReply) || strings.Contains(err.Error(), secret) || strings.Contains(f.logBuf.String(), secret) {
				t.Fatalf("unsafe failure: %v", err)
			}
		})
	}
}

func TestOpenCodeLastReplyContextErrors(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		f := &openCodeFixture{tuple: openCodeTuple, lookErr: cause}
		if _, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest())); !errors.Is(err, cause) {
			t.Fatalf("lookup = %v, want %v", err, cause)
		}
		f.lookErr = nil
		f.exportFn = func(string) ([]byte, error) { return nil, cause }
		if _, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest())); !errors.Is(err, cause) {
			t.Fatalf("export = %v, want %v", err, cause)
		}
	}
}

func TestOpenCodeLastReplyExporterTimeoutIsUnavailable(t *testing.T) {
	f := &openCodeFixture{tuple: openCodeTuple, exportFn: func(string) ([]byte, error) {
		return nil, errors.New("opencode export: timed out")
	}}
	parent := context.Background()
	_, err := f.reader().LastReply(parent, openCodeAgent(openCodeTuple.Digest()))
	if !errors.Is(err, domain.ErrNoReply) || errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
		t.Fatalf("export timeout = %v; parent = %v", err, parent.Err())
	}
	if strings.Contains(err.Error(), "ses_abc") || strings.Contains(f.logBuf.String(), "ses_abc") {
		t.Fatalf("session leaked: %v", err)
	}
}

func TestOpenCodeReaderStopsAfterLookupCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exported := false
	r := NewOpenCodeReader(
		func(context.Context, string) (domain.SessionTuple, error) { cancel(); return openCodeTuple, nil },
		func(context.Context, string) ([]byte, error) { exported = true; return nil, nil }, nil)
	if _, err := r.LastReply(ctx, openCodeAgent(openCodeTuple.Digest())); !errors.Is(err, context.Canceled) || exported {
		t.Fatalf("lookup cancellation = %v, exported = %v", err, exported)
	}
}

func TestOpenCodeLastReplyCountsCompletedFiles(t *testing.T) {
	const doc = `{"info":{"id":"ses_abc"},"messages":[{"info":{"role":"user"}},
	{"info":{"role":"assistant","time":{"completed":42}},"parts":[
	{"type":"tool","tool":"edit","state":{"status":"error","input":{"filePath":"failed.go"}}},
	{"type":"tool","tool":"write","state":{"status":"pending","input":{"filePath":"pending.go"}}},
	{"type":"tool","tool":"write","state":{"status":"completed","input":{"filePath":"ok.go"}}},
	{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"ok.go"}}},
	{"type":"text","text":"готово"}]}]}`
	f := &openCodeFixture{tuple: openCodeTuple, out: doc}
	r, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest()))
	if err != nil || r.Text != "готово" || len(r.Meta.Files) != 1 || r.Meta.Files[0] != "ok.go" || !r.Written.Equal(time.UnixMilli(42)) {
		t.Fatalf("reply = %+v, %v", r, err)
	}
}

func TestOpenCodeReaderKeepsPaneSessionsSeparate(t *testing.T) {
	tuples := map[string]domain.SessionTuple{
		"p1": openCodeTuple,
		"p2": {Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_two"},
	}
	var exported []string
	r := NewOpenCodeReader(
		func(_ context.Context, pane string) (domain.SessionTuple, error) { return tuples[pane], nil },
		func(_ context.Context, id string) ([]byte, error) {
			exported = append(exported, id)
			return []byte(`{"info":{"id":"` + id + `"},"messages":[{"info":{"role":"user"}},` +
				`{"info":{"role":"assistant"},"parts":[{"type":"text","text":"reply for ` + id + `"}]}]}`), nil
		}, nil)
	for pane, tuple := range tuples {
		a := openCodeAgent(tuple.Digest())
		a.PaneID = pane
		got, err := r.LastReply(context.Background(), a)
		if err != nil || got.Text != "reply for "+tuple.Value {
			t.Fatalf("pane %s: %+v, %v", pane, got, err)
		}
	}
	old := openCodeAgent(openCodeTuple.Digest())
	tuples["p1"] = domain.SessionTuple{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_new"}
	if _, err := r.LastReply(context.Background(), old); !errors.Is(err, domain.ErrNoReply) || len(exported) != 2 {
		t.Fatalf("changed session = %v, exported %v", err, exported)
	}
	old.SessionDigest = tuples["p1"].Digest()
	if _, err := r.LastReply(context.Background(), old); err != nil || len(exported) != 3 || exported[2] != "ses_new" {
		t.Fatalf("reconciled session = %v, exported %v", err, exported)
	}
}

// TestOpenCodeLastReplyTurnStillRunning: opencode completes a message and
// can report done or idle at every step, so a read whose newest record is
// tool work, or that has no assistant text yet, answers ErrReplyPending
// instead of a fragment. ErrReplyPending is still an ErrNoReply.
func TestOpenCodeLastReplyTurnStillRunning(t *testing.T) {
	for name, out := range map[string]string{
		"last record is a tool": `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},
			{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"first step"}]},
			{"info":{"role":"assistant","time":{"created":4}},"parts":[{"type":"tool","tool":"bash","state":{"status":"completed"}}]}]}`,
		"tool after text in one message": `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},
			{"info":{"role":"assistant","time":{"created":2}},"parts":[{"type":"text","text":"let me look"},{"type":"tool","tool":"read"}]}]}`,
		"no assistant message yet": `{"info":{"id":"ses_abc"},"messages":[
			{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]}]}`,
		"v2 tool last": `{"info":{"id":"ses_abc"},"messages":[
			{"type":"user","time":{"created":1},"content":[{"type":"text","text":"go"}]},
			{"type":"assistant","time":{"created":2},"content":[{"type":"text","text":"checking"},{"type":"tool","tool":"bash"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &openCodeFixture{tuple: openCodeTuple, out: out}
			_, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest()))
			if !errors.Is(err, domain.ErrReplyPending) || !errors.Is(err, domain.ErrNoReply) {
				t.Fatalf("err = %v, want ErrReplyPending", err)
			}
			if !strings.Contains(f.logBuf.String(), "opencode reply pending") || strings.Contains(f.logBuf.String(), "ses_abc") {
				t.Fatalf("pending log missing or leaks the session: %s", f.logBuf.String())
			}
		})
	}
}

// TestOpenCodeLastReplyBookkeepingAfterText: reasoning, patches and step
// markers after the final text do not make the turn pending.
func TestOpenCodeLastReplyBookkeepingAfterText(t *testing.T) {
	const doc = `{"info":{"id":"ses_abc"},"messages":[
		{"info":{"role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},
		{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[
			{"type":"tool","tool":"bash","state":{"status":"completed"}},
			{"type":"text","text":"All done."},
			{"type":"reasoning","text":"wrap up"},
			{"type":"patch"},
			{"type":"step-finish"}]}]}`
	f := &openCodeFixture{tuple: openCodeTuple, out: doc}
	r, err := f.reader().LastReply(context.Background(), openCodeAgent(openCodeTuple.Digest()))
	if err != nil || r.Text != "All done." {
		t.Fatalf("reply = %q, %v", r.Text, err)
	}
}
