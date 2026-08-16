package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/local/jeff/internal/contexts"
	"github.com/local/jeff/internal/conversation"
	"github.com/local/jeff/internal/events"
	"github.com/local/jeff/internal/formatting"
	"github.com/local/jeff/internal/gateway"
	"github.com/local/jeff/internal/opencode"
	"github.com/local/jeff/internal/question"
	"github.com/local/jeff/internal/session"
	"github.com/local/jeff/internal/store"
	"github.com/local/jeff/internal/streamer"
	"github.com/local/jeff/internal/telegram"
	"github.com/local/jeff/internal/turncontext"
	"strconv"
	"strings"
)

type TelegramClient interface {
	SendMessage(context.Context, telegram.SendMessageParams) (telegram.Message, error)
	EditMessage(context.Context, telegram.EditMessageParams) (telegram.Message, error)
}
type Resolver interface {
	Resolve(context.Context, conversation.Key, string) (*session.Resolved, error)
}
type StreamFunc func(context.Context, streamer.Params) error
type QuestionRegistry interface {
	Ask(context.Context, question.AskParams) error
	ExpireConversation(context.Context, string)
}
type Deps struct {
	Telegram  TelegramClient
	OpenCode  streamer.OpenCodeClient
	Resolver  Resolver
	Projects  *contexts.ContextsConfig
	QA        *contexts.QaSettings
	Stream    StreamFunc
	Questions QuestionRegistry
}

func BuildResponder(d Deps) events.Responder {
	prompts := gateway.New(d.OpenCode)
	return func(ctx context.Context, msg events.IncomingMessage) error {
		prompt := strings.TrimSpace(msg.Text)
		if msg.Command == "project" && msg.RequestedProject == "" && prompt == "" {
			return reply(ctx, d.Telegram, msg, "Available projects: "+available(d.Projects))
		}
		if prompt == "" && msg.RequestedProject == "" {
			return reply(ctx, d.Telegram, msg, "Send a prompt or use #project.")
		}
		if msg.RequestedProject != "" && !d.Projects.Has(msg.RequestedProject) {
			return reply(ctx, d.Telegram, msg, fmt.Sprintf("Unknown project #%s. Available: %s", msg.RequestedProject, available(d.Projects)))
		}
		resolved, err := d.Resolver.Resolve(ctx, msg.Conversation, msg.RequestedProject)
		if err != nil {
			return err
		}
		if resolved.SwitchBlocked != "" {
			return reply(ctx, d.Telegram, msg, fmt.Sprintf("This conversation is already bound to #%s; start a new conversation to use #%s.", resolved.Project, resolved.SwitchBlocked))
		}
		if resolved.IsNew || resolved.SwitchBlocked != "" {
			if err := reply(ctx, d.Telegram, msg, fmt.Sprintf("Running in #%s (%s).", resolved.Project, resolved.Directory)); err != nil {
				return err
			}
		}
		if prompt == "" {
			return nil
		}
		preamble := turncontext.Build(turncontext.Params{ChatID: strconv.FormatInt(msg.ChatID, 10), TopicID: strconv.FormatInt(msg.TopicID, 10), RootMessageID: strconv.FormatInt(msg.Conversation.RootMessageID, 10), MessageID: strconv.FormatInt(msg.MessageID, 10), UserID: strconv.FormatInt(msg.UserID, 10), Name: msg.UserName, Project: resolved.Project})
		send := func(ctx context.Context, text string) error { return sendChunks(ctx, d.Telegram, msg, text) }
		var ask streamer.AskQuestion
		var expire streamer.ExpireQuestions
		if d.Questions != nil {
			ask = func(ctx context.Context, req *opencode.QuestionRequest) error {
				return d.Questions.Ask(ctx, question.AskParams{Request: *req, Conversation: msg.Conversation.String(), ChatID: msg.ChatID, TopicID: msg.TopicID, ReplyToMessageID: msg.MessageID, Directory: resolved.Directory, UserID: msg.UserID, Timeout: d.QA.QuestionTimeout()})
			}
			expire = func(ctx context.Context) { d.Questions.ExpireConversation(ctx, msg.Conversation.String()) }
		}
		params := streamer.Params{OpenCode: d.OpenCode, Send: send, Status: BuildStatus(d.Telegram, msg.ChatID, msg.TopicID), AskQuestion: ask, ExpireQuestions: expire, SessionID: resolved.SessionID, Prompt: prompt, Directory: resolved.Directory, Provider: resolved.Provider, Model: resolved.Model, Effort: resolved.Effort, Preamble: preamble, Timeout: d.QA.TurnTimeout(), QuestionTimeout: d.QA.QuestionTimeout()}
		return prompts.Submit(ctx, msg.Conversation.String(), params.PromptParams(), func(ctx context.Context, ready func()) error {
			params.StreamReady = ready
			return d.Stream(ctx, params)
		})
	}
}
func available(c *contexts.ContextsConfig) string {
	var names []string
	for n := range c.Contexts {
		names = append(names, "#"+n)
	}
	return strings.Join(names, ", ")
}
func reply(ctx context.Context, t TelegramClient, msg events.IncomingMessage, text string) error {
	_, err := t.SendMessage(ctx, telegram.SendMessageParams{ChatID: msg.ChatID, MessageThreadID: msg.TopicID, ReplyToMessageID: msg.MessageID, Text: formatting.Plain(text)})
	return err
}
func sendChunks(ctx context.Context, t TelegramClient, msg events.IncomingMessage, text string) error {
	for _, chunk := range formatting.Chunks(text, formatting.MaxMessageRunes) {
		if _, err := t.SendMessage(ctx, telegram.SendMessageParams{ChatID: msg.ChatID, MessageThreadID: msg.TopicID, ReplyToMessageID: msg.MessageID, Text: formatting.Plain(chunk)}); err != nil {
			return err
		}
	}
	return nil
}

