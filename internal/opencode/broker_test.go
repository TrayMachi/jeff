package opencode

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// eventBackend is a fake opencode /event endpoint that counts how many SSE
// connections are open at once — the external proxy for opencode's event-bus
// listeners. Each subscription is one GET /event = one listener, so maxSeen is
// exactly the listener high-water mark. It holds every connection open (like a
// real, long-lived stream) until the request context ends, and can broadcast a
// scripted event to every live connection.
type eventBackend struct {
	active  atomic.Int64
	maxSeen atomic.Int64

	mu    sync.Mutex
	conns map[chan string]struct{} // live connections' write channels
}

func newEventBackend(t *testing.T) (*eventBackend, *Client) {
	t.Helper()
	b := &eventBackend{conns: map[chan string]struct{}{}}
	srv := httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(srv.Close)
	return b, NewClient(srv.URL, "opencode", "secret", nil)
}

func (b *eventBackend) serve(w http.ResponseWriter, r *http.Request) {
	n := b.active.Add(1)
	for { // bump the high-water mark
		m := b.maxSeen.Load()
		if n <= m || b.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	defer b.active.Add(-1)

	lines := make(chan string, 64)
	b.mu.Lock()
	b.conns[lines] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.conns, lines)
		b.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.(http.Flusher).Flush()
	for {
		select {
		case line := <-lines:
			_, _ = io.WriteString(w, line)
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// broadcast pushes one event to every currently-open connection.
func (b *eventBackend) broadcast(eventType string, properties map[string]any) {
	line := sseLine(eventType, properties)
	b.mu.Lock()
	for ch := range b.conns {
		ch <- line
	}
	b.mu.Unlock()
}

func waitFor(t *testing.T, want int64, get func() int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met: got %d, want %d", get(), want)
}

// TestSharedStreamReusesOneConnectionPerDirectory is the leak guard. K turns in
// the same workspace must share ONE upstream /event connection — i.e. one
// opencode bus listener — instead of one per turn. This is RED against the
// per-turn baseline (maxSeen == K) and GREEN once Subscribe multiplexes.
func TestSharedStreamReusesOneConnectionPerDirectory(t *testing.T) {
	backend, client := newEventBackend(t)
	es := NewEventStream(t.Context(), client)

	const K = 8
	var wg sync.WaitGroup
	for range K {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// t.Context() lives until the test ends, so each turn stays
			// subscribed (the wedged-turn shape from the incident).
			if _, err := es.Subscribe(t.Context(), "/work"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait() // all K turns have subscribed

	if got := backend.maxSeen.Load(); got != 1 {
		t.Fatalf("opencode saw %d concurrent /event connections for one directory, want 1 "+
			"(per-turn subscriptions leak one bus listener each)", got)
	}
}

// TestSharedStreamFansEventOutToEverySubscriber proves the multiplexer doesn't
// lose events: one upstream event reaches every turn subscribed to that
// directory (each still filters by sessionID downstream).
func TestSharedStreamFansEventOutToEverySubscriber(t *testing.T) {
	backend, client := newEventBackend(t)
	es := NewEventStream(t.Context(), client)

	const K = 5
	streams := make([]*Stream, K)
	for i := range K {
		s, err := es.Subscribe(t.Context(), "/work")
		if err != nil {
			t.Fatal(err)
		}
		streams[i] = s
	}
	waitFor(t, 1, backend.active.Load) // one shared upstream

	backend.broadcast("session.idle", map[string]any{"sessionID": "s1"})

	for i, s := range streams {
		select {
		case ev, ok := <-s.Events():
			if !ok || ev.SessionIdle == nil || ev.SessionIdle.SessionID != "s1" {
				t.Fatalf("subscriber %d got %+v (ok=%v), want session.idle for s1", i, ev, ok)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d never received the broadcast event", i)
		}
	}
}

// TestSeparateDirectoriesGetSeparateConnections keeps the load-bearing
// directory scoping: opencode only emits a worktree's events to a subscription
// opened with that directory, so distinct directories must each hold their own
// upstream.
func TestSeparateDirectoriesGetSeparateConnections(t *testing.T) {
	backend, client := newEventBackend(t)
	es := NewEventStream(t.Context(), client)

	for _, dir := range []string{"/work/a", "/work/b", "/work/a", "/work/b"} {
		if _, err := es.Subscribe(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 2, backend.active.Load)
	if got := backend.maxSeen.Load(); got != 2 {
		t.Fatalf("got %d connections for 2 directories, want 2", got)
	}
}

// TestSubscribeSurfacesUpstreamOpenFailure keeps the streamer's fail-fast
// contract: when the shared upstream can't be opened, Subscribe returns the
// error (so the turn surfaces it without prompting) rather than handing back a
// silent stream.
func TestSubscribeSurfacesUpstreamOpenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	es := NewEventStream(t.Context(), NewClient(srv.URL, "opencode", "secret", nil))

	if _, err := es.Subscribe(t.Context(), "/work"); err == nil {
		t.Fatal("Subscribe succeeded, want the upstream open failure surfaced")
	}
}

// TestUpstreamDeathEndsSubscriberAndRebuilds proves a dead shared upstream
// fails its in-flight turn exactly like a dropped per-turn connection (channel
// closed, Err set), and that the next turn rebuilds a fresh upstream instead of
// reusing the dead hub.
func TestUpstreamDeathEndsSubscriberAndRebuilds(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		if conns.Add(1) == 1 {
			// Kill the first upstream mid-stream: a transport error, not an EOF.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	es := NewEventStream(t.Context(), NewClient(srv.URL, "opencode", "secret", nil))

	s1, err := es.Subscribe(t.Context(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	for range s1.Events() { //nolint:revive // drain until the dead upstream closes us
	}
	if s1.Err() == nil {
		t.Error("Err = nil after the upstream died, want a transport error")
	}

	// Draining s1 to completion means fanout has dropped the dead hub, so a new
	// turn opens a second, healthy upstream.
	if _, err := es.Subscribe(t.Context(), "/work"); err != nil {
		t.Fatalf("Subscribe after upstream death = %v, want a fresh upstream", err)
	}
	waitFor(t, 2, conns.Load)
}

// TestHubDetachesOnlyTheOverrunningSubscriber proves deliver's safety valve:
// every send is non-blocking, so a subscriber that overruns subscriberBuffer is
// detached with ErrSlowConsumer (its channel closed, removed from the hub)
// while a subscriber that keeps draining is left attached and error-free.
// Because deliver never blocks, one wedged turn cannot stall the shared reader
// and re-create the per-directory wedge the broker exists to prevent.
func TestHubDetachesOnlyTheOverrunningSubscriber(t *testing.T) {
	h := &eventHub{directory: "/work", subs: map[*subscriber]struct{}{}}

	// steady is drained after every delivery, so its buffer never fills; stuck
	// is never drained, so it accumulates one event per delivery.
	steady := &subscriber{ch: make(chan Event, subscriberBuffer)}
	stuck := &subscriber{ch: make(chan Event, subscriberBuffer)}
	h.subs[steady] = struct{}{}
	h.subs[stuck] = struct{}{}

	// Fill stuck's buffer exactly to capacity — still no overrun.
	for range subscriberBuffer {
		h.deliver(Event{})
		<-steady.ch
	}
	if _, ok := h.subs[stuck]; !ok {
		t.Fatal("stuck subscriber detached before overrunning its buffer")
	}

	// One more event: stuck's non-blocking send fails and it is detached; steady
	// still has room and survives. deliver returning at all proves it never
	// blocked on the full subscriber.
	h.deliver(Event{})
	<-steady.ch

	if _, ok := h.subs[stuck]; ok {
		t.Error("stuck subscriber still attached after overrunning its buffer")
	}
	if !errors.Is(stuck.terminal, ErrSlowConsumer) {
		t.Errorf("stuck subscriber err = %v, want ErrSlowConsumer", stuck.terminal)
	}
	// The channel is closed but still holds its buffered events; ranging drains
	// them and then returns on the close (it would block forever if not closed).
	drained := 0
	for range stuck.ch {
		drained++
	}
	if drained != subscriberBuffer {
		t.Errorf("stuck buffered %d events before detach, want %d", drained, subscriberBuffer)
	}
	if _, ok := h.subs[steady]; !ok {
		t.Error("steady subscriber was wrongly detached")
	}
	if steady.terminal != nil {
		t.Errorf("steady subscriber err = %v, want nil", steady.terminal)
	}
}
