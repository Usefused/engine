package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	enginemiddleware "github.com/Usefused/engine/internal/engine/middleware"
	"github.com/Usefused/engine/internal/engine/store"
)

// RESTProxyMountPaths is the single source of truth for which REST prefixes
// main.go proxies to the Registry. Defined here (not duplicated in main.go)
// so the route-wiring code and this package's own tests can't drift apart.
//
// Why /workspace is deliberately absent: workspace membership/settings live
// in the Engine's own DB (workspace_handlers.go), a different plane than
// everything else in this list, which is Registry-owned catalogue/account/
// SDK/billing data. Proxying /workspace here would silently serve Registry
// data (or a 404) instead of the Engine's own -- so it's excluded at the
// route list level, not just by omission in main.go.
var RESTProxyMountPaths = []string{
	"/integrations",
	"/account",
	"/sdks",
	"/leads",
	"/credits",
}

// RESTProxyHandler validates the caller's API key the same way
// GraphQLProxyHandler does, then forwards through the licence-identity proxy.
// Mounted at each path in RESTProxyMountPaths.
func RESTProxyHandler(proxy Forwarder, s store.Store, enforcers ...*enginemiddleware.RuntimeEnforcer) http.HandlerFunc {
	return RESTProxyHandlerWithRuntimeContracts(proxy, s, nil, enforcers...)
}

func RESTProxyHandlerWithRuntimeContracts(proxy Forwarder, s store.Store, contractFetcher RuntimeContractFetcher, enforcers ...*enginemiddleware.RuntimeEnforcer) http.HandlerFunc {
	enforcer := firstRuntimeEnforcer(enforcers)
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, err := controlActorAccount(r.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.WarnContext(r.Context(), "RESTProxyHandler: rejected request with invalid API key", slog.Any("error", err), slog.String("path", r.URL.Path))
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}

		if !isMutatingMethod(r.Method) {
			// GET is not traced at this layer -- same rationale as GraphQL
			// reads: the Registry already traces its own handlers, and every
			// catalogue/account read passing through here would be a lot of
			// low-value span volume.
			forwardWithRuntime(enforcer, proxy, w, r, accountID)
			return
		}

		if isImportApplyPath(r.Method, r.URL.Path) {
			// Import apply is the single synchronous contract mutation that
			// returns the concrete service/version needed for registration.
			forwardImportApplyWithAutoRegister(proxy, s, contractFetcher, w, r, accountID)
			return
		}

		if isIntegrationDeletePath(r.Method, r.URL.Path) {
			forwardIntegrationDeleteWithWorkspaceCleanup(proxy, s, w, r, accountID)
			return
		}

		if isSDKGeneratePath(r.Method, r.URL.Path) {
			// The one REST-proxied request that needs a workspace check
			// BEFORE forwarding -- see sdk_generate_intercept.go
			// (Task 6, engine_workspace_registration_plan.md).
			forwardSDKGenerateWithWorkspaceGate(proxy, s, w, r, accountID)
			return
		}

		forwardRESTMutationWithSpan(proxy, enforcer, w, r, accountID)
	}
}

func firstRuntimeEnforcer(enforcers []*enginemiddleware.RuntimeEnforcer) *enginemiddleware.RuntimeEnforcer {
	if len(enforcers) == 0 {
		return nil
	}
	return enforcers[0]
}

func forwardWithRuntime(enforcer *enginemiddleware.RuntimeEnforcer, proxy Forwarder, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	forward := func(w http.ResponseWriter, r *http.Request) {
		proxy.Forward(w, r, "")
	}
	if enforcer == nil {
		forward(w, r)
		return
	}
	enforcer.Forward(w, r, accountID, forward)
}

// isMutatingMethod reports whether method is one Sprint 1's OTEL standard
// requires a span for: user/agent-triggered writes. GET (and any other
// method, e.g. HEAD/OPTIONS) is read-only from the Engine's perspective.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	default:
		return false
	}
}

// forwardRESTMutationWithSpan mirrors forwardMutationWithSpan in
// graphql_proxy.go -- same four audit attributes, same status-capture
// mechanism. Kept as a separate function (not a shared helper across both
// proxy types) because the two span names and user_action values differ and
// forcing them through one generic function would need attribute maps
// instead of readable literals, for no real complexity savings.
func forwardRESTMutationWithSpan(proxy Forwarder, enforcer *enginemiddleware.RuntimeEnforcer, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.proxy.rest_mutation", trace.WithAttributes(
		attribute.String("user_action", "rest."+r.Method),
		attribute.String("account_id", accountID.String()),
		attribute.String("path", r.URL.Path),
	))
	defer span.End()

	rec := newStatusRecorder(w)
	forwardWithRuntime(enforcer, proxy, rec, r.WithContext(ctx), accountID)
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}
