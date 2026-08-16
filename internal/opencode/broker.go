package opencode

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// subscriberBuffer is the per-turn fan-out channel depth. The streamer drains
// events in a tight select loop; its only stalls are brief Lark progress
// posts, so a deep buffer absorbs a burst without ever blocking the shared
// upstream reader. A subscriber that somehow overruns it is detached (see
// ErrSlowConsumer) rather than allowed to stall every other turn.
const subscriberBuffer = 1024

// ErrSlowConsumer detaches a turn that cannot keep up with the shared stream.
// It must never block the upstream reader, so an overrun ends that one turn
// (it falls back to its own error reply) instead of wedging the directory.
var ErrSlowConsumer = errors.New("event subscriber overflowed the shared stream buffer")

// EventStream multiplexes opencode's directory-scoped /event SSE stream so the
// bot holds at most one upstream connection per workspace directory regardless
// of how many turns run concurrently.
//
// opencode leaks an event-bus listener when a /event connection closes, so the
// old one-subscription-per-turn pattern leaked a listener per turn until the
// bus stopped delivering session.idle and every turn hung. One shared upstream
// per directory bounds the listener count to the number of active directories.
//
// It implements the streamer's OpenCodeClient seam: Subscribe is multiplexed;
// Prompt and AbortSession pass through to the wrapped client.
type EventStream struct {
	client *Client
	base   context.Context //nolint:containedctx // the process ctx; bounds every shared upstream so shutdown closes them all

	mu   sync.Mutex
	hubs map[string]*eventHub // one per directory; absent once its upstream dies
}

// NewEventStream wraps c so per-turn Subscribe calls share one upstream
// connection per directory. base bounds those upstreams — cancelling it (at
// shutdown) closes every shared stream.
func NewEventStream(base context.Context, c *Client) *EventStream {
	return &EventStream{client: c, base: base, hubs: make(map[string]*eventHub)}
}

// eventHub fans one directory's upstream /event stream out to the live turns
// subscribed to it.
type eventHub struct {
	directory string             // the worktree this upstream is scoped to (for logs)
	cancel    context.CancelFunc // tears the upstream reader down
	ready     chan struct{}      // closed once the upstream open attempt resolves
	openErr   error              // set before ready closes; nil once established

	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool  // upstream ended (failed to open, or died after opening)
	death  error // terminal error handed to late/!ok subscribers
}

// PromptAsync passes through to the wrapped opencode client.
func (es *EventStream) PromptAsync(ctx context.Context, p PromptParams) error {
	return es.client.PromptAsync(ctx, p)
}

// AbortSession passes through to the wrapped opencode client.
func (es *EventStream) AbortSession(ctx context.Context, sessionID, directory string) error {
	return es.client.AbortSession(ctx, sessionID, directory)
}

// subscriber is one turn's view of the shared stream.
type subscriber struct {
	ch       chan Event
	terminal error
}

// Subscribe attaches one turn to its directory's shared upstream, opening that
// upstream on the first subscriber. The returned Stream behaves exactly like a
// dedicated Client.Subscribe: Events carries the directory's events (the caller
// still filters by sessionID) and is closed with Err() set when the turn's ctx
// ends or the upstream dies.
func (es *EventStream) Subscribe(ctx context.Context, directory string) (*Stream, error) {
	hub, err := es.hubFor(ctx, directory)
	if err != nil {
		return nil, err
	}

	sub := &subscriber{ch: make(chan Event, subscriberBuffer)}
	hub.mu.Lock()
	if hub.closed {
		// The upstream died between hubFor and here. Hand back a finished stream
		// carrying its terminal error (nil for a clean exit); the next
		// subscriber rebuilds a fresh hub.
		death := hub.death
		hub.mu.Unlock()
		if death != nil {
			sub.terminal = death
		}
		close(sub.ch)
		return NewStream(sub.ch, func() error { return sub.terminal }), nil
	}
	hub.subs[sub] = struct{}{}
	hub.mu.Unlock()

	// Detach when the turn ends; the shared upstream stays open for the
	// directory's other and future turns.
	go func() {
		<-ctx.Done()
		hub.detach(sub, ctx.Err())
	}()
	return NewStream(sub.ch, func() error { return sub.terminal }), nil
}

