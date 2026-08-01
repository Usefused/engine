package store

import (
	"context"
	"fmt"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *cachedStore) ReconcileBootstrapOwner(ctx context.Context, input accesscontrol.BootstrapInput) (accesscontrol.BootstrapResult, error) {
	repository, ok := s.Store.(accesscontrol.BootstrapRepository)
	if !ok {
		return accesscontrol.BootstrapResult{}, fmt.Errorf("store does not support access-control bootstrap")
	}
	return repository.ReconcileBootstrapOwner(ctx, input)
}

func (s *cachedStore) LoadControlPrincipal(ctx context.Context, credentialHash string) (accesscontrol.ControlPrincipal, error) {
	loader, ok := s.Store.(accesscontrol.PrincipalLoader)
	if !ok {
		return accesscontrol.ControlPrincipal{}, fmt.Errorf("store does not support control authentication")
	}
	return loader.LoadControlPrincipal(ctx, credentialHash)
}

func (s *cachedStore) LoadAuthorizationRevision(ctx context.Context) (int64, error) {
	loader, ok := s.Store.(accesscontrol.AuthorizationRevisionLoader)
	if !ok {
		return 0, fmt.Errorf("store does not support authorization revision loading")
	}
	return loader.LoadAuthorizationRevision(ctx)
}

func (s *cachedStore) RecordAuthorizationAudit(ctx context.Context, event accesscontrol.AuditEvent) error {
	recorder, ok := s.Store.(accesscontrol.AuditRecorder)
	if !ok {
		return fmt.Errorf("store does not support authorization auditing")
	}
	return recorder.RecordAuthorizationAudit(ctx, event)
}

func (s *cachedStore) ResolveAuthorizationResourceDisplayNames(ctx context.Context, resources []accesscontrol.ResourceRef) (map[accesscontrol.ResourceRef]string, error) {
	resolver, ok := s.Store.(interface {
		ResolveAuthorizationResourceDisplayNames(context.Context, []accesscontrol.ResourceRef) (map[accesscontrol.ResourceRef]string, error)
	})
	if !ok {
		return nil, fmt.Errorf("store does not support authorization display names")
	}
	return resolver.ResolveAuthorizationResourceDisplayNames(ctx, resources)
}
