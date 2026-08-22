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

func GenerateAppTokenHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.app_tokens.generate")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeUnauthorized)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			http.Error(w, "invalid app_family_id", http.StatusBadRequest)
			return
		}

		var payload TokenGeneratePayload
		if err := decodeOneStrictJSON(r.Body, &payload); err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		expiresIn, err := tokenExpiryDuration(payload.ExpiresIn)
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bindings, err := tokenBindingRequests(payload.Bindings)
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		if err != nil {
			if errors.Is(err, applifecycle.ErrTokenPolicyInvalid) || errors.Is(err, store.ErrAppTokenBindingInvalid) {
				recordTokenMutationError(span, applifecycle.OutcomeInvalid)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			recordTokenMutationError(span, applifecycle.OutcomeFailed)
			slog.ErrorContext(ctx, "failed to create app token", slog.String("error_code", "token_persistence_failed"))
			http.Error(w, "failed to create token", http.StatusInternalServerError)
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

func RevokeAppTokenHandler(revoker AppTokenRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.app_tokens.revoke")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeUnauthorized)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		if err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			http.Error(w, "invalid app_family_id", http.StatusBadRequest)
			return
		}

		tokenName := r.URL.Query().Get("name")
		if tokenName == "" {
			recordTokenMutationError(span, applifecycle.OutcomeInvalid)
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		if _, err := revoker.RevokeAppToken(ctx, familyID, tokenName); err != nil {
			recordTokenMutationError(span, applifecycle.OutcomeFailed)
			slog.ErrorContext(ctx, "failed to revoke token", slog.String("error_code", "token_revoke_failed"))
			http.Error(w, "failed to revoke token", http.StatusInternalServerError)
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