// hubFor returns the directory's hub, opening its upstream once. The opener
// does the network call without holding es.mu, so a slow or wedged directory
// cannot block subscribes to other directories.
func (es *EventStream) hubFor(ctx context.Context, directory string) (*eventHub, error) {
	es.mu.Lock()
	hub := es.hubs[directory]
	firstSubscriber := hub == nil
	if firstSubscriber {
		hub = &eventHub{directory: directory, ready: make(chan struct{}), subs: make(map[*subscriber]struct{})}
		es.hubs[directory] = hub
	}
	es.mu.Unlock()

	if firstSubscriber {
		// Bind the upstream to base, not this turn's ctx, or the first turn to
		// finish would close the shared stream out from under the others.
		upCtx, cancel := context.WithCancel(es.base)
		//nolint:contextcheck // upstream is process-lifetime by design; see above.
		up, err := es.client.Subscribe(upCtx, directory)
		hub.cancel = cancel
		if err != nil {
			cancel()
			hub.mu.Lock()
			hub.closed, hub.death = true, err
			hub.mu.Unlock()
			es.drop(directory, hub)
			hub.openErr = err // waiters read it once ready closes
			close(hub.ready)
			return nil, err
		}
		close(hub.ready)
		go es.fanout(directory, hub, up)
		return hub, nil
	}

	// Not the opener: wait for its result (or give up if this turn is cancelled).
	select {
	case <-hub.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if hub.openErr != nil {
		return nil, hub.openErr
	}
	return hub, nil
}

// fanout copies every upstream event to all live subscribers, then — when the
// upstream ends — fails them all the same way a dropped per-turn connection
// did and drops the hub so the next turn opens a fresh upstream.
func (es *EventStream) fanout(directory string, hub *eventHub, up *Stream) {
	for ev := range up.Events() {
		hub.deliver(ev)
	}
	es.drop(directory, hub)
	hub.closeAll(up.Err())
	hub.cancel()
}

// drop removes the hub from the directory map if it is still the current one,
// so a later Subscribe rebuilds a fresh upstream instead of reusing a dead hub.
func (es *EventStream) drop(directory string, hub *eventHub) {
	es.mu.Lock()
	if es.hubs[directory] == hub {
		delete(es.hubs, directory)
	}
	es.mu.Unlock()
}

// deliver fans one event out under the hub lock with a non-blocking send: a
// subscriber that has overrun its buffer is detached rather than allowed to
// block the shared reader (which would re-create a per-directory wedge).
func (h *eventHub) deliver(ev Event) {
	h.mu.Lock()
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			// A turn fell subscriberBuffer events behind; detach it rather than
			// block the shared reader. Should never fire in normal operation.
			slog.Warn("detaching a slow event subscriber from the shared stream",
				"directory", h.directory, "buffer", subscriberBuffer)
			sub.terminal = ErrSlowConsumer
			close(sub.ch)
			delete(h.subs, sub)
		}
	}
	h.mu.Unlock()
}

// detach removes one subscriber (its turn ended) and closes its channel. The
// guard makes it a no-op if closeAll/deliver already removed the subscriber, so
// the channel is never closed twice.
func (h *eventHub) detach(sub *subscriber, err error) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; ok {
		sub.terminal = err
		close(sub.ch)
		delete(h.subs, sub)
	}
	h.mu.Unlock()
}

// closeAll ends every remaining subscriber with the upstream's terminal error
// and marks the hub dead so a Subscribe that raced the death resolves cleanly.
func (h *eventHub) closeAll(err error) {
	h.mu.Lock()
	h.closed, h.death = true, err
	for sub := range h.subs {
		sub.terminal = err
		close(sub.ch)
		delete(h.subs, sub)
	}
	h.mu.Unlock()
}
