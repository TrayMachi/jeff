// Package opencode is a thin client for iu's dedicated opencode server.
//
// Iu is a client of a long-lived OpenCode server. This wrapper owns three
// concerns:
//
//  1. Authenticated transport — the server uses HTTP Basic auth; the
//     Authorization header is injected on every request, including the SSE
//     stream.
//  2. Readiness — check /global/health before serving traffic, asserting the
//     server version matches the pin in
//     deploy/opencode.version (this hand-rolled client is written against a
//     specific server, so a drifted binary must not boot half-working).
//  3. A minimal session surface — create a session, send a prompt, abort a
//     turn, and expose the SSE event stream the streamer consumes.
//
// Iu has no interactive tool-approval UI, so sessions are created with an
// allow-all permission ruleset (with a deny list for secret directories — see
// allowAllPermissions): the agent runs its tools autonomously (it runs in the
// trusted OTP workspace). There is therefore no permission.asked handling here,
// and the ruleset must leave no permission resolving to "ask" or the turn hangs
// until it times out. The *question* tool is different: it is not a permission
// gate — the agent explicitly asks the user something and blocks until
// ReplyQuestion or RejectQuestion (iu renders it as an interactive Lark
// card, see internal/question).
//
// Endpoints (from the OpenAPI spec at /doc; the consumed subset is snapshot
// in deploy/opencode-api-snapshot.json):
//
//	GET  /global/health                readiness + server version
//	GET  /provider                     model context limits
//	POST /session                      create a session
//	GET  /session/{id}                 fetch a session (existence check)
//	GET  /session/{id}/message         fetch recent message context
//	POST /session/{id}/prompt_async    send a message, return immediately
//	POST /session/{id}/abort           cancel the running turn (best-effort)
//	GET  /event                        SSE stream of all server events
//	GET  /question                     pending question requests (all sessions)
//	POST /question/{id}/reply          answer a question request
//	POST /question/{id}/reject         reject a question request
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound reports a 404 from an opencode read endpoint.
var ErrNotFound = errors.New("opencode resource not found")

// maxHealthProbe bounds the startup preflight because the SSE-capable HTTP
// client intentionally has no process-wide timeout.
const maxHealthProbe = 5 * time.Second

// allowAllPermissions runs tools without prompting: iu has no approval UI, so
// any rule resolving to "ask" deadlocks the turn until it times out.
//
// Two opencode quirks drive the shape below. (1) Its matcher is minimatch with
// dot:true, so "*" matches one path segment and does NOT cross "/"; the
// external_directory permission is asked with an absolute path, which the
// catch-all "*" never matches — only "**" does. (2) It resolves with findLast
// (last match wins) over agent+session rules, so order is load-bearing.
//
// external_directory only gates paths outside the workspace. Jeff denies
// external paths by default and appends the selected project's allow rule.

// allowedExternalDirs are absolute-path globs the agent may read after it
// reaches outside a workspace. Keep this as a small allow-list: adding a broad
// home-directory rule would re-expose credential stores and other sessions'
// opencode state.
var allowedExternalDirs = []string{
	"/tmp/opencode/**",
	"/home/tray/go/pkg/mod/**",
	"/home/tray/.agents/skills/**",
	"/home/tray/.claude/skills/**",
	"/home/tray/.config/opencode/skills/**",
}

func permissionRules(directory string) []map[string]string {
	rules := []map[string]string{
		{"permission": "*", "pattern": "*", "action": "allow"},
		{"permission": "external_directory", "pattern": "**", "action": "deny"},
	}

	rules = append(rules,
		map[string]string{
			"permission": "external_directory",
			"pattern":    "/home/tray/projects/*-worktrees/**",
			"action":     "allow",
		},
	)

	for _, dir := range append([]string{strings.TrimRight(strings.TrimSpace(directory), "/") + "/**"}, allowedExternalDirs...) {
		rules = append(rules, map[string]string{"permission": "external_directory", "pattern": dir, "action": "allow"})
	}
	return rules
}

