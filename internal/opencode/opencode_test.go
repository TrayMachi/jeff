package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func healthHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": version})
	}
}

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "opencode", "secret", nil)
}

func TestReadyWhenVersionMatchesPin(t *testing.T) {
	var path atomic.Value
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		healthHandler("1.15.13")(w, r)
	}))
	if err := c.CheckReady(t.Context(), "1.15.13"); err != nil {
		t.Fatal(err)
	}
	if got := path.Load(); got != "/global/health" {
		t.Errorf("path = %v, want /global/health", got)
	}
}

func TestVersionMismatchFailsFast(t *testing.T) {
	c := newTestClient(t, healthHandler("1.16.0"))
	err := c.CheckReady(t.Context(), "1.15.13")
	if err == nil {
		t.Fatal("CheckReady succeeded, want version-mismatch error")
	}
	for _, want := range []string{"1.16.0", "1.15.13", "deploy/opencode.version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestMissingVersionFieldCountsAsMismatch(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true})
	}))
	err := c.CheckReady(t.Context(), "1.15.13")
	if err == nil || !strings.Contains(err.Error(), "missing version") {
		t.Errorf("err = %v, want missing-version error", err)
	}
}

func TestAuthRejectionFailsFast(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	err := c.CheckReady(t.Context(), "1.15.13")
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("err = %v, want credentials error", err)
	}
}

func TestProbeThatHangsDoesNotWedgeBoot(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "opencode", "secret", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.CheckReady(ctx, "1.15.13")
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CheckReady wedged on a hung probe")
	}
}

func TestRequestsCarryBasicAuth(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Errorf("Authorization = %q, want Basic", r.Header.Get("Authorization"))
		}
		healthHandler("1.15.13")(w, r)
	}))
	if err := c.CheckReady(t.Context(), "1.15.13"); err != nil {
		t.Fatal(err)
	}
}

func TestNoPasswordSendsNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("Authorization = %q, want unset", h)
		}
		healthHandler("1.15.13")(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "opencode", "", nil)
	if err := c.CheckReady(t.Context(), "1.15.13"); err != nil {
		t.Fatal(err)
	}
}

func TestAllowAllPermissionsOrdering(t *testing.T) {
	// opencode resolves permissions with findLast, so the broad external deny
	// must precede every allow-list rule, otherwise it would shadow the allows.
	rules := permissionRules("/home/tray/projects/demo")
	if r := rules[0]; r["permission"] != "*" || r["action"] != "allow" {
		t.Fatalf("first rule = %v, want catch-all allow", r)
	}
	if r := rules[1]; r["permission"] != "external_directory" || r["pattern"] != "**" || r["action"] != "deny" {
		t.Fatalf("second rule = %v, want external_directory ** deny", r)
	}
	for i, r := range rules {
		if r["permission"] == "external_directory" && r["action"] == "deny" && i != 1 {
			t.Errorf("deny rule at index %d, want only index 1", i)
		}
		if r["permission"] == "external_directory" && r["action"] == "allow" && i < 2 {
			t.Errorf("allow rule at index %d precedes the default deny", i)
		}
	}
	// Every allowed root must end its glob in /** so it matches opencode's asked
	// "<dir>/*" form (a bare dir without a wildcard never matches).
	for _, dir := range rules[2:] {
		if !strings.HasSuffix(dir["pattern"], "/**") {
			t.Errorf("allowed dir %q must end in /**", dir)
		}
	}
}

