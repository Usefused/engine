package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"log/slog"
)

type TokenGeneratePayload struct {
	Name string `json:"name"`
}

func GenerateSDKTokenHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.sdk_tokens.generate")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			recordTokenMutationError(span, "unauthorized", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		if err != nil {
			recordTokenMutationError(span, "invalid", errors.New("invalid app_family_id"))
			http.Error(w, "invalid app_family_id", http.StatusBadRequest)
			return
		}

		var payload TokenGeneratePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			recordTokenMutationError(span, "invalid", err)
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		tokenVal := "fused-sdk-" + uuid.NewString()
		tokenHash := auth.HashToken(tokenVal)

		tok, err := s.CreateAppToken(ctx, familyID, tokenHash, payload.Name)
		if err != nil {
			recordTokenMutationError(span, "failed", err)
			slog.ErrorContext(ctx, "failed to create sdk token", slog.Any("error", err))
			http.Error(w, "failed to create token", http.StatusInternalServerError)
			return
		}
		span.SetAttributes(attribute.String("outcome", "created"))

		setOneTimeSecretResponseHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":            tok.ID,
			"app_family_id": tok.AppFamilyID,
			"name":          tok.Name,
			"token":         tokenVal, // ONLY RETURNED ONCE!
			"created_at":    tok.CreatedAt,
		})
	}
}

func RevokeSDKTokenHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.sdk_tokens.revoke")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			recordTokenMutationError(span, "unauthorized", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setTokenMutationActor(span, ctx)

		familyID, err := uuid.Parse(r.URL.Query().Get("app_family_id"))
		if err != nil {
			recordTokenMutationError(span, "invalid", errors.New("invalid app_family_id"))
			http.Error(w, "invalid app_family_id", http.StatusBadRequest)
			return
		}

		tokenName := r.URL.Query().Get("name")
		if tokenName == "" {
			recordTokenMutationError(span, "invalid", errors.New("missing token name"))
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("app.family_id", familyID.String()),
		)

		if err := s.RevokeAppToken(ctx, familyID, tokenName); err != nil {
			recordTokenMutationError(span, "failed", err)
			slog.ErrorContext(ctx, "failed to revoke token", slog.Any("error", err))
			http.Error(w, "failed to revoke token", http.StatusInternalServerError)
			return
		}

		span.SetAttributes(attribute.String("outcome", "revoked"))
		w.WriteHeader(http.StatusNoContent)
	}
}

func setTokenMutationActor(span trace.Span, ctx context.Context) {
	if actor, ok := accesscontrol.ActorFromContext(ctx); ok {
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
	}
}

func recordTokenMutationError(span trace.Span, outcome string, err error) {
	span.SetAttributes(attribute.String("outcome", outcome))
	span.RecordError(err)
	span.SetStatus(codes.Error, outcome)
}