// Client talks to one opencode server.
type Client struct {
	baseURL string
	auth    string // Authorization header value, or ""
	http    *http.Client
}

// NewClient builds a client for the server at baseURL with HTTP Basic auth
// (no Authorization header when password is empty, matching the Python
// client). httpClient overrides the transport (the test seam); nil uses a
// default with a bounded connect via the request context.
func NewClient(baseURL, username, password string, httpClient *http.Client) *Client {
	auth := ""
	if password != "" {
		auth = "Basic " + basicToken(username, password)
	}
	if httpClient == nil {
		// No overall timeout: the SSE stream is open-ended. Per-call bounds
		// come from the caller's context.
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    auth,
		http:    httpClient,
	}
}

// is2xx mirrors httpx's raise_for_status: any 2xx is success.
func is2xx(status int) bool {
	return status >= 200 && status < 300
}

func basicToken(username, password string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://x", nil) //nolint:noctx // header construction only, never sent
	req.SetBasicAuth(username, password)
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
}

func (c *Client) newRequest(ctx context.Context, method, path, directory string, body any) (*http.Request, error) {
	u := c.baseURL + path
	if directory != "" {
		u += "?" + url.Values{"directory": {directory}}.Encode()
	}
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	return req, nil
}

// do runs one request/response cycle: build the request, send it, require a
// 2xx (op labels the failure), and decode the body into out when non-nil.
// SessionExists, checkHealth, and Subscribe stay hand-rolled — they branch on
// status or keep the body open.
func (c *Client) do(ctx context.Context, method, path, directory string, body, out any, op string) error {
	req, err := c.newRequest(ctx, method, path, directory, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if !is2xx(resp.StatusCode) {
		return fmt.Errorf("%s: HTTP %d", op, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CheckReady performs one bounded startup health and version check. Process
// restart policy belongs to the service manager, not this client.
func (c *Client) CheckReady(ctx context.Context, expectedVersion string) error {
	probeCtx, cancel := context.WithTimeout(ctx, maxHealthProbe)
	defer cancel()
	req, err := c.newRequest(probeCtx, http.MethodGet, "/global/health", "", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencode health check: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var health struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			return fmt.Errorf("decode opencode health response: %w", err)
		}
		if health.Version == "" {
			return errors.New("opencode health response is missing version")
		}
		if health.Version != expectedVersion {
			//nolint:staticcheck // operator-facing boot message
			return fmt.Errorf(
				"opencode at %s runs v%s, but iu is pinned to v%s (deploy/opencode.version). "+
					"Align the server binary and the pin before starting iu.",
				c.baseURL, health.Version, expectedVersion)
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		//nolint:staticcheck // operator-facing boot message
		return fmt.Errorf(
			"opencode rejected credentials (HTTP %d) at %s. Check OPENCODE_SERVER_PASSWORD / "+
				"OPENCODE_SERVER_USERNAME match the server.", resp.StatusCode, c.baseURL)
	default:
		return fmt.Errorf("opencode health check: HTTP %d", resp.StatusCode)
	}
}

// CreateSession creates a new session in directory (the context's workspace)
// and returns its id, with the allow-all permission ruleset.
func (c *Client) CreateSession(ctx context.Context, directory string) (string, error) {
	var body struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/session", directory, map[string]any{
		"permission": permissionRules(directory),
	}, &body, "create session")
	if err != nil {
		return "", err
	}
	return body.ID, nil
}

// Session is the session-level state returned by opencode.
type Session struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Directory string   `json:"directory"`
	Agent     string   `json:"agent"`
	Cost      *float64 `json:"cost"`
	Model     struct {
		ID       string `json:"id"`
		Provider string `json:"providerID"`
		Variant  string `json:"variant"`
	} `json:"model"`
}

// Tokens is opencode's usage counter shape on assistant messages.
type Tokens struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	Reasoning float64 `json:"reasoning"`
	Cache     struct {
		Read  float64 `json:"read"`
		Write float64 `json:"write"`
	} `json:"cache"`
}

