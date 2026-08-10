package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	Name      string   `json:"name"`
	Allow     []string `json:"allow"`
	ExpiresIn *int64   `json:"expires_in"`
}

func GenerateAppTokenHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.app_tokens.generate")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			recordTokenMutationError(span, "unauthorized")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		if err != nil {
			recordTokenMutationError(span, "invalid")
			http.Error(w, "invalid app_family_id", http.StatusBadRequest)
			return
		}

		var payload TokenGeneratePayload
		if err := decodeOneStrictJSON(r.Body, &payload); err != nil {
			recordTokenMutationError(span, "invalid")
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		expiresIn, err := tokenExpiryDuration(payload.ExpiresIn)
		if err != nil {
			recordTokenMutationError(span, "invalid")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		tokenVal, tok, err := applifecycle.New(s).GenerateToken(ctx, applifecycle.GenerateTokenParams{
			AppFamilyID: familyID, Name: payload.Name, Allow: payload.Allow, ExpiresIn: expiresIn,
		})
		if err != nil {
			if errors.Is(err, applifecycle.ErrTokenPolicyInvalid) {
				recordTokenMutationError(span, "invalid")
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			recordTokenMutationError(span, "failed")
			slog.ErrorContext(ctx, "failed to create app token", slog.String("error_code", "token_persistence_failed"))
			http.Error(w, "failed to create token", http.StatusInternalServerError)
			return
		}
		span.SetAttributes(attribute.String("outcome", "created"))

		setOneTimeSecretResponseHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{
			"id":            tok.ID,
			"app_family_id": tok.AppFamilyID,
			"name":          tok.Name,
			"allow":         projectTokenAllow(tok.AllowAll, tok.AllowedOperations),
			"expires_at":    tok.ExpiresAt,
			"token":         tokenVal, // ONLY RETURNED ONCE!
			"created_at":    tok.CreatedAt,
		})
	}
}

func RevokeAppTokenHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.app_tokens.revoke")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			recordTokenMutationError(span, "unauthorized")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		if err != nil {
			recordTokenMutationError(span, "invalid")
			http.Error(w, "invalid app_family_id", http.StatusBadRequest)
			return
		}

		tokenName := r.URL.Query().Get("name")
		if tokenName == "" {
			recordTokenMutationError(span, "invalid")
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		if err := s.RevokeAppToken(ctx, familyID, tokenName); err != nil {
			recordTokenMutationError(span, "failed")
			slog.ErrorContext(ctx, "failed to revoke token", slog.String("error_code", "token_revoke_failed"))
			http.Error(w, "failed to revoke token", http.StatusInternalServerError)
			return
		}

		span.SetAttributes(attribute.String("outcome", "revoked"))
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
		return []string{"*"}
	}
	return operations
}

func setTokenMutationActor(span trace.Span, ctx context.Context) {
	if actor, ok := accesscontrol.ActorFromContext(ctx); ok {
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
	}
}

func recordTokenMutationError(span trace.Span, outcome string) {
	span.SetAttributes(attribute.String("outcome", outcome))
	span.SetStatus(codes.Error, outcome)
}
