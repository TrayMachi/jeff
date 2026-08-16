package turncontext

import "fmt"

type Params struct{ ChatID, TopicID, RootMessageID, MessageID, UserID, Name, Project string }

func Build(p Params) string {
	return fmt.Sprintf("[Context]\nExecution Mode: telegram-bot\nName: %s\nTelegram User ID: %s\nTelegram Chat ID: %s\nTelegram Topic ID: %s\nTelegram Root Message ID: %s\nTelegram Message ID: %s\nProject: %s\n", p.Name, p.UserID, p.ChatID, p.TopicID, p.RootMessageID, p.MessageID, p.Project)
}
