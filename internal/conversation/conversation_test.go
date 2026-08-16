package conversation

import "testing"

func TestParsePrompt(t *testing.T) {
	project, prompt := ParsePrompt("#api  fix login")
	if project != "api" || prompt != "fix login" {
		t.Fatalf("got %q %q", project, prompt)
	}
}
func TestParseCommand(t *testing.T) {
	cmd, ok := ParseCommand("/project@jeff api fix login", "jeff")
	if !ok || cmd.Name != "project" || cmd.Argument != "api" || cmd.Prompt != "fix login" {
		t.Fatalf("got %+v %v", cmd, ok)
	}
	if _, ok := ParseCommand("/status@other", "jeff"); ok {
		t.Fatal("accepted another bot command")
	}
}
func TestParseKey(t *testing.T) {
	want := Key{ChatID: -1, TopicID: 2, RootMessageID: 3}
	got, err := ParseKey(want.String())
	if err != nil || got != want {
		t.Fatalf("got %+v %v", got, err)
	}
}
func TestRootForReply(t *testing.T) {
	if got := RootFor(7, 8, 9, 3); got.RootMessageID != 3 {
		t.Fatalf("got %+v", got)
	}
}
