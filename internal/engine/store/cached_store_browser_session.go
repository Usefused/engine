package store

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *cachedStore) browserSessionStore() (BrowserSessionStore, error) {
	repository, ok := s.Store.(BrowserSessionStore)
	if !ok {
		return nil, ErrManagedLoginUnavailable
	}
	return repository, nil
}

func (s *cachedStore) IssueBrowserSession(ctx context.Context, actor accesscontrol.Actor, authMethod string, expiresAt time.Time) (BrowserSessionCredential, error) {
	repository, err := s.browserSessionStore()
	if err != nil {
		return BrowserSessionCredential{}, err
	}
	return repository.IssueBrowserSession(ctx, actor, authMethod, expiresAt)
}

func (s *cachedStore) RevokeBrowserSession(ctx context.Context, actor accesscontrol.Actor, at time.Time) (BrowserLogoutContext, error) {
	repository, err := s.browserSessionStore()
	if err != nil {
		return BrowserLogoutContext{}, err
	}
	return repository.RevokeBrowserSession(ctx, actor, at)
}

var _ BrowserSessionStore = (*cachedStore)(nil)
