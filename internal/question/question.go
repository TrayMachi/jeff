package question

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/local/jeff/internal/opencode"
	"github.com/local/jeff/internal/telegram"
)

type Transport interface {
	SendMessage(context.Context, telegram.SendMessageParams) (telegram.Message, error)
	EditMessage(context.Context, telegram.EditMessageParams) (telegram.Message, error)
	AnswerCallback(context.Context, telegram.CallbackAnswerParams) error
}
type OpenCodeClient interface {
	ReplyQuestion(context.Context, string, [][]string, string) error
	RejectQuestion(context.Context, string, string) error
}
type AskParams struct {
	Request                           opencode.QuestionRequest
	Conversation                      string
	ChatID, TopicID, ReplyToMessageID int64
	Directory                         string
	UserID                            int64
	Timeout                           time.Duration
}
type pending struct {
	req                                opencode.QuestionRequest
	conversation, directory            string
	chatID, topicID, messageID, userID int64
	answers                            [][]string
	selected                           map[int]map[int]bool
	customQuestion                     int
	expires                            time.Time
}
type callback struct {
	requestID        string
	question, option int
	submit, custom   bool
	expires          time.Time
}
type Registry struct {
	transport Transport
	oc        OpenCodeClient
	mu        sync.Mutex
	pending   map[string]*pending
	callbacks map[string]callback
}