func TestCreateSessionSendsAllowAllRuleset(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != "/work" {
			t.Errorf("directory = %q, want /work", got)
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Permission []map[string]string `json:"permission"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		var rawBody map[string]any
		if err := json.Unmarshal(raw, &rawBody); err != nil {
			t.Fatal(err)
		}
		if _, ok := rawBody["title"]; ok {
			t.Error("title present, want opencode to own session naming")
		}
		// The ruleset must lead with the catch-all allow, then an
		// external_directory:** deny, then specific external-directory allows.
		// The deny is required because iu has no permission UI: unknown external
		// reads must fail fast instead of resolving to "ask" and deadlocking.
		if len(body.Permission) < 2 {
			t.Fatalf("permission = %v, want at least catch-all allow and external_directory deny", body.Permission)
		}
		if r := body.Permission[0]; r["permission"] != "*" || r["pattern"] != "*" || r["action"] != "allow" {
			t.Errorf("rule[0] = %v, want catch-all allow", r)
		}
		if r := body.Permission[1]; r["permission"] != "external_directory" || r["pattern"] != "**" || r["action"] != "deny" {
			t.Errorf("rule[1] = %v, want external_directory ** deny", r)
		}
		// Every remaining rule must be an external_directory allow, and each must
		// come AFTER the ** deny so it wins under opencode's findLast.
		for _, r := range body.Permission[2:] {
			if r["permission"] != "external_directory" || r["action"] != "allow" {
				t.Errorf("trailing rule = %v, want external_directory allow", r)
			}
		}
		if len(body.Permission) != len(permissionRules("/work")) {
			t.Errorf("got %d permission rules, want %d", len(body.Permission), len(permissionRules("/work")))
		}
		wantPatterns := []string{
			"/home/tray/projects/*-worktrees/**",
			"/home/tray/projects/work/*-worktrees/**",
			"/work/**",
			"/tmp/opencode/**",
			"/home/tray/projects/work/**",
		}
		for i, want := range wantPatterns {
			if got := body.Permission[i+2]["pattern"]; got != want {
				t.Errorf("permission rule %d = %q, want %q", i+2, got, want)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_123"})
	}))
	id, err := c.CreateSession(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ses_123" {
		t.Errorf("id = %q, want ses_123", id)
	}
}

func TestGetSessionFetchesSessionState(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/ses_123" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != "/work" {
			t.Errorf("directory = %q, want /work", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":        "ses_123",
			"title":     "Fix login",
			"directory": "/work",
			"agent":     "build",
			"model": map[string]any{
				"providerID": "anthropic",
				"id":         "claude-opus-4-8",
				"variant":    "high",
			},
			"tokens": map[string]any{"input": 1000, "output": 200, "reasoning": 50},
			"cost":   0.0123,
		})
	}))
	s, err := c.GetSession(t.Context(), "ses_123", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "ses_123" || s.Title != "Fix login" || s.Directory != "/work" || s.Agent != "build" {
		t.Errorf("session = %+v", s)
	}
	if s.Model.Provider != "anthropic" || s.Model.ID != "claude-opus-4-8" || s.Model.Variant != "high" {
		t.Errorf("model = %+v", s.Model)
	}
	if s.Cost == nil || *s.Cost != 0.0123 {
		t.Errorf("cost = %v", s.Cost)
	}
}

func TestGetLatestAssistantContextUsesNewestAssistantWithOutput(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/ses_123/message" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != "/work" {
			t.Errorf("directory = %q, want /work", got)
		}
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Errorf("limit = %q, want 20", got)
		}
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{"info": map[string]any{
				"role": "assistant", "providerID": "openai", "modelID": "old",
				"tokens": map[string]any{"input": 900000, "output": 1000},
			}},
			map[string]any{"info": map[string]any{
				"role": "assistant", "providerID": "anthropic", "modelID": "latest",
				"tokens": map[string]any{
					"input": 8000, "output": 3000, "reasoning": 1000,
					"cache": map[string]any{"read": 4000, "write": 1000},
				},
			}},
			map[string]any{"info": map[string]any{"role": "user"}},
		})
	}))

	context, err := c.GetLatestAssistantContext(t.Context(), "ses_123", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if context == nil {
		t.Fatal("context = nil")
	}
	if context.Provider != "anthropic" || context.Model != "latest" {
		t.Errorf("context model = %s/%s", context.Provider, context.Model)
	}
	if context.Tokens.Input != 8000 || context.Tokens.Cache.Read != 4000 {
		t.Errorf("tokens = %+v", context.Tokens)
	}
}

func TestGetSessionReportsNotFound(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := c.GetSession(t.Context(), "ses_missing", "/work")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestModelContextLimit(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider" {
			t.Errorf("path = %q, want /provider", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != "/work" {
			t.Errorf("directory = %q, want /work", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"all": []any{
				map[string]any{
					"id": "anthropic",
					"models": map[string]any{
						"claude-opus-4-8": map[string]any{
							"limit": map[string]any{"context": 200000},
						},
					},
				},
			},
		})
	}))

	limit, err := c.ModelContextLimit(t.Context(), "anthropic", "claude-opus-4-8", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 200000 {
		t.Errorf("limit = %v, want 200000", limit)
	}
}

func TestPromptBodyShape(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/s1/prompt_async" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != "/work" {
			t.Errorf("directory = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		parts := body["parts"].([]any)
		if len(parts) != 2 {
			t.Fatalf("parts = %v, want preamble + text", parts)
		}
		if parts[0].(map[string]any)["text"] != "[Context] who" {
			t.Errorf("first part = %v, want the preamble", parts[0])
		}
		model := body["model"].(map[string]any)
		if model["providerID"] != "anthropic" || model["modelID"] != "claude-opus-4-8" {
			t.Errorf("model = %v", model)
		}
		if body["variant"] != "high" {
			t.Errorf("variant = %v, want high", body["variant"])
		}
		w.WriteHeader(http.StatusOK)
	}))
	err := c.PromptAsync(t.Context(), PromptParams{
		SessionID: "s1", Text: "hello", Directory: "/work",
		Provider: "anthropic", Model: "claude-opus-4-8", Effort: "high",
		Preamble: "[Context] who",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPromptOmitsUnsetFields(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if _, ok := body["model"]; ok {
			t.Error("model present, want omitted")
		}
		if _, ok := body["variant"]; ok {
			t.Error("variant present, want omitted")
		}
		if parts := body["parts"].([]any); len(parts) != 1 {
			t.Errorf("parts = %v, want just the text", parts)
		}
		w.WriteHeader(http.StatusOK)
	}))
	if err := c.PromptAsync(t.Context(), PromptParams{SessionID: "s1", Text: "hi", Directory: "/w"}); err != nil {
		t.Fatal(err)
	}
}

// --- the SSE stream ---------------------------------------------------------

func sseLine(eventType string, properties map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": eventType, "properties": properties})
	return "data: " + string(b) + "\n\n"
}

func TestSubscribeDeliversDecodedEventsAndCleanEOF(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("directory"); got != "/work" {
			t.Errorf("directory = %q, want /work (the load-bearing scope param)", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseLine("message.updated", map[string]any{
			"info": map[string]any{"id": "m1", "sessionID": "s1", "role": "assistant"},
		}))
		_, _ = io.WriteString(w, sseLine("some.future.event", map[string]any{"x": 1}))
		_, _ = io.WriteString(w, sseLine("message.part.updated", map[string]any{
			"part": map[string]any{
				"id": "p1", "messageID": "m1", "sessionID": "s1",
				"type": "text", "text": "hello",
			},
		}))
		_, _ = io.WriteString(w, sseLine("session.idle", map[string]any{"sessionID": "s1"}))
		// Stream then ends without error: a clean EOF.
	}))

	stream, err := c.Subscribe(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range stream.Events() {
		got = append(got, ev)
	}
	if stream.Err() != nil {
		t.Errorf("Err = %v, want nil (clean EOF)", stream.Err())
	}
	// The unknown event type was skipped.
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].MessageUpdated == nil || got[0].MessageUpdated.Info.ID != "m1" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].PartUpdated == nil || got[1].PartUpdated.Part.Text != "hello" {
		t.Errorf("event 1 = %+v", got[1])
	}
	if got[2].SessionIdle == nil || got[2].SessionIdle.SessionID != "s1" {
		t.Errorf("event 2 = %+v", got[2])
	}
}

func TestSubscribeRejectsMalformedKnownEvent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"session.idle","properties":[]}`+"\n\n")
	}))

	stream, err := c.Subscribe(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Events() {
	}
	if stream.Err() == nil {
		t.Fatal("Err = nil, want malformed event error")
	}
}

