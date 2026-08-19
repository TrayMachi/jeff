package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
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
	InForumTopic     bool
}

type ForumClient interface {
	CreateForumTopic(context.Context, CreateForumTopicParams) (ForumTopic, error)
	SendMessage(context.Context, SendMessageParams) (Message, error)
}

type Router struct {
	BotUsername string
	Allowed     map[int64]bool
	ForumChatID int64
	Forum       ForumClient
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
	mentions := messageMentionsBot(msg, r.BotUsername)
	text := strings.TrimSpace(msg.Text)
	if mentions {
		text = stripBotMention(msg.Text, msg.Entities, r.BotUsername)
	}
	command, isCommand := conversation.ParseCommand(text, r.BotUsername)
	if isCommand && command.Name != "cancel" && command.Name != "stop" && command.Name != "status" && command.Name != "health" && command.Name != "project" {
		return
	}
	requested := ""
	if isCommand {
		switch command.Name {
		case "project":
			requested = command.Argument
			text = command.Prompt
		case "cancel", "stop", "status", "health":
			text = "/" + command.Name
		}
	} else {
		requested, text = conversation.ParsePrompt(text)
	}

	topLevel := msg.ReplyToMessage == nil && (msg.MessageThreadID == 0 || msg.MessageThreadID == 1)
	inConfiguredForum := r.ForumChatID != 0 && msg.Chat.ID == r.ForumChatID && msg.Chat.Type == "supergroup"
	newForumRequest := inConfiguredForum && topLevel && r.Forum != nil && !isCommand && (mentions || requested != "")
	if msg.Chat.Type != "private" && !mentions && !newForumRequest && !isCommand {
		return
	}
	if newForumRequest {
		r.handleNewForumRequest(ctx, msg, text, requested)
		return
	}

	incoming := r.incoming(msg, text, requested, command.Name)
	if r.TextHandler != nil && r.TextHandler(ctx, incoming) {
		return
	}
	if r.Dispatch != nil {
		r.Dispatch(ctx, incoming)
	}
}

func (r *Router) handleNewForumRequest(ctx context.Context, msg *Message, text, requested string) {
	name := topicName(requested, displayName(*msg.From), text)
	topic, err := r.Forum.CreateForumTopic(ctx, CreateForumTopicParams{ChatID: msg.Chat.ID, Name: name})
	if err != nil {
		slog.Warn("failed to create Telegram forum topic", "chat", msg.Chat.ID, "error", err)
		_, _ = r.Forum.SendMessage(ctx, SendMessageParams{ChatID: msg.Chat.ID, ReplyToMessageID: msg.MessageID, Text: "I couldn't create a request topic. Check that Jeff is an administrator with topic-management permission."})
		return
	}
	copied, err := r.Forum.SendMessage(ctx, SendMessageParams{ChatID: msg.Chat.ID, MessageThreadID: topic.MessageThreadID, Text: text})
	if err != nil {
		slog.Warn("failed to copy request into Telegram forum topic", "chat", msg.Chat.ID, "topic", topic.MessageThreadID, "error", err)
		_, _ = r.Forum.SendMessage(ctx, SendMessageParams{ChatID: msg.Chat.ID, ReplyToMessageID: msg.MessageID, Text: "I created the topic but couldn't copy the request into it, so nothing was started."})
		return
	}
	link := topicLink(msg.Chat, copied.MessageID, topic.MessageThreadID)
	ack := "Started topic."
	if link != "" {
		ack = "Started topic " + link
	}
	_, _ = r.Forum.SendMessage(ctx, SendMessageParams{ChatID: msg.Chat.ID, ReplyToMessageID: msg.MessageID, Text: ack})
	incoming := Incoming{
		Conversation:     conversation.Key{ChatID: msg.Chat.ID, TopicID: topic.MessageThreadID, RootMessageID: topic.MessageThreadID},
		MessageID:        copied.MessageID,
		ChatID:           msg.Chat.ID,
		TopicID:          topic.MessageThreadID,
		Text:             text,
		UserID:           msg.From.ID,
		UserName:         displayName(*msg.From),
		ChatType:         msg.Chat.Type,
		MentionsBot:      true,
		RequestedProject: requested,
		InForumTopic:     true,
	}
	if r.Dispatch != nil {
		r.Dispatch(ctx, incoming)
	}
}

func (r *Router) incoming(msg *Message, text, requested, command string) Incoming {
	key := conversation.RootFor(msg.Chat.ID, msg.MessageThreadID, msg.MessageID, replyRoot(msg))
	if msg.MessageThreadID != 0 {
		key.RootMessageID = msg.MessageThreadID
	}
	if msg.Chat.Type == "private" {
		key.RootMessageID = 0
	}
	return Incoming{Conversation: key, MessageID: msg.MessageID, ChatID: msg.Chat.ID, TopicID: msg.MessageThreadID, Text: text, UserID: msg.From.ID, UserName: displayName(*msg.From), ChatType: msg.Chat.Type, MentionsBot: messageMentionsBot(msg, r.BotUsername), RequestedProject: requested, Command: command, InForumTopic: r.ForumChatID != 0 && msg.Chat.ID == r.ForumChatID && msg.Chat.Type == "supergroup" && msg.MessageThreadID != 0}
}

func topicName(project, user, prompt string) string {
	parts := []string{}
	if project != "" {
		parts = append(parts, "#"+project)
	}
	if user != "" {
		parts = append(parts, user)
	}
	if prompt != "" {
		parts = append(parts, strings.Join(strings.Fields(prompt), " "))
	}
	name := strings.Join(parts, " — ")
	runes := []rune(name)
	if len(runes) > 128 {
		name = string(runes[:125]) + "..."
	}
	if name == "" {
		return "Jeff request"
	}
	return name
}

func topicLink(chat Chat, messageID, threadID int64) string {
	if messageID == 0 || threadID == 0 {
		return ""
	}
	if chat.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d?thread=%d", chat.Username, messageID, threadID)
	}
	id := strconv.FormatInt(chat.ID, 10)
	id = strings.TrimPrefix(id, "-100")
	if id == "" || strings.HasPrefix(id, "-") {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d?thread=%d", id, messageID, threadID)
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
