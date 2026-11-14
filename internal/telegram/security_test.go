package telegram

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"altrdb-telegram-bot/internal/config"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestWebhookRequiresConfiguredSecret(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.TelegramMode = config.ModeWebhook
		c.TelegramWebhookSecret = "long-test-webhook-secret"
	})
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { h.tg.API().StartWebhook(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for _, header := range []string{"", "wrong", h.cfg.TelegramWebhookSecret} {
		u := h.message("/start")
		data, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(data)))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
		h.tg.WebhookHandler().ServeHTTP(httptest.NewRecorder(), req)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(h.api.byMethod("sendMessage")) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := len(h.api.byMethod("sendMessage")); got != 1 {
		t.Fatalf("processed %d updates, want only authenticated update", got)
	}
	if got := h.tg.updates.Load(); got != 1 {
		t.Fatalf("dispatched %d updates, want 1", got)
	}
}

func TestMembershipCannotBypassPolicy(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowlist", true: "groups"}[group], func(t *testing.T) {
			h := newHarness(t, nil)
			h.send("/start")
			if err := h.store.DisableChat(h.ctx, testChatID, "paused"); err != nil {
				t.Fatal(err)
			}
			chatType := models.ChatTypePrivate
			if group {
				chatType = models.ChatTypeGroup
			} else {
				h.cfg.AllowedTelegramUsers = []int64{999}
			}
			h.tg.handleMyChatMember(h.ctx, &models.ChatMemberUpdated{
				Chat: models.Chat{ID: testChatID, Type: chatType}, From: models.User{ID: testUserID},
				OldChatMember: models.ChatMember{Type: models.ChatMemberTypeLeft}, NewChatMember: models.ChatMember{Type: models.ChatMemberTypeMember},
			})
			chat, err := h.store.GetChat(h.ctx, testChatID)
			if err != nil {
				t.Fatal(err)
			}
			if chat.Enabled {
				t.Fatal("unauthorized membership update enabled chat")
			}
		})
	}
}

func TestUnauthorizedDoesNotDisableUIChat(t *testing.T) {
	h := newHarness(t, nil)
	h.send("/start")
	h.tg.noteUnreachable(h.ctx, testChatID, bot.ErrorUnauthorized)
	chat, err := h.store.GetChat(h.ctx, testChatID)
	if err != nil {
		t.Fatal(err)
	}
	if !chat.Enabled {
		t.Fatal("global authentication fault disabled chat")
	}
}
