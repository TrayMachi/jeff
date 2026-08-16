package streamer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/local/jeff/internal/opencode"
)

func msgUpdated(sessionID, messageID string) opencode.Event {
	return opencode.Event{MessageUpdated: &opencode.MessageUpdated{
		Info: opencode.MessageInfo{ID: messageID, SessionID: sessionID, Role: "assistant"},
	}}
}

func textPartEvent(sessionID, messageID, partID, text string) opencode.Event {
	return opencode.Event{PartUpdated: &opencode.PartUpdated{Part: opencode.Part{
		ID: partID, MessageID: messageID, SessionID: sessionID, Type: "text", Text: text,
	}}}
}

// reasoningPart is a part OpenCode emits as its own type: "reasoning".
func reasoningPart(sessionID, messageID, partID, text string) opencode.Event {
	return opencode.Event{PartUpdated: &opencode.PartUpdated{Part: opencode.Part{
		ID: partID, MessageID: messageID, SessionID: sessionID, Type: "reasoning", Text: text,
	}}}
}

// todoPart is an opencode todowrite tool part carrying the agent's todo list.
func todoPart(t *testing.T, sessionID, messageID, partID string, todos []map[string]string) opencode.Event {
	t.Helper()
	state, err := json.Marshal(map[string]any{"input": map[string]any{"todos": todos}})
	if err != nil {
		t.Fatal(err)
	}
	return opencode.Event{PartUpdated: &opencode.PartUpdated{Part: opencode.Part{
		ID: partID, MessageID: messageID, SessionID: sessionID, Type: "tool", Tool: "todowrite",
		State: state,
	}}}
}

func toolPart(sessionID, messageID, partID, callID, tool string) opencode.Event {
	return opencode.Event{PartUpdated: &opencode.PartUpdated{Part: opencode.Part{
		ID: partID, CallID: callID, MessageID: messageID, SessionID: sessionID, Type: "tool", Tool: tool,
	}}}
}

func titledToolPart(t *testing.T, sessionID, messageID, partID, callID, tool, status, title string) opencode.Event {
	t.Helper()
	state, err := json.Marshal(map[string]any{"status": status, "title": title})
	if err != nil {
		t.Fatal(err)
	}
	ev := toolPart(sessionID, messageID, partID, callID, tool)
	ev.PartUpdated.Part.State = state
	return ev
}

func idle(sessionID string) opencode.Event {
	return opencode.Event{SessionIdle: &opencode.SessionIdle{SessionID: sessionID}}
}

// retryStatus is a session.status event opencode publishes before each
// provider retry.
func retryStatus(sessionID string, attempt int, message string) opencode.Event {
	ev := &opencode.SessionStatus{SessionID: sessionID}
	ev.Status.Type = "retry"
	ev.Status.Attempt = attempt
	ev.Status.Message = message
	return opencode.Event{SessionStatus: ev}
}

func sessionError(sessionID, name, message string) opencode.Event {
	payload := &opencode.ErrorPayload{Name: name}
	payload.Data.Message = message
	return opencode.Event{SessionError: &opencode.SessionError{SessionID: sessionID, Error: payload}}
}

// fakeOpenCode models the subscribe-then-prompt ordering: events only flow
// after Prompt.
type fakeOpenCode struct {
	events       []opencode.Event
	promptErr    error
	subscribeErr error
	abortErr     error
	hang         bool  // block after the events (no idle) to exercise the timeout
	streamErr    error // terminal stream error after the events (transport failure)

	goCh     chan struct{}
	mu       sync.Mutex
	prompted *opencode.PromptParams
	aborted  bool
	subCtx   context.Context
}

func newFakeOpenCode(events ...opencode.Event) *fakeOpenCode {
	return &fakeOpenCode{events: events, goCh: make(chan struct{})}
}

func (f *fakeOpenCode) Subscribe(ctx context.Context, _ string) (*opencode.Stream, error) {
	f.mu.Lock()
	f.subCtx = ctx
	f.mu.Unlock()
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	ch := make(chan opencode.Event)
	terminal := new(error)
	go func() {
		defer close(ch)
		select {
		case <-f.goCh:
		case <-ctx.Done():
			return
		}
		for _, ev := range f.events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
		if f.hang {
			<-ctx.Done() // never completes; the turn ends by timeout/abort
			return
		}
		*terminal = f.streamErr
	}()
	return opencode.NewStream(ch, func() error { return *terminal }), nil
}

