package events

import (
	"context"

	"github.com/local/jeff/internal/conversation"
)

type IncomingMessage struct {
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
	InForumTopic     bool
}
type Responder func(context.Context, IncomingMessage) error
