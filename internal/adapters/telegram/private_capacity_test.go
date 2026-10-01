package telegram_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

func strangerText(update, actor int64) *models.Update {
	return &models.Update{ID: update, Message: &models.Message{
		ID:   int(update),
		Date: 100,
		Chat: models.Chat{ID: actor, Type: models.ChatTypePrivate},
		From: &models.User{ID: actor, FirstName: "name"},
		Text: "hello",
	}}
}

// TestPrivateCapacityNoticeIsRateLimited: with the recipient directory full,
// every stranger message used to send a capacity notice, each blocking the
// synchronous poller for up to a second. The notice now goes once per actor
// and under a global cap.
func TestPrivateCapacityNoticeIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.gw.SetPrivateRegistration(func(context.Context, domain.PrivateContact) (bool, error) {
		return false, domain.ErrRecipientCapacity
	})
	h.api.on("sendMessage", func(url.Values) apiReply { return okReply(map[string]any{"message_id": 1}) })
	ctx := context.Background()
	update := int64(1)
	for range 5 {
		h.bot.ProcessUpdate(ctx, strangerText(update, 100))
		update++
	}
	if n := len(h.api.callsOf("sendMessage")); n != 1 {
		t.Fatalf("one actor got %d capacity notices, want 1", n)
	}
	for actor := int64(200); actor < 300; actor++ {
		h.bot.ProcessUpdate(ctx, strangerText(update, actor))
		update++
	}
	if n := len(h.api.callsOf("sendMessage")); n > 10 {
		t.Fatalf("%d capacity notices for a stranger flood, want a global cap", n)
	}
}