func (f *fakeOpenCode) PromptAsync(_ context.Context, p opencode.PromptParams) error {
	f.mu.Lock()
	f.prompted = &p
	f.mu.Unlock()
	if f.promptErr != nil {
		return f.promptErr
	}
	close(f.goCh)
	return nil
}

func (f *fakeOpenCode) AbortSession(context.Context, string, string) error {
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
	return f.abortErr
}

func (f *fakeOpenCode) wasAborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

func (f *fakeOpenCode) subscribeCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subCtx
}

func questionAsked(sessionID, requestID string) opencode.Event {
	return opencode.Event{QuestionAsked: &opencode.QuestionRequest{
		ID: requestID, SessionID: sessionID,
		Questions: []opencode.QuestionInfo{{
			Question: "Pick one.", Header: "Pick",
			Options: []opencode.QuestionOption{{Label: "A", Description: "first"}},
		}},
	}}
}

func questionReplied(sessionID, requestID string) opencode.Event {
	return opencode.Event{QuestionReplied: &opencode.QuestionReplied{
		SessionID: sessionID, RequestID: requestID,
	}}
}

func questionRejected(sessionID, requestID string) opencode.Event {
	return opencode.Event{QuestionRejected: &opencode.QuestionRejected{
		SessionID: sessionID, RequestID: requestID,
	}}
}

// gatedOpenCode hand-feeds events mid-turn — the question tests control the
// timeline (ask, wait past a clock, then resume), which the scripted
// fakeOpenCode can't express.
type gatedOpenCode struct {
	ch       chan opencode.Event
	terminal error

	mu      sync.Mutex
	aborted bool
}

func newGatedOpenCode() *gatedOpenCode {
	return &gatedOpenCode{ch: make(chan opencode.Event)}
}

func (g *gatedOpenCode) Subscribe(context.Context, string) (*opencode.Stream, error) {
	return opencode.NewStream(g.ch, func() error { return g.terminal }), nil
}

func (g *gatedOpenCode) PromptAsync(context.Context, opencode.PromptParams) error { return nil }

func (g *gatedOpenCode) AbortSession(context.Context, string, string) error {
	g.mu.Lock()
	g.aborted = true
	g.mu.Unlock()
	return nil
}

func (g *gatedOpenCode) wasAborted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.aborted
}

func (g *gatedOpenCode) feed(events ...opencode.Event) {
	for _, ev := range events {
		g.ch <- ev
	}
}

type blockingPromptOpenCode struct {
	ch            chan opencode.Event
	terminal      error
	promptStarted chan struct{}
	releasePrompt chan struct{}
}

func newBlockingPromptOpenCode() *blockingPromptOpenCode {
	return &blockingPromptOpenCode{
		ch:            make(chan opencode.Event),
		promptStarted: make(chan struct{}),
		releasePrompt: make(chan struct{}),
	}
}

func (b *blockingPromptOpenCode) Subscribe(context.Context, string) (*opencode.Stream, error) {
	return opencode.NewStream(b.ch, func() error { return b.terminal }), nil
}

func (b *blockingPromptOpenCode) PromptAsync(ctx context.Context, _ opencode.PromptParams) error {
	close(b.promptStarted)
	select {
	case <-b.releasePrompt:
	case <-ctx.Done():
		return ctx.Err()
	}
	close(b.ch)
	return nil
}

func (b *blockingPromptOpenCode) AbortSession(context.Context, string, string) error { return nil }

type sink struct {
	mu       sync.Mutex
	sent     []string
	status   []string
	finished []string
}

type fakeStatus struct {
	update func(context.Context, string) error
	finish func(context.Context, string) error
}

func (f fakeStatus) Update(ctx context.Context, text string) error {
	return f.update(ctx, text)
}

func (f fakeStatus) Finish(ctx context.Context, text string) error {
	if f.finish == nil {
		return nil
	}
	return f.finish(ctx, text)
}

func (s *sink) send(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, text)
	return nil
}

