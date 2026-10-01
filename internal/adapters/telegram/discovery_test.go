package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/permgps/herdr-telegram-agents/internal/adapters/telegram"
	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

func promotion(chatID int64, title string, isForum bool, from models.User, rights bool) *models.Update {
	return &models.Update{ID: 7, MyChatMember: &models.ChatMemberUpdated{
		Chat: models.Chat{ID: chatID, Type: models.ChatTypeSupergroup, Title: title, IsForum: isForum},
		From: from,
		Date: int(time.Now().Unix()),
		NewChatMember: models.ChatMember{
			Type:          models.ChatMemberTypeAdministrator,
			Administrator: &models.ChatMemberAdministrator{CanManageTopics: rights},
		},
	}}
}

func newProbe(t *testing.T) (*telegram.Probe, *fakeAPI, *logBuffer) {
	t.Helper()
	api := newFakeAPI(t)
	// A long-poll that answers instantly would spin; slow it down a little.
	api.on("getUpdates", func(url.Values) apiReply {
		time.Sleep(20 * time.Millisecond)
		return okReply([]any{})
	})
	api.on("getMe", func(url.Values) apiReply {
		return okReply(map[string]any{"id": 42, "is_bot": true, "first_name": "Agents", "username": "agents_bot"})
	})
	log, buf := newTestLog(t)
	p, err := telegram.NewProbe(testToken, log, bot.WithServerURL(api.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Close()
		assertNoSecret(t, buf)
	})
	return p, api, buf
}

func recvCandidate(t *testing.T, ch <-chan domain.GroupCandidate) domain.GroupCandidate {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no candidate")
		return domain.GroupCandidate{}
	}
}

