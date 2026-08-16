package telegram

import (
	"context"
	"strings"

	"github.com/local/jeff/internal/conversation"
)

type Incoming struct {
	Conversation     conversation.Key
	MessageID        int64
	ChatID           int64
	TopicID          int64
	Text             string
	UserID           int64
	UserName         string
	ChatType         string
	MentionsBot      bool
	RequestedProject string
	Command          string
	MentionUserID    int64
}

type Router struct {
	BotUsername string
	Allowed     map[int64]bool
	Dispatch    func(context.Context, Incoming)
	Callback    func(context.Context, CallbackQuery)
	TextHandler func(context.Context, Incoming) bool
}

func (r *Router) Handle(ctx context.Context, update Update) {
	if update.CallbackQuery != nil {
		query := update.CallbackQuery
		if query.Message == nil || !r.Allowed[query.Message.Chat.ID] {
			return
		}
		if r.Callback != nil {
			r.Callback(ctx, *query)
		}
		return
	}
	msg := update.Message
	if msg == nil || msg.From == nil || msg.From.IsBot || !r.Allowed[msg.Chat.ID] {
		return
	}
	command, isCommand := conversation.ParseCommand(msg.Text, r.BotUsername)
	if isCommand && command.Name != "cancel" && command.Name != "stop" && command.Name != "status" && command.Name != "project" {
		return
	}
	mentions := messageMentionsBot(msg, r.BotUsername)
	if msg.Chat.Type != "private" && !mentions && !isCommand {
		return
	}
	rootID := replyRoot(msg)
	key := conversation.RootFor(msg.Chat.ID, msg.MessageThreadID, msg.MessageID, rootID)
	if msg.Chat.Type == "private" {
		key.RootMessageID = 0
	}
	text := strings.TrimSpace(msg.Text)
	requested := ""
	if mentions {
		text = stripBotMention(msg.Text, msg.Entities, r.BotUsername)
	}
	if isCommand {
		switch command.Name {
		case "project":
			requested = command.Argument
			text = command.Prompt
		case "cancel", "stop", "status":
			text = "/" + command.Name
		}
	} else {
		requested, text = conversation.ParsePrompt(text)
	}
	incoming := Incoming{Conversation: key, MessageID: msg.MessageID, ChatID: msg.Chat.ID, TopicID: msg.MessageThreadID, Text: text, UserID: msg.From.ID, UserName: displayName(*msg.From), ChatType: msg.Chat.Type, MentionsBot: mentions, RequestedProject: requested, Command: command.Name}
	if r.TextHandler != nil && r.TextHandler(ctx, incoming) {
		return
	}
	if r.Dispatch != nil {
		r.Dispatch(ctx, incoming)
	}
}

func replyRoot(msg *Message) int64 {
	if msg == nil || msg.ReplyToMessage == nil {
		return 0
	}
	root := msg.ReplyToMessage
	for root.ReplyToMessage != nil {
		root = root.ReplyToMessage
	}
	return root.MessageID
}

func stripBotMention(text string, entities []Entity, username string) string {
	for i := len(entities) - 1; i >= 0; i-- {
		entity := entities[i]
		if entity.Type != "mention" || entity.Offset < 0 || entity.Offset+entity.Length > len(text) {
			continue
		}
		value := text[entity.Offset : entity.Offset+entity.Length]
		if strings.EqualFold(value, "@"+username) {
			text = strings.TrimSpace(text[:entity.Offset] + text[entity.Offset+entity.Length:])
		}
	}
	return text
}

func displayName(u User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}
func messageMentionsBot(msg *Message, username string) bool {
	if username == "" {
		return false
	}
	for _, e := range msg.Entities {
		if e.Type == "mention" && e.Offset >= 0 && e.Offset+e.Length <= len(msg.Text) && strings.EqualFold(msg.Text[e.Offset:e.Offset+e.Length], "@"+username) {
			return true
		}
	}
	return false
}
