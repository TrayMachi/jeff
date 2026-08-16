package formatting

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunksPreserveUnicode(t *testing.T) {
	input := strings.Repeat("你好 ", 100)
	chunks := Chunks(input, 17)
	var got strings.Builder
	for _, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > 17 {
			t.Fatalf("chunk too long: %d", utf8.RuneCountInString(chunk))
		}
		got.WriteString(chunk)
	}
	if strings.ReplaceAll(got.String(), " ", "") != strings.ReplaceAll(input, " ", "") {
		t.Fatal("chunks changed content")
	}
}
func TestPlainLeavesTextUntouched(t *testing.T) {
	if got := Plain("<tag> & text"); got != "<tag> & text" {
		t.Fatal(got)
	}
}
