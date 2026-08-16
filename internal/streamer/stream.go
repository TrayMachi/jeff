// StreamReply consumes one OpenCode turn and posts its finalized answer.
// Answer text is not edited live; one delayed run-status message provides
// progress while the turn runs:
//
//  1. Subscribe to the /event SSE stream (scoped to the context directory)
//     *before* prompting, so no early deltas are missed.
//  2. Send the prompt with prompt_async.
//  3. Accumulate assistant **answer text** parts (in arrival order), ignoring
//     tool parts and reasoning/thinking parts (those are hidden from the user).
//  4. On session.idle finalize: send the accumulated text (chunked) via the
//     injected Send func. On session.error (or a transport failure) send the
//     error text instead. Either way exactly one reply is produced.
//
// OpenCode owns provider retries and publishes session.status retry events.
// The streamer summarizes those as neutral run-status activity while leaving
// retry policy and details with OpenCode; only session.error is terminal.
//
// The Python version's three constructs — consume task, watchdog task,
// timed_out flag — collapse into one select loop over the SSE event channel,
// a ticker, and ctx.Done(). All turn state (text accumulation,
// last-activity, timed-out) is owned by that loop; the ticker and the
// timeout check are arms of the same select, never a second goroutine —
// that's the invariant that makes "no locking" true, and -race enforces it.
//
// The agent can also *ask the user a question* mid-turn (opencode's question
// tool): a question.asked event makes the injected AskQuestion post an
// interactive card and the agent blocks server-side. The answer flows back
// out-of-band (card callback → POST /question/{id}/reply, see
// internal/question) — the streamer only observes question.replied/.rejected
// on the same SSE stream, so the select loop stays the sole owner of turn
// state. While a question is pending the turn clock and status cadence are
// suspended (the wait is the human's, not the agent's) and QuestionTimeout
// bounds it: on expiry the questions are rejected and the turn resumes.
package streamer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/local/jeff/internal/opencode"
)

const emptyFallback = "The agent finished without producing any text."

// maxQuestionWait caps how long the turn clock stays suspended on an
// unanswered question card. A QuestionTimeout of 0 ("no explicit window")
// must not mean forever — the active stream watcher is occupied for the whole
// wait, and only /cancel or a restart could end it.
const maxQuestionWait = 24 * time.Hour

const (
	defaultStatusDelay    = time.Minute
	defaultStatusInterval = 3 * time.Minute
	// LiveStatusEditBudget leaves Lark's twentieth edit for the terminal
	// receipt. The app transport enforces the same budget.
	LiveStatusEditBudget = 19
)

// effectiveQuestionTimeout clamps the configured question window to
// (0, maxQuestionWait].
func effectiveQuestionTimeout(d time.Duration) time.Duration {
	if d <= 0 || d > maxQuestionWait {
		return maxQuestionWait
	}
	return d
}

// ErrContentRejected is what a RunStatus callback returns (wrapped) when the
// transport rejected the message *content* itself, not a transient hiccup —
// e.g. Lark's edge filter bouncing a payload. The same content is rejected on
// every push, so StreamReply stops the live checklist for the rest of the turn
// instead of re-submitting it each update. Transport-neutral on purpose: the
// injected callback translates its own error into this so the streamer stays
// transport-agnostic.
var ErrContentRejected = errors.New("transport rejected the message content")

// cancelledNotice is the reply when the turn was aborted from outside the
// streamer (an @iu /cancel, or an abort issued through the opencode
// UI/API). opencode marks the interrupted assistant message with
// MessageAbortedError.
const cancelledNotice = "🛑 Stopped."

// describeError extracts a readable line from an opencode session error
// payload.
func describeError(err *opencode.ErrorPayload) string {
	if err != nil {
		if err.Name == "MessageAbortedError" {
			return cancelledNotice
		}
		switch {
		case err.Name != "" && err.Data.Message != "":
			return fmt.Sprintf("%s: %s", err.Name, err.Data.Message)
		case err.Data.Message != "":
			return err.Data.Message
		case err.Name != "":
			return err.Name
		}
	}
	return "The agent reported an error."
}

// isAnswerTextPart reports whether a part is answer text the user should see.
func isAnswerTextPart(part *opencode.Part) bool {
	return part.Type == "text" && !part.Ignored
}

