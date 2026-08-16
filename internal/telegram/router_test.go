package telegram

import (
	"context"
	"testing"
)

func TestRouterParsesMentionAndReplyRoot(t *testing.T) {
	var got Incoming
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{7: true}, Dispatch: func(_ context.Context, msg Incoming) { got = msg }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 9, From: &User{ID: 4, FirstName: "A"}, Chat: Chat{ID: 7, Type: "group"}, Text: "@jeff #api fix it", Entities: []Entity{{Type: "mention", Offset: 0, Length: 5}}, ReplyToMessage: &Message{MessageID: 3}}})
	if got.Conversation.RootMessageID != 3 || got.RequestedProject != "api" || got.Text != "fix it" {
		t.Fatalf("got %+v", got)
	}
}
func TestRouterRejectsUnallowedChat(t *testing.T) {
	called := false
	r := &Router{Allowed: map[int64]bool{}, Dispatch: func(context.Context, Incoming) { called = true }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 1, From: &User{ID: 1}, Chat: Chat{ID: 7, Type: "private"}, Text: "hello"}})
	if called {
		t.Fatal("dispatched unallowed chat")
	}
}
