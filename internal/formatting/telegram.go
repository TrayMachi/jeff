package formatting

import (
	"strings"
	"unicode/utf8"
)

const MaxMessageRunes = 3800

func Plain(text string) string { return text }
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
