package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/applifecycle"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type TokenGeneratePayload struct {
	Name        string                    `json:"name"`
	Allow       []string                  `json:"allow"`
	ExpiresIn   *int64                    `json:"expires_in"`
	BindingMode store.AppTokenBindingMode `json:"binding_mode"`
	Bindings    []TokenBindingPayload     `json:"bindings"`
}

type TokenBindingPayload struct {
	ServiceSlug string     `json:"service_slug"`
	AuthName    string     `json:"auth_name"`
	EndUserRef  string     `json:"end_user_ref"`
	ResourceID  *uuid.UUID `json:"resource_id"`
}

type AppTokenRevoker interface {
	RevokeAppToken(context.Context, uuid.UUID, string) (*store.AppTokenRevocation, error)
}

// GenerateAppTokenHandler issues one family credential only after validation,
// preserving reviewed token failures instead of hiding them as server outages.
func GenerateAppTokenHandler(s store.Store) http.HandlerFunc {
	// The request owns its one-time credential and canonical mutation span.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.app_tokens.generate")
		defer span.End()
		ctx = contextWithControlMutationTelemetryRecorded(ctx)

		_, err := controlActorAccount(ctx)
		// Anonymous requests cannot issue execution credentials.
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeUnauthorized)
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to generate an app token.", "Log in or provide a valid Fused credential.", "app_token_generation", "", "not_committed", "")
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		// Token scope requires an exact identity, never a guessed app name.
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_app_family_id", "The SDK or MCP ID is not a valid UUID.", "Use the SDK ID or MCP ID shown by the corresponding list command.", "app_token_generation", "", "not_committed", "")
			return
		}

		var payload TokenGeneratePayload
		// Reject malformed or extra input before interpreting credential policy.
		if err := decodeOneStrictJSON(r.Body, &payload); err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_app_token_request", "The app token request body is invalid.", "Check the token name, expiry, allow policy, and fixed bindings.", "app_token_generation", "", "not_committed", "")
			return
		}
		expiresIn, err := tokenExpiryDuration(payload.ExpiresIn)
		// Invalid lifetimes must not silently create non-expiring credentials.
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_app_token_expiry", err.Error(), "Provide a positive in-range expiry in seconds or omit it.", "app_token_generation", "", "not_committed", "")
			return
		}
		bindings, err := tokenBindingRequests(payload.Bindings)
		// A partial connected-user binding must never authorize issuance.
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_app_token_binding", err.Error(), "Provide service_slug, auth_name, and end_user_ref for every fixed binding.", "app_token_generation", "", "not_committed", "")
			return
		}
		actor, _ := accesscontrol.ActorFromContext(ctx)

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		tokenVal, tok, err := applifecycle.New(s).GenerateToken(ctx, applifecycle.GenerateTokenParams{
			AppFamilyID: familyID, Name: payload.Name, Allow: payload.Allow, ExpiresIn: expiresIn,
			BindingMode: payload.BindingMode, Bindings: bindings,
			IssuedBySubjectID: optionalActorID(actor.SubjectID), IssuedByCredentialID: optionalActorID(actor.CredentialID),
		})
		// Failure never reveals a generated plaintext or changes an existing token.
		if err != nil {
			writeAppTokenGenerationError(w, ctx, span, err)
			return
		}
		span.SetAttributes(attribute.String("outcome", string(applifecycle.OutcomeCreated)))

		setOneTimeSecretResponseHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{
			"id":            tok.ID,
			"app_family_id": tok.AppFamilyID,
			"name":          tok.Name,
			"allow":         projectTokenAllow(tok.AllowAll, tok.AllowedOperations),
			"expires_at":    tok.ExpiresAt,
			"binding_mode":  tok.BindingMode,
			"binding_count": len(bindings),
			"token":         tokenVal, // ONLY RETURNED ONCE!
			"created_at":    tok.CreatedAt,
		})
	}
}

// writeAppTokenGenerationError projects only reviewed domain failures; unknown
// persistence errors remain generic and never expose SQL or credential material.
func writeAppTokenGenerationError(w http.ResponseWriter, ctx context.Context, span trace.Span, err error) {
	// A duplicate label is a caller-resolvable conflict, not a database outage.
	if errors.Is(err, store.ErrAppTokenNameConflict) {
		recordTokenMutationError(span, applifecycle.OutcomeConflict)
		writeControlAPIMutationError(w, ctx, http.StatusConflict, "app_token_name_conflict", "A token with this name already exists for this app.", "Choose a different token name, or explicitly revoke the existing token before reusing its name. Existing token plaintext cannot be retrieved.", "app_token_generation", "", "not_committed", "")
		return
	}
	// Policy and binding validation retain their existing shared SDK/MCP contract.
	if errors.Is(err, applifecycle.ErrTokenPolicyInvalid) || errors.Is(err, store.ErrAppTokenBindingInvalid) {
		recordTokenMutationError(span, applifecycle.OutcomeInvalid)
		writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_app_token_policy", "The app token policy or fixed binding is invalid.", "Correct the allow policy or fixed bindings and retry.", "app_token_generation", "", "not_committed", "")
		return
	}
	recordTokenMutationError(span, applifecycle.OutcomeFailed)
	slog.ErrorContext(ctx, "failed to create app token", slog.String("error_code", "token_persistence_failed"))
	writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "app_token_create_failed", "The Engine could not create the app token.", "Inspect the current app token list before retrying, and use the request or trace ID to check Engine logs.", "app_token_generation", "", "unknown", "")
}