func TestSubscribeIgnoresWellFormedUnknownEvent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseLine("some.future.event", map[string]any{"x": 1}))
	}))

	stream, err := c.Subscribe(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Events() {
		t.Fatal("unknown event was delivered")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v, want clean EOF", err)
	}
}

func TestSubscribeTransportErrorMidTurn(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseLine("message.updated", map[string]any{
			"info": map[string]any{"id": "m1", "sessionID": "s1", "role": "assistant"},
		}))
		w.(http.Flusher).Flush()
		// Kill the connection mid-stream: a transport error, not an EOF.
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))

	stream, err := c.Subscribe(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range stream.Events() {
		n++
	}
	if n != 1 {
		t.Errorf("events before failure = %d, want 1", n)
	}
	if stream.Err() == nil {
		t.Error("Err = nil, want a transport error")
	}
}

func TestSubscribeFailsFastOnBadStatus(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, err := c.Subscribe(t.Context(), "/work"); err == nil {
		t.Fatal("Subscribe succeeded, want error (surfaced without prompting)")
	}
}

func TestSubscribeReturnsOnlyOnceEstablished(t *testing.T) {
	// Subscribe must not return before the stream is open — the
	// subscribe-before-prompt ordering depends on it.
	release := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseLine("session.idle", map[string]any{"sessionID": "s1"}))
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, err := c.Subscribe(t.Context(), "/w")
		if err != nil {
			t.Error(err)
			return
		}
		for range stream.Events() { //nolint:revive // drain
		}
	}()
	select {
	case <-done:
		t.Fatal("Subscribe returned before the server responded")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe never completed after release")
	}
}

