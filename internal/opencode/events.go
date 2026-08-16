// SSE event decoding: hand-written minimal structs, not the official Go SDK
// and not codegen — the streamer reads a handful of fields off 8 event types
// and deliberately ignores everything else (forward-compatible by construction).
// Safe because the server is version-pinned (deploy/opencode.version,
// asserted at boot) and the consumed API slice is snapshot in
// deploy/opencode-api-snapshot.json, diffed on every bump by the
// upgrade-opencode skill. Revisit codegen only if the consumed surface grows
// past ~10 types.
package opencode

import (
	"encoding/json"
	"fmt"
)

// Event is one decoded SSE event: exactly one of the typed pointers is set.
type Event struct {
	MessageUpdated   *MessageUpdated
	PartUpdated      *PartUpdated
	SessionStatus    *SessionStatus
	SessionError     *SessionError
	SessionIdle      *SessionIdle
	QuestionAsked    *QuestionRequest
	QuestionReplied  *QuestionReplied
	QuestionRejected *QuestionRejected
}

// MessageUpdated is a message.updated event's properties.
type MessageUpdated struct {
	Info MessageInfo `json:"info"`
}

// MessageInfo identifies the message a message.updated event describes.
type MessageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
}

// PartUpdated is a message.part.updated event's properties.
type PartUpdated struct {
	Part Part `json:"part"`
}

// Part is one message part delta. State stays raw because tool state is an
// open record whose relevant fields depend on the tool.
type Part struct {
	ID        string          `json:"id"`
	CallID    string          `json:"callID"`
	MessageID string          `json:"messageID"`
	SessionID string          `json:"sessionID"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Tool      string          `json:"tool"`
	Ignored   bool            `json:"ignored"`
	State     json.RawMessage `json:"state"`
}

// Todo is one item of a todowrite tool call's input.
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// TodoState extracts a todowrite part's todo list from its state. ok
// distinguishes an explicitly empty list (which clears prior progress) from
// an absent or malformed state.
func (p *Part) TodoState() (todos []Todo, ok bool) {
	var state struct {
		Input struct {
			Todos *[]Todo `json:"todos"`
		} `json:"input"`
	}
	if len(p.State) == 0 || json.Unmarshal(p.State, &state) != nil {
		return nil, false
	}
	if state.Input.Todos == nil {
		return nil, false
	}
	return *state.Input.Todos, true
}

// Todos returns the current todowrite list, or nil for absent and malformed
// state. Call TodoState when an explicitly empty list matters.
func (p *Part) Todos() []Todo {
	todos, _ := p.TodoState()
	return todos
}

// ToolState returns the safe display fields from a tool part's open state.
func (p *Part) ToolState() (status, title string) {
	var state struct {
		Status string `json:"status"`
		Title  string `json:"title"`
	}
	if len(p.State) == 0 || json.Unmarshal(p.State, &state) != nil {
		return "", ""
	}
	return state.Status, state.Title
}

// SessionStatus is a session.status event's properties. The streamer only
// acts on Status.Type == "retry" — published before each provider-retry
// sleep.
type SessionStatus struct {
	SessionID string `json:"sessionID"`
	Status    struct {
		Type    string `json:"type"`
		Attempt int    `json:"attempt"`
		Message string `json:"message"`
	} `json:"status"`
}

// SessionError is a session.error event's properties.
type SessionError struct {
	SessionID string        `json:"sessionID"`
	Error     *ErrorPayload `json:"error"`
}

// ErrorPayload is an opencode session error body.
type ErrorPayload struct {
	Name string `json:"name"`
	Data struct {
		Message string `json:"message"`
	} `json:"data"`
}

// SessionIdle is a session.idle event's properties — the turn-complete
// signal.
type SessionIdle struct {
	SessionID string `json:"sessionID"`
}

// QuestionRequest is a question.asked event's properties: the agent asked the
// user one or more questions and its turn is blocked server-side until
// ReplyQuestion or RejectQuestion.
type QuestionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Questions []QuestionInfo `json:"questions"`
}

// QuestionInfo is one question of a request.
type QuestionInfo struct {
	Question string           `json:"question"`
	Header   string           `json:"header"` // short label, ≤30 chars
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple"` // multi-select
	// Custom is a *pointer* because absence and false differ: opencode's
	// contract is "allow a custom answer (default: true)" — the question
	// tool's input schema doesn't even carry the field (the model can't set
	// it), so it is absent on every model-asked question and false only when
	// opencode internals say so (e.g. an internal Yes/No confirmation). Read it through
	// AllowsCustom.
	Custom *bool `json:"custom"`
}

// AllowsCustom reports whether the question accepts a typed free-text answer:
// true unless opencode explicitly said custom: false, matching opencode's own
// client (`custom !== false`).
func (q *QuestionInfo) AllowsCustom() bool {
	return q.Custom == nil || *q.Custom
}

// QuestionOption is one selectable choice.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// QuestionReplied is a question.replied event's properties — somebody (iu)
// answered the request and the agent's turn resumed.
type QuestionReplied struct {
	SessionID string `json:"sessionID"`
	RequestID string `json:"requestID"`
}

// QuestionRejected is a question.rejected event's properties — the request
// was rejected; the agent's question tool call fails and the turn resumes
// without an answer.
type QuestionRejected struct {
	SessionID string `json:"sessionID"`
	RequestID string `json:"requestID"`
}

// decodeEvent two-pass decodes one SSE data payload. Known reports whether iu
// consumes the event type; malformed consumed events return an error.
func decodeEvent(payload []byte) (event Event, known bool, err error) {
	var envelope struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Event{}, false, fmt.Errorf("decode event envelope: %w", err)
	}
	switch envelope.Type {
	case "message.updated":
		v := &MessageUpdated{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{MessageUpdated: v}, true, nil
	case "message.part.updated":
		v := &PartUpdated{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{PartUpdated: v}, true, nil
	case "session.status":
		v := &SessionStatus{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{SessionStatus: v}, true, nil
	case "session.error":
		v := &SessionError{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{SessionError: v}, true, nil
	case "session.idle":
		v := &SessionIdle{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{SessionIdle: v}, true, nil
	case "question.asked":
		v := &QuestionRequest{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{QuestionAsked: v}, true, nil
	case "question.replied":
		v := &QuestionReplied{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{QuestionReplied: v}, true, nil
	case "question.rejected":
		v := &QuestionRejected{}
		if err := json.Unmarshal(envelope.Properties, v); err != nil {
			return Event{}, true, err
		}
		return Event{QuestionRejected: v}, true, nil
	default:
		return Event{}, false, nil
	}
}
