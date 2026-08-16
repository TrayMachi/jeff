// Package streamer streams an OpenCode reply into a Telegram conversation.
package streamer

import "unicode/utf8"

// MaxReplyBytes leaves room under Lark's 30 KB rich-text message-body limit
// for the JSON envelope and escaping performed by the transport.
const MaxReplyBytes = 24 * 1024

// ChunkText splits text into chunks no longer than limit bytes without
// changing the response. Concatenating the chunks always reproduces text.
func ChunkText(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	for start := 0; start < len(text); {
		end := min(start+limit, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == start {
			_, size := utf8.DecodeRuneInString(text[start:])
			end = start + size
		}
		chunks = append(chunks, text[start:end])
		start = end
	}
	return chunks
}