// AssistantContext is the model and token snapshot for one assistant message.
type AssistantContext struct {
	Provider string `json:"providerID"`
	Model    string `json:"modelID"`
	Tokens   Tokens `json:"tokens"`
}

// ModelContextLimit returns the configured context-window size for a model.
func (c *Client) ModelContextLimit(ctx context.Context, providerID, modelID, directory string) (float64, error) {
	var response struct {
		All []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				Limit struct {
					Context float64 `json:"context"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"all"`
	}
	if err := c.do(ctx, http.MethodGet, "/provider", directory, nil, &response, "list providers"); err != nil {
		return 0, err
	}
	for _, provider := range response.All {
		if provider.ID != providerID {
			continue
		}
		if limit := provider.Models[modelID].Limit.Context; limit > 0 {
			return limit, nil
		}
		break
	}
	return 0, fmt.Errorf("opencode model %s/%s has no context limit", providerID, modelID)
}

// GetSession fetches opencode's current session state.
func (c *Client) GetSession(ctx context.Context, sessionID, directory string) (*Session, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/session/"+sessionID, directory, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case !is2xx(resp.StatusCode):
		return nil, fmt.Errorf("get session: HTTP %d", resp.StatusCode)
	}
	var out Session
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetLatestAssistantContext returns the context snapshot OpenCode's TUI uses:
// the newest assistant message that produced output.
func (c *Client) GetLatestAssistantContext(ctx context.Context, sessionID, directory string) (*AssistantContext, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/session/"+sessionID+"/message", directory, nil)
	if err != nil {
		return nil, err
	}
	query := req.URL.Query()
	query.Set("limit", "20")
	req.URL.RawQuery = query.Encode()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if !is2xx(resp.StatusCode) {
		return nil, fmt.Errorf("get session messages: HTTP %d", resp.StatusCode)
	}
	var messages []struct {
		Info struct {
			Role string `json:"role"`
			AssistantContext
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		info := &messages[i].Info
		if info.Role == "assistant" && info.Tokens.Output > 0 {
			return &info.AssistantContext, nil
		}
	}
	return nil, nil
}

// SessionExists reports whether a session still exists server-side. Only a
// 404 means absent; other failures are returned so callers do not replace a
// valid session during a server or authentication failure.
func (c *Client) SessionExists(ctx context.Context, sessionID, directory string) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/session/"+sessionID, directory, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case !is2xx(resp.StatusCode):
		return false, fmt.Errorf("check session: HTTP %d", resp.StatusCode)
	default:
		return true, nil
	}
}

// PromptParams carry one prompt_async call. Provider/Model/Effort are
// omitted from the request when unset so the server defaults; Preamble (the
// [Context] block) is sent as a leading text part.
type PromptParams struct {
	SessionID string
	Text      string
	Directory string
	Provider  string
	Model     string
	Effort    string
	Preamble  string
}

// PromptAsync sends a user message and returns immediately. The
// assistant's reply arrives over the event stream, which is what lets the
// streamer stream it back to Lark.
func (c *Client) PromptAsync(ctx context.Context, p PromptParams) error {
	parts := []map[string]any{}
	if p.Preamble != "" {
		parts = append(parts, map[string]any{"type": "text", "text": p.Preamble})
	}
	if p.Text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": p.Text})
	}
	body := map[string]any{"parts": parts}
	if p.Provider != "" && p.Model != "" {
		body["model"] = map[string]any{"providerID": p.Provider, "modelID": p.Model}
	}
	if p.Effort != "" {
		body["variant"] = p.Effort
	}
	return c.do(ctx, http.MethodPost, "/session/"+p.SessionID+"/prompt_async", p.Directory, body, nil, "prompt")
}

