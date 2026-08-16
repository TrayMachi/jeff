package streamer

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
)

func TestChunkTextShortIsSingle(t *testing.T) {
	if diff := cmp.Diff([]string{"hi"}, ChunkText("hi", MaxReplyBytes)); diff != "" {
		t.Errorf("ChunkText mismatch (-want +got):\n%s", diff)
	}
}

func TestChunkTextEmptyIsEmpty(t *testing.T) {
	if got := ChunkText("", MaxReplyBytes); len(got) != 0 {
		t.Errorf("ChunkText = %q, want empty", got)
	}
}

func TestChunkTextPreservesWhitespace(t *testing.T) {
	text := "  first\r\n\n  second  \n"
	if diff := cmp.Diff([]string{text}, ChunkText(text, MaxReplyBytes)); diff != "" {
		t.Errorf("ChunkText mismatch (-want +got):\n%s", diff)
	}
}

func TestChunkTextPreservesResponseAcrossChunks(t *testing.T) {
	text := "  ```\n" + strings.Repeat("界", 50) + "\n```  \n"
	chunks := ChunkText(text, 25)
	if got := strings.Join(chunks, ""); got != text {
		t.Errorf("joined chunks = %q, want %q", got, text)
	}
	for _, chunk := range chunks {
		if len(chunk) > 25 {
			t.Errorf("chunk length %d exceeds limit 25: %q", len(chunk), chunk)
		}
		if !utf8.ValidString(chunk) {
			t.Errorf("chunk is not valid UTF-8: %q", chunk)
		}
	}
}