func (s *sink) Update(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = append(s.status, text)
	return nil
}

func (s *sink) Finish(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, text)
	return nil
}

func (s *sink) sentSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.sent...)
}

func (s *sink) statusSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.status...)
}

func (s *sink) finishedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.finished...)
}

// run mirrors the Python suite's run() helper: a full model spec, no
// live status or timeout.
func run(t *testing.T, oc *fakeOpenCode, s *sink) {
	t.Helper()
	err := StreamReply(t.Context(), Params{
		OpenCode:  oc,
		Send:      s.send,
		SessionID: "s1",
		Prompt:    "hello",
		Directory: "/home/ubuntu/otp",
		Provider:  "anthropic",
		Model:     "claude-opus-4-8",
		Effort:    "high",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func wantSent(t *testing.T, s *sink, want ...string) {
	t.Helper()
	if diff := cmp.Diff(want, s.sentSnapshot()); diff != "" {
		t.Errorf("sent mismatch (-want +got):\n%s", diff)
	}
}

func TestPromptParamsIncludesEveryPromptField(t *testing.T) {
	p := Params{
		SessionID: "ses_1",
		Prompt:    "fix it",
		Directory: "/workspace",
		Provider:  "openai",
		Model:     "gpt-5",
		Effort:    "high",
		Preamble:  "[Context]",
	}
	want := opencode.PromptParams{
		SessionID: "ses_1",
		Text:      "fix it",
		Directory: "/workspace",
		Provider:  "openai",
		Model:     "gpt-5",
		Effort:    "high",
		Preamble:  "[Context]",
	}
	if diff := cmp.Diff(want, p.PromptParams()); diff != "" {
		t.Errorf("PromptParams mismatch (-want +got):\n%s", diff)
	}
}

func TestHappyPathAccumulatesAndSendsOnce(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "Hello "),
		textPartEvent("s1", "m1", "p1", "Hello world"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "Hello world")
	// the prompt was sent with the context's model params
	want := opencode.PromptParams{
		SessionID: "s1", Text: "hello", Directory: "/home/ubuntu/otp",
		Provider: "anthropic", Model: "claude-opus-4-8", Effort: "high",
	}
	if diff := cmp.Diff(&want, oc.prompted); diff != "" {
		t.Errorf("prompted mismatch (-want +got):\n%s", diff)
	}
}

func TestStreamReadyWaitsForInitialPrompt(t *testing.T) {
	oc := newBlockingPromptOpenCode()
	s := &sink{}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode:  oc,
			Send:      s.send,
			SessionID: "s1",
			Prompt:    "hello",
			Directory: "/home/ubuntu/otp",
			StreamReady: func() {
				close(ready)
			},
		})
	}()

	<-oc.promptStarted
	select {
	case <-ready:
		t.Fatal("stream marked ready before the initial prompt returned")
	case <-time.After(50 * time.Millisecond):
	}

	close(oc.releasePrompt)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("stream was not marked ready after the initial prompt returned")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish")
	}
}

func TestTurnEndCancelsSubscription(t *testing.T) {
	// StreamReply receives the process-lifetime ctx; it must hand Subscribe a
	// per-turn one and cancel it on return, or the SSE reader goroutine and
	// its connection leak every turn.
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), textPartEvent("s1", "m1", "p1", "hi"), idle("s1"))
	s := &sink{}
	run(t, oc, s)
	if oc.subscribeCtx().Err() == nil {
		t.Error("subscription context still live after the turn ended")
	}
}

func TestEffectiveQuestionTimeoutClamps(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, maxQuestionWait},            // "no explicit window" ≠ forever
		{-time.Minute, maxQuestionWait}, // nonsense config
		{30 * time.Minute, 30 * time.Minute},
		{maxQuestionWait, maxQuestionWait},
		{48 * time.Hour, maxQuestionWait}, // above the cap
	} {
		if got := effectiveQuestionTimeout(c.in); got != c.want {
			t.Errorf("effectiveQuestionTimeout(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPreservesReplyBeforeSending(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "See https://example.com <ok>"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "See https://example.com <ok>")
}

func TestPreambleIsForwardedToPrompt(t *testing.T) {
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), textPartEvent("s1", "m1", "p1", "ok"), idle("s1"))
	s := &sink{}
	err := StreamReply(t.Context(), Params{
		OpenCode: oc, Send: s.send,
		SessionID: "s1", Prompt: "hello", Directory: "/home/ubuntu/otp",
		Preamble: "[Context] who",
	})
	if err != nil {
		t.Fatal(err)
	}
	if oc.prompted.Preamble != "[Context] who" {
		t.Errorf("preamble = %q", oc.prompted.Preamble)
	}
}

