package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/local/jeff/internal/app"
	"github.com/local/jeff/internal/config"
	"github.com/local/jeff/internal/contexts"
	"github.com/local/jeff/internal/events"
	"github.com/local/jeff/internal/opencode"
	"github.com/local/jeff/internal/projects"
	"github.com/local/jeff/internal/question"
	"github.com/local/jeff/internal/session"
	"github.com/local/jeff/internal/store"
	"github.com/local/jeff/internal/streamer"
	"github.com/local/jeff/internal/telegram"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fileCfg, err := contexts.Load(cfg.ConfigPath())
	if err != nil {
		return err
	}
	catalog, qa, err := projects.Load(cfg.ConfigPath())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath()), 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer db.Close()
	oc := opencode.NewClient(cfg.OpencodeBaseURL, cfg.OpencodeUsername, cfg.OpencodePassword, nil)
	pin, err := cfg.OpencodePinnedVersion()
	if err != nil {
		return err
	}
	if err := oc.CheckReady(ctx, pin); err != nil {
		return err
	}
	bot := telegram.NewClient(cfg.TelegramBotToken, &http.Client{Timeout: 70 * time.Second})
	me, err := bot.GetMe(ctx)
	if err != nil {
		return err
	}
	questions := question.NewRegistry(bot, oc)
	for _, cc := range catalog.Config.Contexts {
		pending, err := oc.ListQuestions(ctx, cc.Directory)
		if err != nil {
			return err
		}
		for _, q := range pending {
			_ = oc.RejectQuestion(ctx, q.ID, cc.Directory)
		}
	}
	eventStream := opencode.NewEventStream(ctx, oc)
	resolver := session.NewResolver(db, oc, catalog.Config)
	responder := app.BuildResponder(app.Deps{Telegram: bot, OpenCode: eventStream, Resolver: resolver, Projects: catalog.Config, QA: qa, Stream: streamer.StreamReply, Questions: questions})
	dispatcher := events.NewDispatcher(events.DispatcherParams{Telegram: bot, Responder: responder, Canceller: app.BuildCanceller(db, oc, catalog.Config, questions.ExpireConversation), Status: app.BuildStatusProvider(db, oc, catalog.Config)})
	router := &telegram.Router{BotUsername: me.Username, Allowed: cfg.TelegramAllowedChats, Dispatch: func(ctx context.Context, incoming telegram.Incoming) {
		dispatcher.Dispatch(ctx, events.IncomingMessage{Conversation: incoming.Conversation, MessageID: incoming.MessageID, ChatID: incoming.ChatID, TopicID: incoming.TopicID, Text: incoming.Text, UserID: incoming.UserID, UserName: incoming.UserName, ChatType: incoming.ChatType, MentionsBot: incoming.MentionsBot, RequestedProject: incoming.RequestedProject, Command: incoming.Command})
	}, TextHandler: func(ctx context.Context, incoming telegram.Incoming) bool {
		return questions.HandleText(ctx, incoming.Conversation.String(), incoming.UserID, incoming.Text)
	}, Callback: func(ctx context.Context, q telegram.CallbackQuery) {
		if err := questions.HandleCallback(ctx, q); err != nil {
			slog.Warn("question callback failed", "error", err)
		}
	}}
	slog.Info("starting Jeff Telegram bot", "username", me.Username, "projects", len(fileCfg.Contexts.Contexts))
	return telegram.NewPoller(bot, router.Handle).Run(ctx)
}