func TestSubscribeDecodesQuestionEvents(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseLine("question.asked", map[string]any{
			"id": "que_1", "sessionID": "s1",
			"questions": []map[string]any{
				{
					"question": "Pick one.", "header": "Pick",
					"options": []map[string]any{
						{"label": "A", "description": "first"},
						{"label": "B", "description": "second"},
					},
					"multiple": true,
					// absent custom — opencode's "default: true" case
				},
				{
					"question": "Yes or no?", "header": "Confirm",
					"options": []map[string]any{},
					"custom":  false, // explicit opt-out for an internal confirmation
				},
			},
		}))
		_, _ = io.WriteString(w, sseLine("question.replied", map[string]any{
			"sessionID": "s1", "requestID": "que_1",
			"answers": [][]string{{"A"}},
		}))
		_, _ = io.WriteString(w, sseLine("question.rejected", map[string]any{
			"sessionID": "s1", "requestID": "que_2",
		}))
	}))

	stream, err := c.Subscribe(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range stream.Events() {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	asked := got[0].QuestionAsked
	if asked == nil || asked.ID != "que_1" || asked.SessionID != "s1" {
		t.Fatalf("event 0 = %+v", got[0])
	}
	if len(asked.Questions) != 2 {
		t.Fatalf("questions = %+v", asked.Questions)
	}
	q := asked.Questions[0]
	if q.Question != "Pick one." || q.Header != "Pick" || !q.Multiple {
		t.Errorf("question = %+v", q)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "A" || q.Options[1].Description != "second" {
		t.Errorf("options = %+v", q.Options)
	}
	// opencode's contract: custom defaults to true when absent, and the model
	// can never set it — only internals send an explicit false.
	if !q.AllowsCustom() {
		t.Error("absent custom must default to allowed")
	}
	if asked.Questions[1].AllowsCustom() {
		t.Error("explicit custom: false must be honored")
	}
	if r := got[1].QuestionReplied; r == nil || r.RequestID != "que_1" || r.SessionID != "s1" {
		t.Errorf("event 1 = %+v", got[1])
	}
	if r := got[2].QuestionRejected; r == nil || r.RequestID != "que_2" {
		t.Errorf("event 2 = %+v", got[2])
	}
}

func TestReplyQuestionBodyShape(t *testing.T) {
	var path atomic.Value
	var body atomic.Value
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery)
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.WriteHeader(http.StatusOK)
	}))
	err := c.ReplyQuestion(t.Context(), "que_1", [][]string{{"A", "custom text"}, {"B"}}, "/w")
	if err != nil {
		t.Fatal(err)
	}
	if got := path.Load(); got != "POST /question/que_1/reply?directory=%2Fw" {
		t.Errorf("request = %v", got)
	}
	var decoded struct {
		Answers [][]string `json:"answers"`
	}
	if err := json.Unmarshal([]byte(body.Load().(string)), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Answers) != 2 || decoded.Answers[0][1] != "custom text" {
		t.Errorf("answers = %+v", decoded.Answers)
	}
}

