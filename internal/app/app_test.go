package app

import (
	"context"
	"testing"

	"github.com/local/jeff/internal/contexts"
	"github.com/local/jeff/internal/events"
	"github.com/local/jeff/internal/telegram"
)

type responderTelegram struct {
	sent []telegram.SendMessageParams
}

func (f *responderTelegram) SendMessage(_ context.Context, p telegram.SendMessageParams) (telegram.Message, error) {
	f.sent = append(f.sent, p)
	return telegram.Message{}, nil
}

func (f *responderTelegram) EditMessage(_ context.Context, _ telegram.EditMessageParams) (telegram.Message, error) {
	return telegram.Message{}, nil
}

func TestProjectCommandListsProjectsWithoutStartingRun(t *testing.T) {
	tg := &responderTelegram{}
	responder := BuildResponder(Deps{
		Telegram: tg,
		Projects: &contexts.ContextsConfig{Contexts: map[string]contexts.ContextConfig{
			"demo": {},
			"api":  {},
		}},
	})

	err := responder(context.Background(), events.IncomingMessage{Command: "project", ChatID: 7, MessageID: 13})
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(tg.sent))
	}
	want := "Available projects:\n- #api\n  Description: \n  Directory: \n- #demo\n  Description: \n  Directory: "
	if got := tg.sent[0].Text; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestProjectCommandIncludesDescriptionAndDirectory(t *testing.T) {
	tg := &responderTelegram{}
	responder := BuildResponder(Deps{
		Telegram: tg,
		Projects: &contexts.ContextsConfig{Contexts: map[string]contexts.ContextConfig{
			"demo": {Description: "Demo project", Directory: "/home/tray/projects/demo"},
		}},
	})

	if err := responder(context.Background(), events.IncomingMessage{Command: "project"}); err != nil {
		t.Fatal(err)
	}
	want := "Available projects:\n- #demo\n  Description: Demo project\n  Directory: /home/tray/projects/demo"
	if got := tg.sent[0].Text; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestShipPrompt(t *testing.T) {
	if got := shipPrompt("fix the bug"); got != "ship fix the bug" {
		t.Fatalf("prompt=%q", got)
	}
	if got := shipPrompt("ship fix the bug"); got != "ship fix the bug" {
		t.Fatalf("prompt=%q", got)
	}
}
