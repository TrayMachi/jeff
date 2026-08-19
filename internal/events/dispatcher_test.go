package events

import (
	"context"
	"testing"

	"github.com/local/jeff/internal/telegram"
)

type healthTelegram struct {
	sent []telegram.SendMessageParams
}

func (f *healthTelegram) SendMessage(_ context.Context, p telegram.SendMessageParams) (telegram.Message, error) {
	f.sent = append(f.sent, p)
	return telegram.Message{}, nil
}

func TestHealthReportsProcessAndSystemdState(t *testing.T) {
	tg := &healthTelegram{}
	d := NewDispatcher(DispatcherParams{
		Telegram: tg,
		Health: func(context.Context) (string, error) {
			return "active", nil
		},
	})
	d.Dispatch(context.Background(), IncomingMessage{ChatID: 7, MessageID: 14, Text: "/health", Command: "health"})
	d.Wait()

	if len(tg.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(tg.sent))
	}
	want := "<b>Alive</b>: yes\n<b>systemd (jeff.service)</b>: active"
	if got := tg.sent[0].Text; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