func TestIgnoresNonAssistantAndOtherSessionParts(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "real answer"),
		textPartEvent("s1", "mX", "pX", "not an assistant msg"), // unknown messageID
		textPartEvent("other", "m1", "p2", "other session"),     // wrong session
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "real answer")
}

func TestReasoningPartsAreHidden(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		reasoningPart("s1", "m1", "r1", "thinking out loud..."),
		textPartEvent("s1", "m1", "p1", "the answer"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "the answer")
}

func TestReasoningOnlyTurnUsesFallback(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		reasoningPart("s1", "m1", "r1", "thinking out loud..."),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "The agent finished without producing any text.")
}

func TestSessionErrorIsReported(t *testing.T) {
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), sessionError("s1", "ProviderError", "boom"))
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "⚠️ ProviderError: boom")
}

func TestPartialTextThenErrorKeepsBoth(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "partial"),
		sessionError("s1", "ProviderError", "died"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "partial\n\n⚠️ ProviderError: died")
}

func TestPromptFailureIsFoldedIntoReply(t *testing.T) {
	oc := newFakeOpenCode(idle("s1"))
	oc.promptErr = errors.New("connection refused")
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "⚠️ connection refused")
}

func TestSubscribeFailureIsFoldedIntoReplyWithoutPrompting(t *testing.T) {
	oc := newFakeOpenCode()
	oc.subscribeErr = errors.New("event stream: HTTP 502")
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "⚠️ event stream: HTTP 502")
	if oc.prompted != nil {
		t.Error("prompted after a failed subscribe")
	}
}

func TestEmptyReplyUsesFallback(t *testing.T) {
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), idle("s1"))
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "The agent finished without producing any text.")
}

func TestLongReplyIsChunked(t *testing.T) {
	big := strings.Repeat("word ", 6000)
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), textPartEvent("s1", "m1", "p1", big), idle("s1"))
	s := &sink{}
	run(t, oc, s)
	sent := s.sentSnapshot()
	if len(sent) <= 1 {
		t.Fatalf("sent %d chunks, want > 1", len(sent))
	}
	for _, c := range sent {
		if len(c) > MaxReplyBytes {
			t.Errorf("chunk length %d exceeds %d", len(c), MaxReplyBytes)
		}
	}
	if got := strings.Join(sent, ""); got != big {
		t.Error("chunking changed the reply")
	}
}

// --- stream-failure shapes (Go-specific pins) --------------------------------

func TestStreamEOFWithoutIdleReportsInterruptionWithText(t *testing.T) {
	// A clean transport close is not proof that the turn completed. Keep the
	// accumulated text, but report the missing session.idle.
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "accumulated so far"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "accumulated so far\n\n⚠️ The event stream ended before the turn completed.")
}

func TestTransportErrorMidTurnBecomesErrorReply(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "partial"),
	)
	oc.streamErr = errors.New("unexpected EOF")
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "partial\n\n⚠️ unexpected EOF")
}

// --- final-answer-only (A2) ---------------------------------------------------

func TestPostsOnlyFinalAssistantMessage(t *testing.T) {
	// A multi-step turn: interim narration (m1) then the closing answer (m2).
	// Only the last message's text should be posted, not the whole monologue.
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "I'll start by writing the test plan..."),
		msgUpdated("s1", "m2"),
		textPartEvent("s1", "m2", "p2", "Verdict: PASS — no P0/P1 issues."),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "Verdict: PASS — no P0/P1 issues.")
}

func TestFinalMessageFallsBackWhenLastMessageHasNoText(t *testing.T) {
	// m2 is the latest assistant message but emits no text (e.g. ends on a
	// tool call); the answer falls back to m1's text rather than the empty
	// fallback.
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "the real answer"),
		msgUpdated("s1", "m2"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "the real answer")
}

