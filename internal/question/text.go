package question

import (
	"context"
	"strings"

	"github.com/local/jeff/internal/telegram"
)

func (r *Registry) HandleText(ctx context.Context, conversation string, userID int64, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	r.mu.Lock()
	var requestID string
	var p *pending
	for id, candidate := range r.pending {
		if candidate.conversation == conversation && candidate.userID == userID && candidate.customQuestion >= 0 {
			requestID, p = id, candidate
			break
		}
	}
	if p == nil {
		r.mu.Unlock()
		return false
	}
	index := p.customQuestion
	p.answers[index] = []string{text}
	complete := true
	for _, answer := range p.answers {
		if answer == nil {
			complete = false
			break
		}
	}
	directory := p.directory
	answers := p.answers
	r.mu.Unlock()
	if complete {
		if err := r.oc.ReplyQuestion(ctx, requestID, answers, directory); err != nil {
			_ = r.oc.RejectQuestion(ctx, requestID, directory)
		}
		r.mu.Lock()
		delete(r.pending, requestID)
		r.mu.Unlock()
	}
	if p.messageID != 0 {
		_, _ = r.transport.EditMessage(ctx, telegram.EditMessageParams{
			ChatID:      p.chatID,
			MessageID:   p.messageID,
			Text:        renderText(&p.req),
			ParseMode:   "HTML",
			ReplyMarkup: r.keyboard(&p.req, p),
		})
	}
	return true
}
