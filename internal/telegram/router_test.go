package telegram

import (
	"context"
	"testing"
)

type fakeForum struct {
	topic   ForumTopic
	created []CreateForumTopicParams
	sent    []SendMessageParams
}

type fakeReactions struct {
	set []SetMessageReactionParams
}

func (f *fakeReactions) SetMessageReaction(_ context.Context, p SetMessageReactionParams) error {
	f.set = append(f.set, p)
	return nil
}

func (f *fakeForum) CreateForumTopic(_ context.Context, p CreateForumTopicParams) (ForumTopic, error) {
	f.created = append(f.created, p)
	return f.topic, nil
}
func (f *fakeForum) SendMessage(_ context.Context, p SendMessageParams) (Message, error) {
	f.sent = append(f.sent, p)
	return Message{MessageID: int64(len(f.sent) + 100)}, nil
}

func TestRouterCreatesForumTopicForTopLevelRequest(t *testing.T) {
	forum := &fakeForum{topic: ForumTopic{MessageThreadID: 77}}
	var got Incoming
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{-1007: true}, ForumChatID: -1007, Forum: forum, Dispatch: func(_ context.Context, m Incoming) { got = m }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 9, From: &User{ID: 4, FirstName: "A"}, Chat: Chat{ID: -1007, Type: "supergroup", Username: "main"}, Text: "@jeff #api fix it", Entities: []Entity{{Type: "mention", Offset: 0, Length: 5}}}})
	if len(forum.created) != 1 || forum.created[0].ChatID != -1007 {
		t.Fatalf("created=%+v", forum.created)
	}
	if got.TopicID != 77 || got.Conversation.TopicID != 77 || got.Conversation.RootMessageID != 77 || got.Text != "fix it" {
		t.Fatalf("got=%+v", got)
	}
	if len(forum.sent) != 2 || forum.sent[0].MessageThreadID != 77 || forum.sent[1].ReplyToMessageID != 9 {
		t.Fatalf("sent=%+v", forum.sent)
	}
	if got := forum.sent[1].Text; got != "Started topic https://t.me/main/101?thread=77" {
		t.Fatal(got)
	}
}
func TestForumTopLevelPromptDoesNotNeedMention(t *testing.T) {
	forum := &fakeForum{topic: ForumTopic{MessageThreadID: 78}}
	var got Incoming
	r := &Router{Allowed: map[int64]bool{-1007: true}, ForumChatID: -1007, Forum: forum, Dispatch: func(_ context.Context, m Incoming) { got = m }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 10, From: &User{ID: 4}, Chat: Chat{ID: -1007, Type: "supergroup"}, Text: "#api fix it"}})
	if got.TopicID != 78 || got.Text != "fix it" {
		t.Fatalf("got=%+v", got)
	}
}

func TestUnmentionedTopicDiscussionDoesNotDispatch(t *testing.T) {
	called := false
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{-1007: true}, ForumChatID: -1007, Dispatch: func(context.Context, Incoming) { called = true }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 11, From: &User{ID: 4}, Chat: Chat{ID: -1007, Type: "supergroup"}, MessageThreadID: 78, Text: "let's discuss this first"}})
	if called {
		t.Fatal("dispatched unmentioned topic discussion")
	}
}

func TestMentionedTopicFollowUpDispatches(t *testing.T) {
	called := false
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{-1007: true}, ForumChatID: -1007, Dispatch: func(_ context.Context, msg Incoming) { called = msg.Text == "continue" }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 12, From: &User{ID: 4}, Chat: Chat{ID: -1007, Type: "supergroup"}, MessageThreadID: 78, Text: "@jeff continue", Entities: []Entity{{Type: "mention", Offset: 0, Length: 5}}}})
	if !called {
		t.Fatal("did not dispatch mentioned topic follow-up")
	}
}

func TestRouterReactsToPrompt(t *testing.T) {
	reactions := &fakeReactions{}
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{7: true}, Reactions: reactions, Dispatch: func(context.Context, Incoming) {}}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 12, From: &User{ID: 4}, Chat: Chat{ID: 7, Type: "group"}, Text: "@jeff continue", Entities: []Entity{{Type: "mention", Offset: 0, Length: 5}}}})
	if len(reactions.set) != 1 || reactions.set[0].ChatID != 7 || reactions.set[0].MessageID != 12 || len(reactions.set[0].Reaction) != 1 || reactions.set[0].Reaction[0] != (ReactionType{Type: "emoji", Emoji: "👀"}) {
		t.Fatalf("reactions=%+v", reactions.set)
	}
}

func TestRouterDoesNotReactToCommand(t *testing.T) {
	reactions := &fakeReactions{}
	r := &Router{Allowed: map[int64]bool{7: true}, Reactions: reactions, Dispatch: func(context.Context, Incoming) {}}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 12, From: &User{ID: 4}, Chat: Chat{ID: 7, Type: "private"}, Text: "/status"}})
	if len(reactions.set) != 0 {
		t.Fatalf("reactions=%+v", reactions.set)
	}
}

func TestMentionedSlashProjectIsParsedAsCommand(t *testing.T) {
	var got Incoming
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{7: true}, Dispatch: func(_ context.Context, msg Incoming) { got = msg }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 13, From: &User{ID: 4}, Chat: Chat{ID: 7, Type: "group"}, Text: "@jeff /project", Entities: []Entity{{Type: "mention", Offset: 0, Length: 5}}}})
	if got.Command != "project" || got.Text != "" || got.RequestedProject != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestHealthCommandDispatchesWithoutMentionInGroup(t *testing.T) {
	var got Incoming
	r := &Router{BotUsername: "jeff", Allowed: map[int64]bool{7: true}, Dispatch: func(_ context.Context, msg Incoming) { got = msg }}
	r.Handle(context.Background(), Update{Message: &Message{MessageID: 14, From: &User{ID: 4}, Chat: Chat{ID: 7, Type: "group"}, Text: "/health"}})
	if got.Command != "health" || got.Text != "/health" {
		t.Fatalf("got %+v", got)
	}
}

func TestTopicLink(t *testing.T) {
	if got := topicLink(Chat{ID: -100123, Username: "main"}, 55, 77); got != "https://t.me/main/55?thread=77" {
		t.Fatal(got)
	}
	if got := topicLink(Chat{ID: -100123}, 55, 77); got != "https://t.me/c/123/55?thread=77" {
		t.Fatal(got)
	}
}
func TestTopicNameIsBounded(t *testing.T) {
	if got := topicName(string(make([]rune, 200))); len([]rune(got)) > 48 {
		t.Fatal(len([]rune(got)))
	}
}

func TestTopicNameUsesConciseRequestSubject(t *testing.T) {
	got := topicName("any recommendations for topic naming? current one is too long")
	if got != "Topic naming" {
		t.Fatalf("got %q", got)
	}
}

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