// --- unified long-run status ---------------------------------------------------

func TestShortTurnDoesNotPostStatus(t *testing.T) {
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), textPartEvent("s1", "m1", "p1", "hi"), idle("s1"))
	s := &sink{}
	err := StreamReply(t.Context(), Params{
		OpenCode: oc, Send: s.send, Status: s,
		SessionID: "s1", Prompt: "hi", Directory: "/home/ubuntu/otp",
		StatusDelay: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, s, "hi")
	if got := s.statusSnapshot(); len(got) != 0 {
		t.Errorf("status = %q, want none", got)
	}
	if got := s.finishedSnapshot(); len(got) != 0 {
		t.Errorf("finished = %q, want none", got)
	}
}

func TestLongTurnStatusUsesLatestTodoAndOneReceipt(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send, Status: s,
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 20 * time.Millisecond, StatusInterval: 30 * time.Millisecond,
			Timeout: time.Second,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"),
		todoPart(t, "s1", "m1", "todo", []map[string]string{
			{"content": "Inspect flow", "status": "completed"},
			{"content": "Run tests", "status": "in_progress"},
		}),
		toolPart("s1", "m1", "read", "call_1", "read"),
		toolPart("s1", "m1", "read", "call_1", "read"),
		toolPart("s1", "m1", "bash", "call_2", "bash"))
	time.Sleep(25 * time.Millisecond)
	oc.feed(textPartEvent("s1", "m1", "answer", "done"), idle("s1"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	statuses := s.statusSnapshot()
	if len(statuses) != 1 {
		t.Fatalf("status = %q, want one update", statuses)
	}
	for _, want := range []string{"**Working for ", "✅ Inspect flow", "🔄 Run tests", "3 actions"} {
		if !strings.Contains(statuses[0], want) {
			t.Errorf("status %q missing %q", statuses[0], want)
		}
	}
	finished := s.finishedSnapshot()
	if len(finished) != 1 || !strings.Contains(finished[0], "**Completed in ") || !strings.Contains(finished[0], "3 actions") {
		t.Fatalf("finished = %q, want one completion receipt", finished)
	}
}

func TestBackendActivityDoesNotSuppressStatusRefresh(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send, Status: s,
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 20 * time.Millisecond, StatusInterval: 25 * time.Millisecond,
			Timeout: 120 * time.Millisecond,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"))
	for i := 0; i < 6; i++ {
		oc.feed(reasoningPart("s1", "m1", fmt.Sprintf("r%d", i), "hidden"))
		time.Sleep(10 * time.Millisecond)
	}
	oc.feed(textPartEvent("s1", "m1", "answer", "done"), idle("s1"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := len(s.statusSnapshot()); got < 2 {
		t.Fatalf("status updates = %d, want at least two despite continuous backend activity", got)
	}
}

func TestRunStatusUsesSafeToolTitleWithoutTodos(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send, Status: s,
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 15 * time.Millisecond, Timeout: time.Second,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"),
		titledToolPart(t, "s1", "m1", "tool", "call", "bash", "running", "  Running\n tests  "))
	time.Sleep(20 * time.Millisecond)
	oc.feed(textPartEvent("s1", "m1", "answer", "done"), idle("s1"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	statuses := s.statusSnapshot()
	if len(statuses) != 1 || !strings.Contains(statuses[0], "Current: Running tests") {
		t.Fatalf("status = %q, want sanitized tool title", statuses)
	}
}

func TestEmptyTodosClearChecklistAtNextRefresh(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send, Status: s,
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 15 * time.Millisecond, StatusInterval: 20 * time.Millisecond,
			Timeout: 300 * time.Millisecond,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"),
		todoPart(t, "s1", "m1", "todo", []map[string]string{{"content": "Old task", "status": "in_progress"}}))
	time.Sleep(20 * time.Millisecond)
	oc.feed(todoPart(t, "s1", "m1", "todo", []map[string]string{}),
		titledToolPart(t, "s1", "m1", "tool", "call", "bash", "running", "Running checks"))
	time.Sleep(25 * time.Millisecond)
	oc.feed(textPartEvent("s1", "m1", "answer", "done"), idle("s1"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	statuses := s.statusSnapshot()
	if len(statuses) < 2 {
		t.Fatalf("status = %q, want checklist then refreshed activity", statuses)
	}
	if !strings.Contains(statuses[0], "Old task") {
		t.Errorf("first status = %q, want old checklist", statuses[0])
	}
	last := statuses[len(statuses)-1]
	if strings.Contains(last, "Old task") || !strings.Contains(last, "Current: Running checks") {
		t.Errorf("last status = %q, want cleared checklist and current activity", last)
	}
}

func TestStatusFailureDisablesFurtherLiveUpdates(t *testing.T) {
	var calls int
	status := fakeStatus{update: func(context.Context, string) error {
		calls++
		return errors.New("Lark unavailable")
	}}
	oc := newFakeOpenCode(msgUpdated("s1", "m1"))
	oc.hang = true
	s := &sink{}
	err := StreamReply(t.Context(), Params{
		OpenCode: oc, Send: s.send, Status: status,
		SessionID: "s1", Prompt: "hi", Directory: "/w",
		StatusDelay: 15 * time.Millisecond, StatusInterval: 15 * time.Millisecond,
		Timeout: 60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("status calls = %d, want one rejected attempt", calls)
	}
}

// --- timeout ------------------------------------------------------------------

func TestTimeoutAbortsAndReports(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "partial work"),
	)
	oc.hang = true                           // never emits idle → the timeout must abort
	oc.abortErr = errors.New("abort failed") // abort is best-effort after timeout
	s := &sink{}
	err := StreamReply(t.Context(), Params{
		OpenCode: oc, Send: s.send,
		SessionID: "s1", Prompt: "hi", Directory: "/home/ubuntu/otp",
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !oc.wasAborted() {
		t.Error("session not aborted on timeout")
	}
	sent := s.sentSnapshot()
	if len(sent) != 1 {
		t.Fatalf("sent = %q, want exactly one reply", sent)
	}
	if !strings.HasPrefix(sent[0], "partial work") {
		t.Errorf("sent = %q, want partial text kept", sent[0])
	}
	if !strings.Contains(sent[0], "That's my limit") {
		t.Errorf("sent = %q, want the timeout notice", sent[0])
	}
}

func TestRenderReceiptFormatsDurationAndSingularAction(t *testing.T) {
	if got, want := renderReceipt("Completed", 16*time.Minute+34*time.Second+900*time.Millisecond, 1),
		"**Completed in 16m 34s** · 1 action."; got != want {
		t.Errorf("renderReceipt = %q, want %q", got, want)
	}
}

// --- external cancel (session aborted outside the streamer) -------------------

func TestExternalAbortReportsCancelled(t *testing.T) {
	oc := newFakeOpenCode(msgUpdated("s1", "m1"), sessionError("s1", "MessageAbortedError", "Aborted"))
	s := &sink{}
	run(t, oc, s)
	// A user-requested stop, not a failure: no ⚠️ marker.
	wantSent(t, s, "🛑 Stopped.")
}

func TestExternalAbortKeepsPartialText(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		textPartEvent("s1", "m1", "p1", "partial"),
		sessionError("s1", "MessageAbortedError", "Aborted"),
	)
	s := &sink{}
	run(t, oc, s)
	wantSent(t, s, "partial\n\n🛑 Stopped.")
}

func TestPendingQuestionSuspendsTurnClockAndStatusDelay(t *testing.T) {
	// The turn would time out at 60ms, but the whole wait is the human's
	// (a question is pending), so the turn must survive it — and stay
	// status-silent: the card is the progress signal.
	oc := newGatedOpenCode()
	s := &sink{}
	var asked []string
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send, Status: s,
			AskQuestion: func(_ context.Context, req *opencode.QuestionRequest) error {
				mu.Lock()
				asked = append(asked, req.ID)
				mu.Unlock()
				return nil
			},
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			Timeout: 60 * time.Millisecond, StatusDelay: 25 * time.Millisecond,
			QuestionTimeout: 10 * time.Second,
		})
	}()

	oc.feed(msgUpdated("s1", "m1"), questionAsked("s1", "que_1"))
	time.Sleep(200 * time.Millisecond) // > Timeout and status delay
	oc.feed(questionReplied("s1", "que_1"),
		textPartEvent("s1", "m1", "p1", "answer after the wait"), idle("s1"))

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != "que_1" {
		t.Errorf("asked = %v, want [que_1]", asked)
	}
	mu.Unlock()
	if oc.wasAborted() {
		t.Error("turn aborted while a question was pending")
	}
	wantSent(t, s, "answer after the wait")
	if status := s.statusSnapshot(); len(status) != 0 {
		t.Errorf("status fired during the question wait: %q", status)
	}
}

func TestExistingStatusTransitionsToWaitingAndBack(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send, Status: s,
			AskQuestion: func(context.Context, *opencode.QuestionRequest) error { return nil },
			SessionID:   "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 15 * time.Millisecond, StatusInterval: time.Second,
			Timeout: time.Second, QuestionTimeout: time.Second,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"))
	time.Sleep(20 * time.Millisecond)
	oc.feed(questionAsked("s1", "que_1"))
	time.Sleep(5 * time.Millisecond)
	oc.feed(questionReplied("s1", "que_1"))
	time.Sleep(5 * time.Millisecond)
	oc.feed(textPartEvent("s1", "m1", "p1", "done"), idle("s1"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	statuses := s.statusSnapshot()
	if len(statuses) != 3 {
		t.Fatalf("status = %q, want working, waiting, working", statuses)
	}
	if !strings.Contains(statuses[0], "**Working for ") ||
		!strings.Contains(statuses[1], "**Waiting for your input**") ||
		!strings.Contains(statuses[2], "**Working for ") {
		t.Errorf("status transitions = %q", statuses)
	}
}

func TestFinalSendFailureStillTerminalizesVisibleStatus(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	wantErr := errors.New("Lark unavailable")
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc,
			Send: func(context.Context, string) error {
				return wantErr
			},
			Status: s, SessionID: "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 15 * time.Millisecond, Timeout: time.Second,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"))
	time.Sleep(20 * time.Millisecond)
	oc.feed(textPartEvent("s1", "m1", "p1", "done"), idle("s1"))
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("StreamReply error = %v, want %v", err, wantErr)
	}
	finished := s.finishedSnapshot()
	if len(finished) != 1 || !strings.Contains(finished[0], "final reply could not be delivered") {
		t.Fatalf("finished = %q, want delivery warning", finished)
	}
}

func TestShutdownTerminalizesVisibleStatusWithDetachedContext(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(ctx, Params{
			OpenCode: oc, Send: s.send, Status: s,
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			StatusDelay: 15 * time.Millisecond, Timeout: time.Second,
		})
	}()
	oc.feed(msgUpdated("s1", "m1"))
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("StreamReply error = %v, want context cancellation", err)
	}
	finished := s.finishedSnapshot()
	if len(finished) != 1 || !strings.Contains(finished[0], "**Interrupted after ") {
		t.Fatalf("finished = %q, want interrupted receipt", finished)
	}
}

