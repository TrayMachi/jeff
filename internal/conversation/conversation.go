package conversation

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Key struct{ ChatID, TopicID, RootMessageID int64 }

func (k Key) String() string {
	return fmt.Sprintf("telegram:%d:%d:%d", k.ChatID, k.TopicID, k.RootMessageID)
}

func ParseKey(value string) (Key, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 4 || parts[0] != "telegram" {
		return Key{}, fmt.Errorf("invalid conversation key %q", value)
	}
	values := make([]int64, 3)
	for i := range values {
		parsed, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return Key{}, fmt.Errorf("invalid conversation key %q: %w", value, err)
		}
		values[i] = parsed
	}
	return Key{ChatID: values[0], TopicID: values[1], RootMessageID: values[2]}, nil
}

type Message struct {
	ID               int64
	ChatID           int64
	TopicID          int64
	RootID           int64
	Text             string
	UserID           int64
	UserName         string
	ChatType         string
	MentionsBot      bool
	BotUsername      string
	RequestedProject string
}

var selector = regexp.MustCompile(`^#([A-Za-z][A-Za-z0-9_-]*)\s*`)

func ParsePrompt(text string) (project, prompt string) {
	text = strings.TrimSpace(text)
	if m := selector.FindStringSubmatch(text); m != nil {
		return m[1], strings.TrimSpace(text[len(m[0]):])
	}
	return "", text
}

func RootFor(chatID, topicID, messageID, replyRoot int64) Key {
	root := messageID
	if replyRoot != 0 {
		root = replyRoot
	}
	return Key{ChatID: chatID, TopicID: topicID, RootMessageID: root}
}

type Command struct{ Name, Argument, Prompt string }

func ParseCommand(text, botUsername string) (Command, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return Command{}, false
	}
	name := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		if !strings.EqualFold(name[at+1:], botUsername) {
			return Command{}, false
		}
		name = name[:at]
	}
	name = strings.ToLower(name)
	cmd := Command{Name: name}
	if len(fields) > 1 {
		cmd.Argument = fields[1]
	}
	if len(fields) > 2 {
		cmd.Prompt = strings.TrimSpace(strings.Join(fields[2:], " "))
	}
	return cmd, true
}
