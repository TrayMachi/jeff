package gateway

import (
	"context"
	"sync"
	"testing"

	"github.com/local/jeff/internal/opencode"
)

type promptRecorder struct {
	mu      sync.Mutex
	prompts []opencode.PromptParams
}

func (r *promptRecorder) PromptAsync(_ context.Context, p opencode.PromptParams) error {
	r.mu.Lock()
	r.prompts = append(r.prompts, p)
	r.mu.Unlock()
	return nil
}

func TestSubmitSendsEveryFollowerWithoutAnotherPresenter(t *testing.T) {
	recorder := &promptRecorder{}
	g := New(recorder)
	presentationStarted := make(chan struct{})
	finishPresentation := make(chan struct{})
	leaderDone := make(chan error, 1)

	go func() {
		leaderDone <- g.Submit(t.Context(), "om_root", opencode.PromptParams{Text: "first"},
			func(_ context.Context, ready func()) error {
				close(presentationStarted)
				ready()
				<-finishPresentation
				return nil
			})
	}()
	<-presentationStarted

	const followers = 8
	var wg sync.WaitGroup
	for i := range followers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.Submit(t.Context(), "om_root", opencode.PromptParams{Text: string(rune('a' + i))},
				func(context.Context, func()) error {
					t.Error("follower unexpectedly became presenter")
					return nil
				})
			if err != nil {
				t.Errorf("Submit() error = %v", err)
			}
		}()
	}
	wg.Wait()

	recorder.mu.Lock()
	got := len(recorder.prompts)
	recorder.mu.Unlock()
	if got != followers {
		t.Fatalf("prompt_async calls = %d, want %d", got, followers)
	}
	close(finishPresentation)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
}

func TestSubmitWaitsForPresenterReadiness(t *testing.T) {
	recorder := &promptRecorder{}
	g := New(recorder)
	presentationStarted := make(chan struct{})
	markReady := make(chan struct{})
	finishPresentation := make(chan struct{})
	leaderDone := make(chan error, 1)

	go func() {
		leaderDone <- g.Submit(t.Context(), "om_root", opencode.PromptParams{},
			func(_ context.Context, ready func()) error {
				close(presentationStarted)
				<-markReady
				ready()
				<-finishPresentation
				return nil
			})
	}()
	<-presentationStarted

	followerDone := make(chan error, 1)
	go func() {
		followerDone <- g.Submit(t.Context(), "om_root", opencode.PromptParams{Text: "follow-up"}, nil)
	}()

	recorder.mu.Lock()
	got := len(recorder.prompts)
	recorder.mu.Unlock()
	if got != 0 {
		t.Fatalf("prompt_async called before presenter ready")
	}
	close(markReady)
	if err := <-followerDone; err != nil {
		t.Fatal(err)
	}
	close(finishPresentation)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
}
