package telegram

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot/models"

	"altrdb-telegram-bot/internal/model"
)

// access is the outcome of the entry checks every update goes through.
type access struct {
	chatID    int64
	userID    int64
	chatType  string
	username  string
	firstName string

	// allowed is false when the update must be dropped.
	allowed bool
	// refuse is the explanation to send back, if any. Sending nothing is the
	// right answer for a stranger in an allowlisted deployment: replying to
	// everyone who finds the bot is a spam vector.
	refuse string
	// alert means the refusal goes into the callback toast rather than a
	// message.
	alert bool
}

// refuseCooldown stops a refused chat from being answered on every keystroke.
const refuseCooldown = time.Hour

type refuseLog struct {
	mu   sync.Mutex
	seen map[int64]time.Time
}

func newRefuseLog() *refuseLog { return &refuseLog{seen: map[int64]time.Time{}} }

// once reports whether chatID may be answered again, recording the time when it
// says yes.
func (r *refuseLog) once(chatID int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.seen[chatID]; ok && now.Sub(last) < refuseCooldown {
		return false
	}
	r.seen[chatID] = now
	// Keep the map from growing without bound.
	if len(r.seen) > 4096 {
		for id, t := range r.seen {
			if now.Sub(t) >= refuseCooldown {
				delete(r.seen, id)
			}
		}
	}
	return true
}

// guardMessage decides whether a message may drive the bot.
func (bt *Bot) guardMessage(ctx context.Context, m *models.Message) access {
	a := access{
		chatID:   m.Chat.ID,
		chatType: string(m.Chat.Type),
		username: m.Chat.Username,
	}
	if m.From != nil {
		a.userID = m.From.ID
		a.username = m.From.Username
		a.firstName = m.From.FirstName
	}
	if a.firstName == "" {
		a.firstName = m.Chat.FirstName
	}
	return bt.guard(ctx, a)
}

// guardCallback does the same for a button press. The chat is taken from the
// message the button is attached to, never from the pressing user, so a
// forwarded panel cannot be used to drive somebody else's subscription.
func (bt *Bot) guardCallback(ctx context.Context, cb *models.CallbackQuery) access {
	a := access{chatID: cb.From.ID, chatType: string(models.ChatTypePrivate), userID: cb.From.ID,
		username: cb.From.Username, firstName: cb.From.FirstName}

	switch cb.Message.Type {
	case models.MaybeInaccessibleMessageTypeMessage:
		if cb.Message.Message != nil {
			msg := cb.Message.Message
			a.chatID = msg.Chat.ID
			a.chatType = string(msg.Chat.Type)
			if msg.Chat.Type == models.ChatTypePrivate {
				a.username = msg.Chat.Username
				a.firstName = msg.Chat.FirstName
			}
		}
	case models.MaybeInaccessibleMessageTypeInaccessibleMessage:
		if cb.Message.InaccessibleMessage != nil {
			a.chatID = cb.Message.InaccessibleMessage.Chat.ID
			a.chatType = string(cb.Message.InaccessibleMessage.Chat.Type)
		}
	}
	return bt.guard(ctx, a)
}

// guard applies the chat-type and allowlist policy.
func (bt *Bot) guard(ctx context.Context, a access) access {
	now := bt.clock.Now()

	if a.chatType != string(models.ChatTypePrivate) && !bt.cfg.AllowGroups {
		a.allowed = false
		if bt.refusals.once(a.chatID, now) {
			a.refuse = "This bot works in private chats only. Message it directly to set up notifications."
		}
		bt.rejected.Add(1)
		return a
	}

	if !bt.cfg.AllowUser(a.userID) {
		a.allowed = false
		a.alert = true
		if bt.refusals.once(a.chatID, now) {
			a.refuse = "This bot is restricted to an allowlist of Telegram user ids. " +
				"Ask the operator to add ALLOWED_TELEGRAM_USER_IDS."
		}
		bt.rejected.Add(1)
		return a
	}

	a.allowed = true
	return a
}

// ensureChat registers or refreshes the chat row. Every command path calls it
// first, so a user who starts with /states instead of /start still works.
func (bt *Bot) ensureChat(ctx context.Context, a access) (*model.Chat, error) {
	chat, err := bt.store.GetChat(ctx, a.chatID)
	if err == nil {
		if uerr := bt.store.UpdateChatIdentity(ctx, a.chatID, a.chatType, a.username, a.firstName); uerr != nil {
			bt.log.Warn("updating chat identity failed", "chat_id", a.chatID, "error", uerr.Error())
		}
		return chat, nil
	}
	return bt.store.RegisterChat(ctx, a.chatID, a.chatType, a.username, a.firstName)
}

// handleMyChatMember reacts to being blocked, unblocked, kicked or re-added.
// Without it a chat that blocked the bot stays "enabled" and the delivery
// worker only finds out the hard way, one failed send at a time.
func (bt *Bot) handleMyChatMember(ctx context.Context, u *models.ChatMemberUpdated) {
	if u == nil || u.Chat.ID == 0 {
		return
	}
	newStatus := strings.ToLower(string(u.NewChatMember.Type))
	oldStatus := strings.ToLower(string(u.OldChatMember.Type))
	if newStatus == oldStatus {
		return
	}

	switch newStatus {
	case "kicked", "left":
		if err := bt.store.DisableChat(ctx, u.Chat.ID, "bot was "+newStatus+" from the chat"); err != nil {
			bt.log.Warn("disabling chat failed", "chat_id", u.Chat.ID, "error", err.Error())
			return
		}
		bt.log.Info("chat disabled because the bot was removed",
			"chat_id", u.Chat.ID, "status", newStatus)
	case "member", "administrator":
		chat, err := bt.store.GetChat(ctx, u.Chat.ID)
		if err != nil {
			// Never talked to us; nothing to re-enable.
			return
		}
		if chat.Enabled {
			return
		}
		if err := bt.store.SetChatEnabled(ctx, u.Chat.ID, true, ""); err != nil {
			bt.log.Warn("re-enabling chat failed", "chat_id", u.Chat.ID, "error", err.Error())
			return
		}
		bt.log.Info("chat re-enabled because the bot was added back", "chat_id", u.Chat.ID)
		bt.send(ctx, u.Chat.ID, "▶ Notifications resumed. Use /settings to review your subscription.", nil)
	}
}