type status struct {
	t           TelegramClient
	chat, topic int64
	messageID   int64
	edits       int
	done        bool
}

func (s *status) Update(ctx context.Context, text string) error {
	if s.done || s.edits >= streamer.LiveStatusEditBudget {
		return nil
	}
	text = formatting.Plain(text)
	if s.messageID == 0 {
		m, err := s.t.SendMessage(ctx, telegram.SendMessageParams{ChatID: s.chat, MessageThreadID: s.topic, Text: text, ParseMode: "HTML"})
		if err == nil {
			s.messageID = m.MessageID
		}
		return err
	}
	_, err := s.t.EditMessage(ctx, telegram.EditMessageParams{ChatID: s.chat, MessageID: s.messageID, Text: text, ParseMode: "HTML"})
	if err == nil {
		s.edits++
	}
	return err
}
func (s *status) Finish(ctx context.Context, text string) error {
	if s.done || s.messageID == 0 {
		return nil
	}
	s.done = true
	_, err := s.t.EditMessage(ctx, telegram.EditMessageParams{ChatID: s.chat, MessageID: s.messageID, Text: formatting.Plain(text), ParseMode: "HTML"})
	return err
}
func BuildStatus(t TelegramClient, chat, topic int64) streamer.RunStatus {
	return &status{t: t, chat: chat, topic: topic}
}

type SessionGetter interface {
	GetSession(context.Context, string, string) (*opencode.Session, error)
	GetLatestAssistantContext(context.Context, string, string) (*opencode.AssistantContext, error)
	ModelContextLimit(context.Context, string, string, string) (float64, error)
}

func BuildStatusProvider(db *store.Store, oc SessionGetter, projects *contexts.ContextsConfig) events.StatusProvider {
	return func(ctx context.Context, keyString string, running bool) (string, error) {
		key, err := conversation.ParseKey(keyString)
		if err != nil {
			return "", err
		}
		row, err := db.GetRow(ctx, key)
		if err != nil {
			return "", err
		}
		if row == nil {
			return "No session for this conversation yet.", nil
		}
		cc := projects.Contexts[row.Project]
		s, err := oc.GetSession(ctx, row.SessionID, cc.Directory)
		if errors.Is(err, opencode.ErrNotFound) {
			return "Session no longer exists.", nil
		}
		if err != nil {
			return "", err
		}
		state := "Idle"
		if running {
			state = "Active"
		}
		return fmt.Sprintf("<b>State</b>: %s\n<b>Project</b>: %s\n<b>Directory</b>: %s\n<b>Session</b>: %s", state, row.Project, cc.Directory, s.ID), nil
	}
}

type Aborter interface {
	AbortSession(context.Context, string, string) error
}

func BuildCanceller(db *store.Store, oc Aborter, projects *contexts.ContextsConfig, expire func(context.Context, string)) events.Canceller {
	return func(ctx context.Context, keyString string) (bool, error) {
		key, err := conversation.ParseKey(keyString)
		if err != nil {
			return false, err
		}
		row, err := db.GetRow(ctx, key)
		if err != nil || row == nil {
			return false, err
		}
		if expire != nil {
			expire(ctx, keyString)
		}
		cc := projects.Contexts[row.Project]
		if err := oc.AbortSession(ctx, row.SessionID, cc.Directory); err != nil {
			return false, err
		}
		return true, nil
	}
}
