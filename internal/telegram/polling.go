package telegram

import (
	"context"
	"log/slog"
	"time"
)

type UpdateHandler func(context.Context, Update)
type Poller struct {
	client  *Client
	handler UpdateHandler
	offset  int64
}

func NewPoller(client *Client, handler UpdateHandler) *Poller {
	return &Poller{client: client, handler: handler}
}
func (p *Poller) Run(ctx context.Context) error {
	for {
		updates, err := p.client.GetUpdates(ctx, p.offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("telegram polling failed", "error", err)
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, update := range updates {
			if update.UpdateID >= p.offset {
				p.offset = update.UpdateID + 1
			}
			if p.handler != nil {
				go p.handler(ctx, update)
			}
		}
	}
}