func TestQuestionTimeoutExpiresAndTurnContinues(t *testing.T) {
	oc := newGatedOpenCode()
	s := &sink{}
	expired := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send,
			AskQuestion: func(context.Context, *opencode.QuestionRequest) error { return nil },
			ExpireQuestions: func(context.Context) {
				select {
				case expired <- struct{}{}:
				default:
				}
			},
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			Timeout: 10 * time.Second, QuestionTimeout: 40 * time.Millisecond,
		})
	}()

	oc.feed(msgUpdated("s1", "m1"), questionAsked("s1", "que_1"))
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("ExpireQuestions was never called")
	}
	// The registry's reject produces this event; the turn then finishes.
	oc.feed(questionRejected("s1", "que_1"),
		textPartEvent("s1", "m1", "p1", "went on without you"), idle("s1"))

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if oc.wasAborted() {
		t.Error("question expiry must not abort the turn")
	}
	wantSent(t, s, "went on without you")
}

func TestAskFailureKeepsTurnClockRunning(t *testing.T) {
	// When the card can't be posted the registry rejects the question, so the
	// streamer must NOT suspend the clock — otherwise a failed ask would hang
	// the turn forever.
	oc := newGatedOpenCode()
	s := &sink{}
	done := make(chan error, 1)
	go func() {
		done <- StreamReply(t.Context(), Params{
			OpenCode: oc, Send: s.send,
			AskQuestion: func(context.Context, *opencode.QuestionRequest) error {
				return errors.New("Lark is down")
			},
			SessionID: "s1", Prompt: "hi", Directory: "/w",
			Timeout: 50 * time.Millisecond, QuestionTimeout: 10 * time.Second,
		})
	}()

	oc.feed(msgUpdated("s1", "m1"), questionAsked("s1", "que_1"))
	// No more events: the turn must end via its own timeout.
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !oc.wasAborted() {
		t.Error("turn was not aborted by the timeout")
	}
	sent := s.sentSnapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "limit") {
		t.Errorf("sent = %v, want the timeout reply", sent)
	}
}

