package accesscontrol

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace"
)

type AuthorizationRevisionLoader interface {
	LoadAuthorizationRevision(ctx context.Context) (int64, error)
}

// RefreshAuthorizationRevision invalidates cached identities and snapshots
// when another writer has committed newer access state.
func RefreshAuthorizationRevision(ctx context.Context, loader AuthorizationRevisionLoader, authenticator *Authenticator) (bool, error) {
	if loader == nil || authenticator == nil {
		return false, fmt.Errorf("invalid authorization revision monitor configuration")
	}
	current := authenticator.CurrentRevision()
	started := time.Now()
	revision, err := loader.LoadAuthorizationRevision(ctx)
	recordAuthorizationQuery(ctx, "load_revision", started)
	if err != nil {
		return false, err
	}
	recordRevisionLag(ctx, revision, current)
	changed := authenticator.SetRevision(revision)
	if changed {
		trace.SpanFromContext(ctx).AddEvent("engine.authorization.cache_invalidated")
	}
	return changed, nil
}

// PollAuthorizationRevisions is the missed-notification recovery path. Access
// mutations can invalidate immediately; this bounded poll makes cache safety
// independent of successful message delivery.
func PollAuthorizationRevisions(ctx context.Context, loader AuthorizationRevisionLoader, authenticator *Authenticator, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := RefreshAuthorizationRevision(ctx, loader, authenticator)
			if err != nil && onError != nil {
				onError(err)
			}
		}
	}
}
