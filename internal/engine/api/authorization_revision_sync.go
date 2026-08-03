package api

import (
	"context"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

// authorizationRevisionSyncHandler holds a mutation response until the local
// authorization cache has observed any binding committed by that mutation.
// This prevents a CLI apply followed immediately by list/download from using
// the caller's pre-create permission snapshot.
func authorizationRevisionSyncHandler(loader accesscontrol.AuthorizationRevisionLoader, sink authorizationRevisionSink, next http.HandlerFunc) http.HandlerFunc {
	if loader == nil || sink == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		bw := newBufferedResponseWriter()
		next(bw, r)
		syncAuthorizationRevision(r.Context(), loader, sink)
		bw.flushTo(w)
	}
}

func syncAuthorizationRevision(ctx context.Context, loader accesscontrol.AuthorizationRevisionLoader, sink authorizationRevisionSink) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.authorization.revision_sync")
	defer span.End()
	revision, err := loader.LoadAuthorizationRevision(ctx)
	if err != nil {
		// The mutation may already be committed, so preserve its response and let
		// the bounded revision poll recover instead of reporting a false failure.
		span.RecordError(err)
		span.SetStatus(codes.Error, "authorization revision sync failed")
		slog.ErrorContext(ctx, "failed to synchronize authorization revision after mutation", slog.Any("error", err))
		return
	}
	if sink.SetRevision(revision) {
		span.AddEvent("engine.authorization.cache_invalidated")
	}
}