func TestQuestionEventsForOtherSessionsOrWithoutHandlerAreIgnored(t *testing.T) {
	// A question for another session must not invoke the callback; with a nil
	// AskQuestion the event is ignored entirely (pre-question behavior).
	called := false
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		questionAsked("s2", "que_other"),
		questionReplied("s1", "que_unknown"), // unknown id: a no-op
		textPartEvent("s1", "m1", "p1", "hi"),
		idle("s1"),
	)
	s := &sink{}
	err := StreamReply(t.Context(), Params{
		OpenCode: oc, Send: s.send,
		AskQuestion: func(context.Context, *opencode.QuestionRequest) error {
			called = true
			return nil
		},
		SessionID: "s1", Prompt: "hello", Directory: "/w",
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("AskQuestion called for another session's question")
	}
	wantSent(t, s, "hi")

	// And the nil-AskQuestion path: same script, no handler, no crash.
	oc2 := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		questionAsked("s1", "que_1"),
		textPartEvent("s1", "m1", "p1", "hi"),
		idle("s1"),
	)
	s2 := &sink{}
	if err := StreamReply(t.Context(), Params{
		OpenCode: oc2, Send: s2.send, SessionID: "s1", Prompt: "x", Directory: "/w",
	}); err != nil {
		t.Fatal(err)
	}
	wantSent(t, s2, "hi")
}