func NewRegistry(t Transport, oc OpenCodeClient) *Registry {
	return &Registry{transport: t, oc: oc, pending: map[string]*pending{}, callbacks: map[string]callback{}}
}
func (r *Registry) Ask(ctx context.Context, p AskParams) error {
	if len(p.Request.Questions) == 0 {
		_ = r.oc.RejectQuestion(ctx, p.Request.ID, p.Directory)
		return fmt.Errorf("question request has no questions")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	pr := &pending{req: p.Request, conversation: p.Conversation, directory: p.Directory, chatID: p.ChatID, topicID: p.TopicID, userID: p.UserID, answers: make([][]string, len(p.Request.Questions)), selected: map[int]map[int]bool{}, customQuestion: -1, expires: time.Now().Add(timeout)}
	r.mu.Lock()
	r.pending[p.Request.ID] = pr
	r.mu.Unlock()
	msg, err := r.transport.SendMessage(ctx, telegram.SendMessageParams{ChatID: p.ChatID, MessageThreadID: p.TopicID, ReplyToMessageID: p.ReplyToMessageID, Text: renderText(&p.Request), ParseMode: "HTML", ReplyMarkup: r.keyboard(&p.Request, pr)})
	if err != nil {
		r.mu.Lock()
		delete(r.pending, p.Request.ID)
		r.mu.Unlock()
		_ = r.oc.RejectQuestion(ctx, p.Request.ID, p.Directory)
		return err
	}
	r.mu.Lock()
	if live := r.pending[p.Request.ID]; live != nil {
		live.messageID = msg.MessageID
	}
	r.mu.Unlock()
	return nil
}
func renderText(req *opencode.QuestionRequest) string {
	var b strings.Builder
	b.WriteString("<b>Question</b>\n\n")
	for i, q := range req.Questions {
		if i > 0 {
			b.WriteString("\n")
		}
		label := q.Question
		if label == "" {
			label = q.Header
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, telegramEscape(label))
		for _, o := range q.Options {
			fmt.Fprintf(&b, "• %s — %s\n", telegramEscape(o.Label), telegramEscape(o.Description))
		}
	}
	return strings.TrimSpace(b.String())
}
func telegramEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
func (r *Registry) token(cb callback) string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	token := "q:" + base64.RawURLEncoding.EncodeToString(raw[:])
	r.callbacks[token] = cb
	return token
}
func (r *Registry) keyboard(req *opencode.QuestionRequest, p *pending) *telegram.InlineKeyboardMarkup {
	kb := &telegram.InlineKeyboardMarkup{}
	for i, q := range req.Questions {
		if p.answers[i] != nil {
			continue
		}
		for j, o := range q.Options {
			text := o.Label
			if p.selected[i] != nil && p.selected[i][j] {
				text = "✅ " + text
			}
			r.mu.Lock()
			token := r.token(callback{requestID: req.ID, question: i, option: j, expires: time.Now().Add(30 * time.Minute)})
			r.mu.Unlock()
			kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineKeyboardButton{{Text: text, CallbackData: token}})
		}
		if q.Multiple {
			r.mu.Lock()
			token := r.token(callback{requestID: req.ID, question: i, submit: true, expires: time.Now().Add(30 * time.Minute)})
			r.mu.Unlock()
			kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineKeyboardButton{{Text: "Submit", CallbackData: token}})
		}
		if q.AllowsCustom() {
			r.mu.Lock()
			token := r.token(callback{requestID: req.ID, question: i, custom: true, expires: time.Now().Add(30 * time.Minute)})
			r.mu.Unlock()
			kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineKeyboardButton{{Text: "Other / type a reply", CallbackData: token}})
		}
	}
	return kb
}
func (r *Registry) HandleCallback(ctx context.Context, query telegram.CallbackQuery) error {
	if err := r.transport.AnswerCallback(ctx, telegram.CallbackAnswerParams{CallbackQueryID: query.ID}); err != nil {
		return err
	}
	r.mu.Lock()
	cb, ok := r.callbacks[query.Data]
	pr := r.pending[cb.requestID]
	if ok {
		delete(r.callbacks, query.Data)
	}
	r.mu.Unlock()
	if !ok || pr == nil || time.Now().After(cb.expires) {
		return nil
	}
	if query.From.ID != pr.userID {
		return r.transport.AnswerCallback(ctx, telegram.CallbackAnswerParams{CallbackQueryID: query.ID, Text: "Only the requester can answer this.", ShowAlert: true})
	}
	q := &pr.req.Questions[cb.question]
	if cb.custom {
		r.mu.Lock()
		pr.customQuestion = cb.question
		r.mu.Unlock()
		return r.transport.AnswerCallback(ctx, telegram.CallbackAnswerParams{CallbackQueryID: query.ID, Text: "Send your answer as the next message."})
	}
	r.mu.Lock()
	if q.Multiple {
		if pr.selected[cb.question] == nil {
			pr.selected[cb.question] = map[int]bool{}
		}
		if cb.submit {
			answers := []string{}
			for i := range q.Options {
				if pr.selected[cb.question][i] {
					answers = append(answers, q.Options[i].Label)
				}
			}
			if len(answers) == 0 {
				r.mu.Unlock()
				return r.transport.AnswerCallback(ctx, telegram.CallbackAnswerParams{CallbackQueryID: query.ID, Text: "Select at least one option.", ShowAlert: true})
			}
			pr.answers[cb.question] = answers
		} else {
			pr.selected[cb.question][cb.option] = !pr.selected[cb.question][cb.option]
		}
	} else {
		if cb.option < 0 || cb.option >= len(q.Options) {
			r.mu.Unlock()
			return nil
		}
		pr.answers[cb.question] = []string{q.Options[cb.option].Label}
	}
	complete := true
	for _, a := range pr.answers {
		if a == nil {
			complete = false
		}
	}
	r.mu.Unlock()
	if complete {
		if err := r.oc.ReplyQuestion(ctx, cb.requestID, pr.answers, pr.directory); err != nil {
			_ = r.oc.RejectQuestion(ctx, cb.requestID, pr.directory)
		}
		r.mu.Lock()
		delete(r.pending, cb.requestID)
		r.mu.Unlock()
	}
	_, err := r.transport.EditMessage(ctx, telegram.EditMessageParams{ChatID: pr.chatID, MessageID: pr.messageID, Text: renderText(&pr.req), ParseMode: "HTML", ReplyMarkup: r.keyboard(&pr.req, pr)})
	return err
}
func (r *Registry) ExpireConversation(ctx context.Context, conversation string) {
	r.mu.Lock()
	var expired []*pending
	for id, p := range r.pending {
		if p.conversation == conversation {
			expired = append(expired, p)
			delete(r.pending, id)
		}
	}
	r.mu.Unlock()
	for _, p := range expired {
		_ = r.oc.RejectQuestion(ctx, p.req.ID, p.directory)
		if p.messageID != 0 {
			_, _ = r.transport.EditMessage(ctx, telegram.EditMessageParams{ChatID: p.chatID, MessageID: p.messageID, Text: telegramEscape("⌛ Expired. The turn moved on."), ParseMode: "HTML"})
		}
	}
}
