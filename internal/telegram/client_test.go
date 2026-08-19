package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateForumTopic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/createForumTopic" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body CreateForumTopicParams
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ChatID != -1007 || body.Name != "request" {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(APIResponse[ForumTopic]{OK: true, Result: ForumTopic{MessageThreadID: 77, Name: body.Name}})
	}))
	defer server.Close()
	client := NewClient("TOKEN", server.Client())
	client.SetBaseURL(server.URL)
	got, err := client.CreateForumTopic(context.Background(), CreateForumTopicParams{ChatID: -1007, Name: "request"})
	if err != nil || got.MessageThreadID != 77 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestSetMessageReaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/setMessageReaction" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body SetMessageReactionParams
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ChatID != -1007 || body.MessageID != 9 || len(body.Reaction) != 1 || body.Reaction[0] != (ReactionType{Type: "emoji", Emoji: "👀"}) {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(APIResponse[bool]{OK: true, Result: true})
	}))
	defer server.Close()
	client := NewClient("TOKEN", server.Client())
	client.SetBaseURL(server.URL)
	if err := client.SetMessageReaction(context.Background(), SetMessageReactionParams{ChatID: -1007, MessageID: 9, Reaction: []ReactionType{{Type: "emoji", Emoji: "👀"}}}); err != nil {
		t.Fatal(err)
	}
}