func noCandidate(t *testing.T, ch <-chan domain.GroupCandidate) {
	t.Helper()
	select {
	case c := <-ch:
		t.Fatalf("unexpected candidate %+v", c)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProbeIdentityKeepsPendingUpdates(t *testing.T) {
	p, api, _ := newProbe(t)
	id, err := p.Identity(ctxT(t))
	if err != nil || id.ID != 42 || id.Username != "agents_bot" {
		t.Fatalf("Identity = %+v, %v", id, err)
	}
	if got := api.methods(); len(got) != 2 || got[0] != "getMe" || got[1] != "deleteWebhook" {
		t.Fatalf("methods = %v", got)
	}
	if v := api.callsOf("deleteWebhook")[0].form.Get("drop_pending_updates"); v == "true" {
		t.Fatalf("drop_pending_updates = %q, want unset or false", v)
	}
}

func TestProbeIdentityRejectsBadToken(t *testing.T) {
	p, api, _ := newProbe(t)
	api.on("getMe", func(url.Values) apiReply { return errReply(401, "Unauthorized") })
	if _, err := p.Identity(ctxT(t)); !errors.Is(err, domain.ErrBotUnauthorized) {
		t.Fatalf("Identity = %v", err)
	}
}

func TestProbeCandidates(t *testing.T) {
	p, api, _ := newProbe(t)
	api.on("getChat", func(url.Values) apiReply {
		return okReply(map[string]any{"id": -300, "type": "supergroup", "title": "Late forum", "is_forum": true})
	})
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	ch, err := p.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Candidates(ctx); err == nil {
		t.Fatal("second Candidates should fail")
	}
	alex := models.User{ID: 7, Username: "alex"}

	p.Process(ctx, promotion(-100, "Agents", true, alex, true))
	c := recvCandidate(t, ch)
	if c.ChatID != -100 || c.Title != "Agents" || c.FromID != 7 || c.FromUsername != "alex" {
		t.Fatalf("candidate = %+v", c)
	}

	p.Process(ctx, promotion(-100, "Agents", true, alex, true))
	noCandidate(t, ch)

	p.Process(ctx, promotion(-200, "No rights", true, alex, false))
	noCandidate(t, ch)

	// Flag missing on the wire: getChat confirms it is a forum.
	p.Process(ctx, promotion(-300, "", false, alex, true))
	c = recvCandidate(t, ch)
	if c.ChatID != -300 || c.Title != "Late forum" {
		t.Fatalf("candidate via getChat = %+v", c)
	}

	api.on("getChat", func(url.Values) apiReply {
		return okReply(map[string]any{"id": -400, "type": "supergroup", "title": "Plain group", "is_forum": false})
	})
	p.Process(ctx, promotion(-400, "Plain group", false, alex, true))
	noCandidate(t, ch)

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, open := <-ch; open {
		t.Fatal("channel still open after Close")
	}
}

func privateText(from models.User, text string) *models.Update {
	return &models.Update{ID: 8, Message: &models.Message{
		ID: 1, Chat: models.Chat{ID: from.ID, Type: models.ChatTypePrivate}, From: &from, Text: text,
	}}
}

func chatShared(from models.User, requestID int, chatID int64, title string) *models.Update {
	return &models.Update{ID: 9, Message: &models.Message{
		ID: 2, Chat: models.Chat{ID: from.ID, Type: models.ChatTypePrivate}, From: &from,
		ChatShared: &models.ChatShared{RequestID: requestID, ChatID: chatID, Title: title},
	}}
}

// lastReply returns the text and parsed reply_markup of the newest sendMessage.
func lastReply(t *testing.T, api *fakeAPI) (string, map[string]any) {
	t.Helper()
	calls := api.callsOf("sendMessage")
	if len(calls) == 0 {
		t.Fatal("no sendMessage")
	}
	f := calls[len(calls)-1].form
	var markup map[string]any
	if raw := f.Get("reply_markup"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &markup); err != nil {
			t.Fatalf("reply_markup %q: %v", raw, err)
		}
	}
	return f.Get("text"), markup
}

func TestProbeStartSendsGroupButton(t *testing.T) {
	p, api, _ := newProbe(t)
	ctx := ctxT(t)
	alex := models.User{ID: 7, Username: "alex"}
	p.Process(ctx, privateText(alex, "/start setup"))
	text, markup := lastReply(t, api)
	if !strings.Contains(text, "Manage topics") {
		t.Fatalf("text = %q", text)
	}
	if api.callsOf("sendMessage")[0].form.Get("chat_id") != "7" {
		t.Fatalf("chat_id = %q", api.callsOf("sendMessage")[0].form.Get("chat_id"))
	}
	button := markup["keyboard"].([]any)[0].([]any)[0].(map[string]any)
	req := button["request_chat"].(map[string]any)
	if button["text"] != "Choose group" || req["request_id"] != float64(1) || req["chat_is_forum"] != true || req["request_title"] != true {
		t.Fatalf("button = %v", button)
	}
	if req["chat_is_channel"] != false {
		t.Fatalf("chat_is_channel = %v", req["chat_is_channel"])
	}
	if rights := req["bot_administrator_rights"].(map[string]any); rights["can_manage_topics"] != true || rights["can_delete_messages"] != true || rights["can_pin_messages"] != true {
		t.Fatalf("bot rights = %v", rights)
	}
	if rights := req["user_administrator_rights"].(map[string]any); rights["can_manage_topics"] != true || rights["can_delete_messages"] != true || rights["can_pin_messages"] != true || rights["can_promote_members"] != true {
		t.Fatalf("user rights = %v", rights)
	}
}

func TestProbeChatShared(t *testing.T) {
	p, api, _ := newProbe(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	if _, err := p.Identity(ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := p.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alex := models.User{ID: 7, Username: "alex"}
	api.on("getChat", func(f url.Values) apiReply {
		return okReply(map[string]any{"id": -100, "type": "supergroup", "title": "Agents (full)", "is_forum": true})
	})
	api.on("getChatMember", func(f url.Values) apiReply {
		if f.Get("user_id") != "42" {
			t.Errorf("getChatMember user_id = %q, want the bot id", f.Get("user_id"))
		}
		return memberReply("administrator", true)
	})

	p.Process(ctx, chatShared(alex, 1, -100, "Agents"))
	c := recvCandidate(t, ch)
	if c.ChatID != -100 || c.Title != "Agents (full)" || c.FromID != 7 || c.FromUsername != "alex" {
		t.Fatalf("candidate = %+v", c)
	}
	text, markup := lastReply(t, api)
	if !strings.Contains(text, "Connected") || markup["remove_keyboard"] != true {
		t.Fatalf("reply = %q %v", text, markup)
	}

	// The same group chosen twice is reported once, but the user still gets an answer.
	p.Process(ctx, chatShared(alex, 1, -100, "Agents"))
	noCandidate(t, ch)
	if n := len(api.callsOf("sendMessage")); n != 2 {
		t.Fatalf("sendMessage calls = %d", n)
	}

	// A promotion seen by hand for the same group is a duplicate too.
	p.Process(ctx, promotion(-100, "Agents", true, alex, true))
	noCandidate(t, ch)

	// Foreign request ids are ignored.
	p.Process(ctx, chatShared(alex, 5, -500, "Other"))
	noCandidate(t, ch)

	// Bot added without the topic right: hint and the button again.
	api.on("getChatMember", func(url.Values) apiReply { return memberReply("administrator", false) })
	p.Process(ctx, chatShared(alex, 1, -200, "No rights"))
	noCandidate(t, ch)
	text, markup = lastReply(t, api)
	if !strings.Contains(text, "Manage topics") || markup["keyboard"] == nil {
		t.Fatalf("reply = %q %v", text, markup)
	}

	// Topics disabled.
	api.on("getChat", func(url.Values) apiReply {
		return okReply(map[string]any{"id": -300, "type": "supergroup", "title": "Plain", "is_forum": false})
	})
	p.Process(ctx, chatShared(alex, 1, -300, "Plain"))
	noCandidate(t, ch)
	if text, _ := lastReply(t, api); !strings.Contains(text, "Topics disabled") {
		t.Fatalf("reply = %q", text)
	}

	// Bot not in the chat at all.
	api.on("getChat", func(url.Values) apiReply { return errReply(400, "Bad Request: chat not found") })
	p.Process(ctx, chatShared(alex, 1, -400, "Gone"))
	noCandidate(t, ch)
	if text, _ := lastReply(t, api); !strings.Contains(text, "cannot see") {
		t.Fatalf("reply = %q", text)
	}
}

// TestProbeBindsToSetupCode: while setup runs, anyone can find the bot and
// message it. Only the user who opened the one-time link from the wizard
// gets the group button and may choose the group; a promotion by an
// anonymous admin (GroupAnonymousBot) or a bot is never a candidate.
func TestProbeBindsToSetupCode(t *testing.T) {
	p, api, _ := newProbe(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	if _, err := p.Identity(ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := p.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Bind("c0de")
	api.on("getChat", func(url.Values) apiReply {
		return okReply(map[string]any{"id": -100, "type": "supergroup", "title": "Agents", "is_forum": true})
	})
	api.on("getChatMember", func(url.Values) apiReply { return memberReply("administrator", true) })
	alex := models.User{ID: 7, Username: "alex"}
	mallory := models.User{ID: 66, Username: "mallory"}

	p.Process(ctx, privateText(mallory, "/start setup"))
	p.Process(ctx, privateText(mallory, "/start setup_wrong"))
	if n := len(api.callsOf("sendMessage")); n != 0 {
		t.Fatalf("stranger got %d replies", n)
	}
	p.Process(ctx, chatShared(mallory, 1, -100, "Agents"))
	noCandidate(t, ch)

	anonymous := models.User{ID: 1087968824, Username: "GroupAnonymousBot", IsBot: true}
	p.Process(ctx, promotion(-100, "Agents", true, anonymous, true))
	noCandidate(t, ch)

	p.Process(ctx, privateText(alex, "/start setup_c0de"))
	if text, _ := lastReply(t, api); !strings.Contains(text, "Manage topics") {
		t.Fatalf("owner reply = %q", text)
	}
	p.Process(ctx, chatShared(alex, 1, -100, "Agents"))
	if c := recvCandidate(t, ch); c.FromID != 7 {
		t.Fatalf("candidate = %+v", c)
	}
}

// TestProbeHoldsPromotionsUntilOwnerKnown: after Bind and before the link
// holder sends /start setup_<code>, any user could add the bot as an
// admin with "Manage topics" to their own forum and become the only
// candidate (and the operator). Promotions in that window are held and
// re-evaluated once the owner is known: only the owner's count, in either
// order, and a promotion dated before setup started never counts.
func TestProbeHoldsPromotionsUntilOwnerKnown(t *testing.T) {
	p, api, _ := newProbe(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	if _, err := p.Identity(ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := p.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Bind("c0de")
	alex := models.User{ID: 7, Username: "alex"}
	mallory := models.User{ID: 66, Username: "mallory"}

	// Before the owner is known: the stranger's promotion is not a candidate.
	p.Process(ctx, promotion(-555, "Agents", true, mallory, true))
	noCandidate(t, ch)
	// The owner promoted the bot by hand before opening the link.
	p.Process(ctx, promotion(-100, "Mine", true, alex, true))
	noCandidate(t, ch)
	// A promotion from before setup started (a pending update) never counts.
	stale := promotion(-777, "Old", true, alex, true)
	stale.MyChatMember.Date = int(time.Now().Add(-2 * time.Hour).Unix())
	p.Process(ctx, stale)
	noCandidate(t, ch)

	p.Process(ctx, privateText(alex, "/start setup_c0de"))
	if c := recvCandidate(t, ch); c.ChatID != -100 || c.FromID != 7 {
		t.Fatalf("candidate = %+v, want the owner's group", c)
	}
	noCandidate(t, ch)

	// After the owner is known: the stranger is still ignored, the owner counts.
	p.Process(ctx, promotion(-556, "Agents 2", true, mallory, true))
	noCandidate(t, ch)
	p.Process(ctx, promotion(-101, "Mine 2", true, alex, true))
	if c := recvCandidate(t, ch); c.ChatID != -101 || c.FromID != 7 {
		t.Fatalf("candidate = %+v", c)
	}
	if n := len(api.callsOf("getChat")); n != 0 {
		t.Fatalf("getChat calls = %d, want none for forum-flagged updates", n)
	}
}