func tokenBindingRequests(payload []TokenBindingPayload) ([]store.AppTokenBindingRequest, error) {
	bindings := make([]store.AppTokenBindingRequest, len(payload))
	for index, item := range payload {
		serviceSlug := strings.TrimSpace(item.ServiceSlug)
		authName, endUserRef := strings.TrimSpace(item.AuthName), strings.TrimSpace(item.EndUserRef)
		if serviceSlug == "" || authName == "" || endUserRef == "" {
			return nil, errors.New("each binding requires service_slug, auth_name, and end_user_ref")
		}
		bindings[index] = store.AppTokenBindingRequest{
			ServiceSlug: serviceSlug, AuthName: authName,
			EndUserRef: endUserRef, ResourceID: item.ResourceID,
		}
	}
	return bindings, nil
}

func optionalActorID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// RevokeAppTokenHandler revokes one family token by name and returns stable
// diagnostics without exposing token plaintext, hashes, or store details.
func RevokeAppTokenHandler(revoker AppTokenRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.app_tokens.revoke")
		defer span.End()
		ctx = contextWithControlMutationTelemetryRecorded(ctx)

		_, err := controlActorAccount(ctx)
		// App-token revocation requires an authenticated control-plane actor.
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeUnauthorized)
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to revoke an app token.", "Log in or provide a valid Fused credential.", "app_token_revocation", "", "not_committed", "")
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		// Token scope requires an exact family identity, never a guessed app name.
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_app_family_id", "The SDK or MCP ID is not a valid UUID.", "Use the SDK ID or MCP ID shown by the corresponding list command.", "app_token_revocation", "", "not_committed", "")
			return
		}

		tokenName := r.URL.Query().Get("name")
		// Token name is part of the exact family-scoped revocation identity.
		if tokenName == "" {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "app_token_name_required", "The token name is required.", "Choose a token name from the SDK or MCP token list.", "app_token_revocation", "", "not_committed", "")
			return
		}

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		// Revocation failures remain opaque so credential history and store detail cannot leak.
		if _, err := revoker.RevokeAppToken(ctx, familyID, tokenName); err != nil {
			// Authoritative absence proves the revocation did not commit.
			if errors.Is(err, store.ErrAppTokenNotFound) {
				recordTokenMutationError(span, applifecycle.OutcomeInvalid)
				writeControlAPIMutationError(w, ctx, http.StatusNotFound, "app_token_not_found", "The selected app token was not found.", "Refresh the SDK or MCP token list before retrying.", "app_token_revocation", "", "not_committed", "")
				return
			}
			recordTokenMutationError(span, applifecycle.OutcomeFailed)
			slog.ErrorContext(ctx, "failed to revoke token", slog.String("error_code", "token_revoke_failed"))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "app_token_revoke_failed", "The Engine could not revoke the app token.", "Inspect the current app token list before retrying, and use the request or trace ID to check Engine logs.", "app_token_revocation", "", "unknown", "")
			return
		}

		span.SetAttributes(attribute.String("outcome", string(applifecycle.OutcomeRevoked)))
		w.WriteHeader(http.StatusNoContent)
	}
}

func tokenExpiryDuration(seconds *int64) (*time.Duration, error) {
	if seconds == nil {
		return nil, nil
	}
	const maxDurationSeconds = int64((1<<63 - 1) / int64(time.Second))
	if *seconds <= 0 || *seconds > maxDurationSeconds {
		return nil, errors.New("expires_in must be a positive in-range number of seconds")
	}
	duration := time.Duration(*seconds) * time.Second
	return &duration, nil
}

func projectTokenAllow(allowAll bool, operations []string) []string {
	if allowAll {
		return []string{store.AppTokenAllowAllWildcard}
	}
	return operations
}

func setTokenMutationActor(span trace.Span, ctx context.Context) {
	if actor, ok := accesscontrol.ActorFromContext(ctx); ok {
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
	}
}

func recordTokenMutationError(span trace.Span, outcome applifecycle.LifecycleOutcome) {
	span.SetAttributes(attribute.String("outcome", string(outcome)))
	span.SetStatus(codes.Error, string(outcome))
}
