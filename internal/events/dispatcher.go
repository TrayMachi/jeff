package events

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/local/jeff/internal/telegram"
)

type TelegramClient interface {
	SendMessage(context.Context, telegram.SendMessageParams) (telegram.Message, error)
}
type Canceller func(context.Context, string) (bool, error)
type StatusProvider func(context.Context, string, bool) (string, error)
type Dispatcher struct {
	telegram  TelegramClient
	responder Responder
	cancel    Canceller
	status    StatusProvider
	mu        sync.Mutex
	running   map[string]int
	wg        sync.WaitGroup
}
type DispatcherParams struct {
	Telegram  TelegramClient
	Responder Responder
	Canceller Canceller
	Status    StatusProvider
}

func NewDispatcher(p DispatcherParams) *Dispatcher {
	return &Dispatcher{telegram: p.Telegram, responder: p.Responder, cancel: p.Canceller, status: p.Status, running: map[string]int{}}
}
func (d *Dispatcher) Dispatch(ctx context.Context, msg IncomingMessage) {
	if msg.ChatType != "private" && !msg.MentionsBot && msg.Command == "" && !msg.InForumTopic {
		return
	}
	switch strings.ToLower(strings.TrimSpace(msg.Text)) {
	case "/cancel", "/stop":
		d.wg.Go(func() { d.handleCancel(ctx, msg) })
		return
	case "/status":
		d.wg.Go(func() { d.handleStatus(ctx, msg) })
		return
	}
	d.mu.Lock()
	d.running[msg.Conversation.String()]++
	d.mu.Unlock()
	d.wg.Go(func() {
		defer d.done(msg)
		if err := d.responder(ctx, msg); err != nil {
			slog.Error("responder failed", "error", err)
			d.reply(ctx, msg, "Something broke. Try again.")
		}
	})
}
func (d *Dispatcher) done(msg IncomingMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := msg.Conversation.String()
	d.running[key]--
	if d.running[key] == 0 {
		delete(d.running, key)
	}
}
func (d *Dispatcher) Wait() { d.wg.Wait() }
func (d *Dispatcher) isRunning(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[key] > 0
}
func (d *Dispatcher) handleCancel(ctx context.Context, msg IncomingMessage) {
	if !d.isRunning(msg.Conversation.String()) {
		d.reply(ctx, msg, "Nothing running. Nothing to cancel.")
		return
	}
	ok := false
	if d.cancel != nil {
		ok, _ = d.cancel(ctx, msg.Conversation.String())
	}
	if ok {
		d.reply(ctx, msg, "Stopping.")
	} else {
		d.reply(ctx, msg, "Could not cancel the current turn.")
	}
}
func (d *Dispatcher) handleStatus(ctx context.Context, msg IncomingMessage) {
	text := "Status unavailable."
	if d.status != nil {
		if value, err := d.status(ctx, msg.Conversation.String(), d.isRunning(msg.Conversation.String())); err == nil {
			text = value
		}
	}
	d.reply(ctx, msg, text)
}
func (d *Dispatcher) reply(ctx context.Context, msg IncomingMessage, text string) {
	_, err := d.telegram.SendMessage(ctx, telegram.SendMessageParams{ChatID: msg.ChatID, MessageThreadID: msg.TopicID, ReplyToMessageID: msg.MessageID, Text: text})
	if err != nil {
		slog.Warn("failed to send Telegram reply", "error", err)
	}
}