// --- provider retries ----------------------------------------------------------

func TestProviderRetryIsSilentAndDoesNotChangePolicy(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		retryStatus("s1", 1, "The usage limit has been reached"),
		textPartEvent("s1", "m1", "p1", "recovered"),
		idle("s1"),
	)
	s := &sink{}
	err := StreamReply(t.Context(), Params{
		OpenCode: oc, Send: s.send, Status: s,
		SessionID: "s1", Prompt: "hi", Directory: "/home/ubuntu/otp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if oc.wasAborted() {
		t.Error("session aborted based on provider message")
	}
	wantSent(t, s, "recovered")
	if status := s.statusSnapshot(); len(status) != 0 {
		t.Errorf("status = %q, want none for a short recovered turn", status)
	}
}

func TestProviderRetryPolicyRemainsWithOpenCode(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		retryStatus("s1", 100, "Provider is overloaded"),
		textPartEvent("s1", "m1", "p1", "eventually recovered"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	if oc.wasAborted() {
		t.Error("iu aborted a retry owned by OpenCode")
	}
	wantSent(t, s, "eventually recovered")
}

func TestRetryStatusForOtherSessionIsIgnored(t *testing.T) {
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		retryStatus("other", 1, "The usage limit has been reached"),
		textPartEvent("s1", "m1", "p1", "fine"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	if oc.wasAborted() {
		t.Error("session aborted on another session's retry")
	}
	wantSent(t, s, "fine")
}

func TestNonRetryStatusIsIgnored(t *testing.T) {
	busy := &opencode.SessionStatus{SessionID: "s1"}
	busy.Status.Type = "busy"
	oc := newFakeOpenCode(
		msgUpdated("s1", "m1"),
		opencode.Event{SessionStatus: busy},
		textPartEvent("s1", "m1", "p1", "ok"),
		idle("s1"),
	)
	s := &sink{}
	run(t, oc, s)
	if oc.wasAborted() {
		t.Error("session aborted on a non-retry status")
	}
	wantSent(t, s, "ok")
}
