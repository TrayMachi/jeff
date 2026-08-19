package formatting

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxMessageRunes = 3800

func Plain(text string) string { return text }

var (
	markdownCodeBlock = regexp.MustCompile("(?s)```[^\\n]*\\n?(.*?)```")
	markdownCode      = regexp.MustCompile("`([^`\\n]+)`")
	markdownLink      = regexp.MustCompile(`\[([^]]+)\]\(([^)\s]+)\)`)
	markdownBold      = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	markdownStrike    = regexp.MustCompile(`~~([^~\n]+)~~`)
)

// MarkdownToHTML converts the Markdown emitted by OpenCode to Telegram's
// HTML subset. Code is protected before inline formatting so its contents are
// displayed literally and cannot be interpreted as Telegram markup.
func MarkdownToHTML(text string) string {
	var protected []string
	protect := func(value string) string {
		index := len(protected)
		protected = append(protected, value)
		return "\x00" + string(rune(index)) + "\x00"
	}

	text = markdownCodeBlock.ReplaceAllStringFunc(text, func(value string) string {
		content := strings.TrimSuffix(strings.TrimPrefix(value, "```"), "```")
		if newline := strings.IndexByte(content, '\n'); newline >= 0 {
			content = content[newline+1:]
		}
		return protect("<pre><code>" + html.EscapeString(content) + "</code></pre>")
	})
	text = markdownCode.ReplaceAllStringFunc(text, func(value string) string {
		content := strings.Trim(value, "`")
		return protect("<code>" + html.EscapeString(content) + "</code>")
	})
	text = html.EscapeString(text)
	text = markdownLink.ReplaceAllString(text, `<a href="$2">$1</a>`)
	text = markdownBold.ReplaceAllString(text, `<b>$1</b>`)
	text = markdownStrike.ReplaceAllString(text, `<s>$1</s>`)

	for index, value := range protected {
		text = strings.ReplaceAll(text, "\x00"+string(rune(index))+"\x00", value)
	}
	return text
}

func Chunks(text string, limit int) []string {
	if limit <= 0 {
		limit = MaxMessageRunes
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	var out []string
	for len(text) > 0 {
		end := byteOffsetForRunes(text, limit)
		part := text[:end]
		if split := strings.LastIndexAny(part, " \n\t"); split > len(part)/2 {
			end = split
			part = text[:end]
		}
		out = append(out, part)
		text = text[end:]
	}
	return out
}

func byteOffsetForRunes(text string, count int) int {
	if count <= 0 {
		return 0
	}
	seen := 0
	for index := range text {
		if seen == count {
			return index
		}
		seen++
	}
	return len(text)
}