func TestReplyQuestionSurfacesHTTPError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if err := c.ReplyQuestion(t.Context(), "que_gone", [][]string{{"A"}}, "/w"); err == nil {
		t.Fatal("ReplyQuestion succeeded, want error on 404")
	}
}

func TestRejectQuestionHitsEndpoint(t *testing.T) {
	var path atomic.Value
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	if err := c.RejectQuestion(t.Context(), "que_1", "/w"); err != nil {
		t.Fatal(err)
	}
	if got := path.Load(); got != "POST /question/que_1/reject" {
		t.Errorf("request = %v", got)
	}
}

func TestListQuestions(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /question" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		// Directory-scoped, like /event: omitting it lists nothing.
		if got := r.URL.Query().Get("directory"); got != "/w" {
			t.Errorf("directory = %q, want /w", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "que_1", "sessionID": "s1", "questions": []map[string]any{
				{"question": "Q?", "header": "H", "options": []map[string]any{}},
			}},
		})
	}))
	got, err := c.ListQuestions(t.Context(), "/w")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "que_1" {
		t.Errorf("questions = %+v", got)
	}
}

func TestAbortSessionHitsEndpoint(t *testing.T) {
	var path atomic.Value
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	if err := c.AbortSession(t.Context(), "s1", "/w"); err != nil {
		t.Fatal(err)
	}
	if got := path.Load(); got != "POST /session/s1/abort" {
		t.Errorf("request = %v", got)
	}
}

func TestAbortSessionReturnsFailure(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	if err := c.AbortSession(t.Context(), "s1", "/w"); err == nil {
		t.Fatal("AbortSession succeeded, want error")
	}
}

func TestSessionExists(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{name: "OK", status: http.StatusOK, want: true},
		{name: "NoContent", status: http.StatusNoContent, want: true},
		{name: "NotFound", status: http.StatusNotFound},
		{name: "Unauthorized", status: http.StatusUnauthorized, wantErr: true},
		{name: "ServerError", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			got, err := c.SessionExists(t.Context(), "ses_1", "/w")
			if (err != nil) != tt.wantErr {
				t.Fatalf("SessionExists() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("SessionExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

var errSentinel = errors.New("sentinel")

func TestStreamErrSeam(t *testing.T) {
	// NewStream is the streamer tests' seam; Err must reflect the pointed-to
	// error once the channel closes.
	ch := make(chan Event)
	err := error(nil)
	s := NewStream(ch, func() error { return err })
	close(ch)
	if s.Err() != nil {
		t.Errorf("Err = %v, want nil", s.Err())
	}
	err = errSentinel
	if !errors.Is(s.Err(), errSentinel) {
		t.Errorf("Err = %v, want sentinel", s.Err())
	}
}

func TestPromptAccepts204(t *testing.T) {
	// prompt_async answers 204 No Content in production — any 2xx is success
	// (the Python client's raise_for_status semantics).
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := c.PromptAsync(t.Context(), PromptParams{SessionID: "s1", Text: "hi", Directory: "/w"}); err != nil {
		t.Fatal(err)
	}
}

func TestTodoStateDistinguishesAbsentAndEmptyLists(t *testing.T) {
	absent := Part{State: json.RawMessage(`{"input":{}}`)}
	if todos, ok := absent.TodoState(); ok || todos != nil {
		t.Fatalf("absent TodoState = (%v, %v), want (nil, false)", todos, ok)
	}

	empty := Part{State: json.RawMessage(`{"input":{"todos":[]}}`)}
	if todos, ok := empty.TodoState(); !ok || len(todos) != 0 {
		t.Fatalf("empty TodoState = (%v, %v), want (empty, true)", todos, ok)
	}
}
