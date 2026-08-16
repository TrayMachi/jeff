// Package gateway coordinates presentation of OpenCode session runs.
// It never queues prompts: every follower is submitted immediately through
// prompt_async while one caller watches and presents the shared session run.
package gateway

import (
	"context"
	"sync"

	"github.com/local/jeff/internal/opencode"
)

// PromptSender submits an asynchronous OpenCode prompt.
type PromptSender interface {
	PromptAsync(context.Context, opencode.PromptParams) error
}

// Presenter watches and presents one OpenCode session run. It must call ready
// after its event subscription is established and the initial prompt is
// accepted. Followers wait for that signal before submitting their prompts.
type Presenter func(ctx context.Context, ready func()) error

// Gateway keeps one presentation watcher per conversation while leaving prompt
// admission and execution to OpenCode.
type Gateway struct {
	prompts PromptSender

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	refs   int
	active *run
}

type run struct {
	ready chan struct{}
	done  chan struct{}
}

// New builds a Gateway using prompts for follower submissions.
func New(prompts PromptSender) *Gateway {
	return &Gateway{prompts: prompts, entries: make(map[string]*entry)}
}

// Submit presents a new run or submits prompt_async into the existing run.
// It does not serialize or retain follower prompts in the gateway.
func (g *Gateway) Submit(ctx context.Context, threadRootID string, prompt opencode.PromptParams, present Presenter) error {
	e, release := g.acquire(threadRootID)
	defer release()

	for {
		r, leader := g.claim(e)
		if leader {
			var once sync.Once
			err := present(ctx, func() { once.Do(func() { close(r.ready) }) })
			g.finish(e, r)
			close(r.done)
			return err
		}

		select {
		case <-r.ready:
			return g.prompts.PromptAsync(ctx, prompt)
		case <-r.done:
			// The previous presentation ended before this prompt could be
			// admitted. Become the presenter for the next OpenCode run.
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *Gateway) acquire(threadRootID string) (*entry, func()) {
	g.mu.Lock()
	e := g.entries[threadRootID]
	if e == nil {
		e = &entry{}
		g.entries[threadRootID] = e
	}
	e.refs++
	g.mu.Unlock()

	return e, func() {
		g.mu.Lock()
		e.refs--
		if e.refs == 0 && e.active == nil {
			delete(g.entries, threadRootID)
		}
		g.mu.Unlock()
	}
}

func (g *Gateway) claim(e *entry) (*run, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e.active != nil {
		return e.active, false
	}
	r := &run{ready: make(chan struct{}), done: make(chan struct{})}
	e.active = r
	return r, true
}

func (g *Gateway) finish(e *entry, r *run) {
	g.mu.Lock()
	if e.active == r {
		e.active = nil
	}
	g.mu.Unlock()
}