// todoIcons map an opencode todowrite status to a checklist icon for the
// progress view.
var todoIcons = map[string]string{
	"completed":   "✅",
	"in_progress": "🔄",
	"pending":     "⬜",
	"cancelled":   "❌",
}

// renderTodos renders an opencode todowrite list as checklist lines for the
// unified run status.
func renderTodos(todos []opencode.Todo) string {
	var lines []string
	for _, todo := range todos {
		label := strings.TrimSpace(todo.Content)
		if label == "" {
			continue
		}
		if todo.Status == "cancelled" {
			label = "~~" + label + "~~"
		}
		icon, ok := todoIcons[todo.Status]
		if !ok {
			icon = "⬜"
		}
		lines = append(lines, icon+" "+label)
	}
	return strings.Join(lines, "\n")
}

func sanitizeActivity(title, tool string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" && tool != "" {
		title = "Using " + tool
	}
	const maxRunes = 160
	runes := []rune(title)
	if len(runes) > maxRunes {
		title = string(runes[:maxRunes-1]) + "…"
	}
	return strings.NewReplacer(
		`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	).Replace(title)
}

func renderRunStatus(state string, elapsed time.Duration, actions int, todos []opencode.Todo, activity string) string {
	var header string
	if state == "Waiting for your input" {
		header = fmt.Sprintf("**%s** · %s · %s.", state, formatDuration(elapsed), formatActions(actions))
	} else {
		header = fmt.Sprintf("**%s for %s** · %s.", state, formatDuration(elapsed), formatActions(actions))
	}
	if checklist := renderTodos(todos); checklist != "" {
		return header + "\n\n" + checklist
	}
	if activity == "" {
		activity = "Still working."
	} else {
		activity = "Current: " + activity
	}
	return header + "\n\n" + activity
}

// textPart is one accumulated answer-text part.
type textPart struct {
	order     int
	text      string
	messageID string
}

// finalMessageText is the text to post: only the **last** assistant
// message's answer.
//
// A long agentic turn emits several assistant messages — interim narration
// between tool calls, then a closing answer. Concatenating them all dumps
// the whole monologue, so only the latest message that produced text is kept
// (falling back to the last one that did, so a turn ending on a tool call
// still has an answer). Its parts are joined in arrival order (message
// deltas).
func finalMessageText(textParts map[string]textPart, assistantOrder map[string]int) string {
	if len(textParts) == 0 {
		return ""
	}
	target := ""
	targetOrder := -2 // below the -1 of an unknown message id
	for _, p := range textParts {
		order, ok := assistantOrder[p.messageID]
		if !ok {
			order = -1
		}
		if order > targetOrder {
			target, targetOrder = p.messageID, order
		}
	}
	var ordered []textPart
	for _, p := range textParts {
		if p.messageID == target {
			ordered = append(ordered, p)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].order < ordered[j].order })
	var b strings.Builder
	for _, p := range ordered {
		b.WriteString(p.text)
	}
	return b.String()
}

// OpenCodeClient is the consumer-defined slice of the opencode client the
// streamer calls.
type OpenCodeClient interface {
	Subscribe(ctx context.Context, directory string) (*opencode.Stream, error)
	PromptAsync(ctx context.Context, p opencode.PromptParams) error
	AbortSession(ctx context.Context, sessionID, directory string) error
}

// Send posts one reply chunk back into the thread.
type Send func(ctx context.Context, text string) error

// RunStatus owns the one Lark status message for a long-running turn. Update
// posts or edits live state; Finish replaces an existing status with its
// terminal receipt and must not create one for a short turn.
type RunStatus interface {
	Update(ctx context.Context, text string) error
	Finish(ctx context.Context, text string) error
}

// AskQuestion presents an agent question request to the user (the question
// registry posts an interactive card). The answer is delivered out-of-band —
// a card callback POSTs it straight to opencode — so the streamer only
// observes the resume via question.replied/question.rejected on the same SSE
// stream.
type AskQuestion func(ctx context.Context, req *opencode.QuestionRequest) error

// ExpireQuestions rejects the turn's pending question requests (the question
// clock ran out) and freezes their cards.
type ExpireQuestions func(ctx context.Context)

// Params configure one StreamReply turn. StatusDelay and StatusInterval use
// production defaults when zero; a nil Status disables live status entirely.
// A nil AskQuestion ignores question events entirely.
type Params struct {
	OpenCode        OpenCodeClient
	Send            Send
	Status          RunStatus
	AskQuestion     AskQuestion
	ExpireQuestions ExpireQuestions
	// StreamReady is called after the SSE subscription is established and the
	// initial prompt is accepted. Callers that suppress duplicate stream watchers
	// use it to know when follow-up prompt_async requests can reuse this watcher.
	StreamReady func()
	SessionID   string
	Prompt      string
	Directory   string
	Provider    string
	Model       string
	Effort      string
	Preamble    string
	Timeout     time.Duration
	// These overrides keep timer tests fast. Zero uses one minute and three
	// minutes respectively.
	StatusDelay    time.Duration
	StatusInterval time.Duration
	// QuestionTimeout bounds how long a question card waits for a human. While
	// a question is pending the turn clock and status cadence are suspended — the
	// agent isn't slow, the human is — and this clock runs instead. On expiry
	// the questions are rejected and the turn resumes (and is judged by the
	// turn clock again). Clamped to (0, 24h]: 0 means "no explicit window",
	// not "wait forever".
	QuestionTimeout time.Duration
}

// PromptParams returns the OpenCode request shared by initial and follow-up
// prompt paths.
func (p Params) PromptParams() opencode.PromptParams {
	return opencode.PromptParams{
		SessionID: p.SessionID,
		Text:      p.Prompt,
		Directory: p.Directory,
		Provider:  p.Provider,
		Model:     p.Model,
		Effort:    p.Effort,
		Preamble:  p.Preamble,
	}
}

// StreamReply prompts the agent and posts its reply (once) into the thread.
//
// Subscribes before prompting, accumulates assistant text until
// session.idle, then posts **only the final assistant message's text**
// (chunked). Any error (a failed prompt, a broken stream, or session.error)
// is folded into that single reply so the user is never left with only the
// processing reaction and no answer. The returned error is non-nil only when
// posting the reply itself failed (the caller's error path then takes over)
// or the context was cancelled outright.
//
// Long turns are kept visible and bounded: after a short grace period one
// status message combines the latest todowrite checklist (or safe tool title),
// elapsed time, and action count. It refreshes on a fixed user-visible cadence
// independent of hidden backend activity. Timeout still aborts the session.
func StreamReply(ctx context.Context, p Params) error {
	// The caller's ctx is process-lifetime; the SSE subscription below must
	// die with the turn, or its reader goroutine and GET /event connection
	// leak on every completed turn.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	assistantMessageIDs := map[string]bool{}
	assistantOrder := map[string]int{}
	messageCounter := 0
	textParts := map[string]textPart{}
	order := 0
	errorText := ""
	timedOut := false
	interrupted := false
	sessionIdle := false
	var startedAt time.Time
	actions := map[string]bool{}
	var todos []opencode.Todo
	activity := ""
	statusVisible := false
	statusBlocked := false
	pendingQuestions := map[string]bool{}

	updateStatus := func(state string) {
		if p.Status == nil || statusBlocked || startedAt.IsZero() {
			return
		}
		text := renderRunStatus(state, time.Since(startedAt), len(actions), todos, activity)
		if err := p.Status.Update(ctx, text); err != nil {
			statusBlocked = true
			slog.Warn("run status update failed; disabling live updates", "error", err)
			return
		}
		statusVisible = true
	}
	finishStatus := func(verb, note string) {
		if p.Status == nil || !statusVisible || startedAt.IsZero() {
			return
		}
		text := renderReceipt(verb, time.Since(startedAt), len(actions))
		if note != "" {
			text += "\n\n" + note
		}
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer finishCancel()
		if err := p.Status.Finish(finishCtx, text); err != nil {
			slog.Warn("run status completion failed", "error", err)
		}
	}

	handleEvent := func(ev opencode.Event) (done bool) {
		switch {
		case ev.MessageUpdated != nil:
			info := ev.MessageUpdated.Info
			if info.SessionID == p.SessionID && info.Role == "assistant" {
				assistantMessageIDs[info.ID] = true
				if _, ok := assistantOrder[info.ID]; !ok {
					assistantOrder[info.ID] = messageCounter
					messageCounter++
				}
			}

		case ev.PartUpdated != nil:
			part := &ev.PartUpdated.Part
			if part.SessionID != p.SessionID {
				return false
			}
			if part.Type == "tool" {
				actionID := part.CallID
				if actionID == "" {
					actionID = part.ID
				}
				if actionID != "" {
					actions[actionID] = true
				}
				if part.Tool == "todowrite" {
					if latest, ok := part.TodoState(); ok {
						todos = latest
					}
					return false
				}
				_, title := part.ToolState()
				activity = sanitizeActivity(title, part.Tool)
			}
			if !isAnswerTextPart(part) || !assistantMessageIDs[part.MessageID] {
				return false
			}
			if existing, ok := textParts[part.ID]; ok {
				textParts[part.ID] = textPart{order: existing.order, text: part.Text, messageID: part.MessageID}
			} else {
				textParts[part.ID] = textPart{order: order, text: part.Text, messageID: part.MessageID}
				order++
			}

		case ev.SessionStatus != nil:
			status := ev.SessionStatus
			if status.SessionID == p.SessionID && status.Status.Type == "retry" {
				activity = "Retrying the model request"
			}

		case ev.SessionError != nil:
			if ev.SessionError.SessionID != p.SessionID {
				return false
			}
			errorText = describeError(ev.SessionError.Error)
			return true

		case ev.SessionIdle != nil:
			if ev.SessionIdle.SessionID != p.SessionID {
				return false
			}
			sessionIdle = true
			return true
		}
		return false
	}

	// Subscribe *before* prompting so no early deltas are missed.
	stream, err := p.OpenCode.Subscribe(ctx, p.Directory)
	if err != nil {
		errorText = errString(err)
	} else if err := p.OpenCode.PromptAsync(ctx, p.PromptParams()); err != nil {
		slog.Error("streaming the reply failed", "error", err)
		errorText = errString(err)
	} else {
		if p.StreamReady != nil {
			p.StreamReady()
		}
		startedAt = time.Now()
		activeStarted := startedAt
		var activeElapsed time.Duration
		questionTimeout := effectiveQuestionTimeout(p.QuestionTimeout)
		statusDelay := effectiveStatusDelay(p.StatusDelay)
		statusInterval := effectiveStatusInterval(p.StatusInterval, p.Timeout, statusDelay)

		var statusTimer, turnTimer, questionTimer *time.Timer
		if p.Status != nil {
			statusTimer = time.NewTimer(statusDelay)
		}
		if p.Timeout > 0 {
			turnTimer = time.NewTimer(p.Timeout)
		}
		defer func() {
			stopTimer(statusTimer)
			stopTimer(turnTimer)
			stopTimer(questionTimer)
		}()

		resume := func(now time.Time) {
			activeStarted = now
			if p.Timeout > 0 {
				turnTimer = resetTimer(turnTimer, p.Timeout-activeElapsed)
			}
			if p.Status != nil {
				if statusVisible {
					updateStatus("Working")
					statusTimer = resetTimer(statusTimer, statusInterval)
				} else {
					statusTimer = resetTimer(statusTimer, statusDelay-activeElapsed)
				}
			}
		}
		resolve := func(requestID string, now time.Time) {
			if !pendingQuestions[requestID] {
				return
			}
			delete(pendingQuestions, requestID)
			if len(pendingQuestions) == 0 {
				stopTimer(questionTimer)
				questionTimer = nil
				resume(now)
			}
		}

	consume:
		for {
			select {
			case ev, ok := <-stream.Events():
				if !ok {
					if streamErr := stream.Err(); streamErr != nil {
						slog.Error("streaming the reply failed", "error", streamErr)
						errorText = errString(streamErr)
					} else if !sessionIdle {
						interrupted = true
						errorText = "The event stream ended before the turn completed."
					}
					break consume
				}

				switch {
				case ev.QuestionAsked != nil:
					req := ev.QuestionAsked
					if req.SessionID != p.SessionID || p.AskQuestion == nil {
						continue
					}
					if err := p.AskQuestion(ctx, req); err != nil {
						slog.Warn("failed to present the agent's question", "request", req.ID, "error", err)
						continue
					}
					if len(pendingQuestions) == 0 {
						now := time.Now()
						activeElapsed += now.Sub(activeStarted)
						stopTimer(statusTimer)
						statusTimer = nil
						stopTimer(turnTimer)
						turnTimer = nil
						questionTimer = resetTimer(questionTimer, questionTimeout)
						if statusVisible {
							updateStatus("Waiting for your input")
						}
					}
					pendingQuestions[req.ID] = true
					continue

				case ev.QuestionReplied != nil:
					resolve(ev.QuestionReplied.RequestID, time.Now())
					continue

				case ev.QuestionRejected != nil:
					resolve(ev.QuestionRejected.RequestID, time.Now())
					continue
				}

				if handleEvent(ev) {
					break consume
				}

			case <-timerChan(statusTimer):
				updateStatus("Working")
				statusTimer = resetTimer(statusTimer, statusInterval)

			case <-timerChan(turnTimer):
				timedOut = true
				if err := p.OpenCode.AbortSession(ctx, p.SessionID, p.Directory); err != nil {
					slog.Warn("failed to abort timed-out session", "session", p.SessionID, "error", err)
				}
				break consume

			case now := <-timerChan(questionTimer):
				slog.Warn("question timed out unanswered", "session", p.SessionID, "after", questionTimeout)
				if p.ExpireQuestions != nil {
					p.ExpireQuestions(ctx)
				}
				clear(pendingQuestions)
				questionTimer = nil
				resume(now)

			case <-ctx.Done():
				finishStatus("Interrupted", "")
				return ctx.Err()
			}
		}
	}

	if timedOut && (errorText == "" || errorText == cancelledNotice) {
		errorText = fmt.Sprintf("⏱️ %d minutes. That's my limit. Resend in a new thread.",
			int(p.Timeout.Minutes()))
	}

	body := finalMessageText(textParts, assistantOrder)
	if errorText != "" {
		prefix := ""
		if strings.TrimSpace(body) != "" {
			prefix = body + "\n\n"
		}
		marker := "⚠️ "
		if errorText == cancelledNotice {
			marker = ""
		}
		body = prefix + marker + errorText
	}

	verb := "Completed"
	switch {
	case timedOut:
		verb = "Timed out"
	case errorText == cancelledNotice:
		verb = "Stopped"
	case interrupted:
		verb = "Interrupted"
	case errorText != "":
		verb = "Failed"
	}

	chunks := ChunkText(body, MaxReplyBytes)
	if len(chunks) == 0 {
		chunks = []string{emptyFallback}
	}
	for _, chunk := range chunks {
		if err := p.Send(ctx, chunk); err != nil {
			note := ""
			if verb == "Completed" {
				note = "⚠️ The final reply could not be delivered."
			}
			finishStatus(verb, note)
			return err
		}
	}
	finishStatus(verb, "")
	return nil
}

func renderReceipt(verb string, elapsed time.Duration, actions int) string {
	preposition := "after"
	if verb == "Completed" {
		preposition = "in"
	}
	return fmt.Sprintf("**%s %s %s** · %s.", verb, preposition, formatDuration(elapsed), formatActions(actions))
}

func formatDuration(elapsed time.Duration) string {
	elapsed = max(elapsed, 0).Truncate(time.Second)
	hours := int(elapsed / time.Hour)
	minutes := int(elapsed/time.Minute) % 60
	seconds := int(elapsed/time.Second) % 60

	var duration string
	switch {
	case hours > 0:
		duration = fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	case minutes > 0:
		duration = fmt.Sprintf("%dm %ds", minutes, seconds)
	default:
		duration = fmt.Sprintf("%ds", seconds)
	}
	return duration
}

func formatActions(actions int) string {
	label := "actions"
	if actions == 1 {
		label = "action"
	}
	return fmt.Sprintf("%d %s", actions, label)
}

func effectiveStatusDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return defaultStatusDelay
	}
	return delay
}

func effectiveStatusInterval(interval, timeout, delay time.Duration) time.Duration {
	if interval <= 0 {
		interval = defaultStatusInterval
	}
	if timeout <= time.Hour || timeout <= delay {
		return interval
	}
	// There may be one initial post plus LiveStatusEditBudget periodic edits.
	// Stretch turns beyond an hour so refreshes span the configured lifetime.
	minimum := (timeout - delay + LiveStatusEditBudget) / (LiveStatusEditBudget + 1)
	return max(interval, minimum)
}

func timerChan(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) *time.Timer {
	stopTimer(timer)
	return time.NewTimer(max(duration, 10*time.Millisecond))
}

// errString mirrors Python's `str(exc) or exc.__class__.__name__` — never an
// empty error line.
func errString(err error) string {
	if s := err.Error(); s != "" {
		return s
	}
	return fmt.Sprintf("%T", err)
}
