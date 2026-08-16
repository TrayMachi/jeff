package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/local/jeff/internal/contexts"
	"github.com/local/jeff/internal/conversation"
	"github.com/local/jeff/internal/store"
)

var ErrUnknownProject = errors.New("unknown project")

type SessionAPI interface {
	CreateSession(context.Context, string) (string, error)
	SessionExists(context.Context, string, string) (bool, error)
}
type Resolved struct {
	SessionID, Directory, Provider, Model, Project string
	Effort                                         string
	IsNew                                          bool
	SwitchBlocked                                  string
}
type Resolver struct {
	store    *store.Store
	opencode SessionAPI
	projects *contexts.ContextsConfig
	mu       sync.Mutex
	locks    map[string]*threadLock
}
type threadLock struct {
	mu   sync.Mutex
	refs int
}

func NewResolver(s *store.Store, oc SessionAPI, projects *contexts.ContextsConfig) *Resolver {
	return &Resolver{store: s, opencode: oc, projects: projects, locks: map[string]*threadLock{}}
}
func (r *Resolver) Resolve(ctx context.Context, key conversation.Key, requested string) (*Resolved, error) {
	unlock := r.lock(key.String())
	defer unlock()
	if requested != "" && !r.projects.Has(requested) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProject, requested)
	}
	row, err := r.store.GetRow(ctx, key)
	if err != nil {
		return nil, err
	}
	project := requested
	isNew := false
	blocked := ""
	if row != nil {
		project = row.Project
		if requested != "" && requested != project && r.projects.Has(requested) {
			blocked = requested
		}
	}
	if project == "" {
		project = r.projects.DefaultContext
	}
	cc, ok := r.projects.Contexts[project]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProject, project)
	}
	sessionID := ""
	if row == nil {
		sessionID, err = r.opencode.CreateSession(ctx, cc.Directory)
		if err != nil {
			return nil, err
		}
		if err = r.store.Set(ctx, key, sessionID, project, cc.Directory, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
		isNew = true
	} else {
		if row.Directory != cc.Directory {
			return nil, fmt.Errorf("stored project directory mismatch for %s", project)
		}
		exists, checkErr := r.opencode.SessionExists(ctx, row.SessionID, cc.Directory)
		if checkErr != nil {
			return nil, checkErr
		}
		sessionID = row.SessionID
		if !exists {
			sessionID, err = r.opencode.CreateSession(ctx, cc.Directory)
			if err != nil {
				return nil, err
			}
			if err = r.store.Set(ctx, key, sessionID, project, cc.Directory, time.Now().UnixMilli()); err != nil {
				return nil, err
			}
		}
	}
	provider, model := cc.ProviderModel()
	return &Resolved{SessionID: sessionID, Directory: cc.Directory, Provider: provider, Model: model, Effort: cc.Effort, Project: project, IsNew: isNew, SwitchBlocked: blocked}, nil
}
func (r *Resolver) lock(id string) func() {
	r.mu.Lock()
	l := r.locks[id]
	if l == nil {
		l = &threadLock{}
		r.locks[id] = l
	}
	l.refs++
	r.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		r.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(r.locks, id)
		}
		r.mu.Unlock()
	}
}