// AbortSession cancels the turn running in sessionID.
func (c *Client) AbortSession(ctx context.Context, sessionID, directory string) error {
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", directory, nil, nil, "abort session")
}

// ReplyQuestion answers a pending question request: one answer list per
// question, in question order, each holding the selected option labels and/or
// free-text answers. The agent's blocked turn resumes with them.
func (c *Client) ReplyQuestion(ctx context.Context, requestID string, answers [][]string, directory string) error {
	return c.do(ctx, http.MethodPost, "/question/"+requestID+"/reply", directory,
		map[string]any{"answers": answers}, nil, "reply question")
}

// RejectQuestion rejects a pending question request: the agent's question
// tool call fails and its turn resumes without an answer. Used when the
// question can't be presented, expires unanswered, or the turn is cancelled.
func (c *Client) RejectQuestion(ctx context.Context, requestID, directory string) error {
	return c.do(ctx, http.MethodPost, "/question/"+requestID+"/reject", directory, nil, nil, "reject question")
}

// ListQuestions returns the pending question requests in one workspace
// directory — boot-time cleanup rejects leftovers from a previous iu run,
// since their cards died with the process. Like /event, the endpoint is
// directory-scoped: without the directory it reports nothing.
func (c *Client) ListQuestions(ctx context.Context, directory string) ([]QuestionRequest, error) {
	var out []QuestionRequest
	if err := c.do(ctx, http.MethodGet, "/question", directory, nil, &out, "list questions"); err != nil {
		return nil, err
	}
	return out, nil
}

// Stream is an established SSE subscription. Events is closed when the stream
// ends; Err then reports the terminal transport error (nil on a clean EOF —
// the two failure shapes the streamer must distinguish).
type Stream struct {
	events   <-chan Event
	terminal func() error
}

// Events returns the stream's event channel.
func (s *Stream) Events() <-chan Event {
	return s.events
}

// Err reports the stream's terminal error. Valid only after Events is closed.
func (s *Stream) Err() error {
	return s.terminal()
}

// NewStream builds a Stream from raw parts. terminal is read only after events
// closes; channel close establishes visibility of the producer's final write.
func NewStream(events <-chan Event, terminal func() error) *Stream {
	return &Stream{events: events, terminal: terminal}
}

// Subscribe opens the server's SSE stream and returns once it is
// established, so the caller prompts only after subscribing (no
// subscribe-after-prompt race — this replaces the Python ready asyncio.Event).
//
// directory is **load-bearing**: opencode scopes message.* / session.*
// events to a worktree, so a /event subscription opened *without* the
// prompt's directory never sees the session deltas or session.idle the
// streamer waits on. Consumers still filter by sessionID since one worktree
// may host several sessions.
func (c *Client) Subscribe(ctx context.Context, directory string) (*Stream, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/event", directory, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req) //nolint:bodyclose // closed by the reader goroutine below
	if err != nil {
		return nil, err
	}
	if !is2xx(resp.StatusCode) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("event stream: HTTP %d", resp.StatusCode)
	}

	events := make(chan Event)
	var terminal error
	stream := NewStream(events, func() error { return terminal })
	go func() {
		defer close(events)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			payload, found := strings.CutPrefix(line, "data:")
			if !found {
				continue
			}
			payload = strings.TrimSpace(payload)
			if payload == "" {
				continue
			}
			event, known, err := decodeEvent([]byte(payload))
			if err != nil {
				terminal = fmt.Errorf("decode event: %w", err)
				return
			}
			if !known {
				continue
			}
			select {
			case events <- event:
			case <-ctx.Done():
				terminal = ctx.Err()
				return
			}
		}
		// Scanner errors are transport failures; a nil error is a clean EOF.
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			terminal = err
		} else if ctx.Err() != nil {
			terminal = ctx.Err()
		}
	}()
	return stream, nil
}
