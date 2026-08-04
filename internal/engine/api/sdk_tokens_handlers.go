package api

import (
	"encoding/json"
	"net/http"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		artifactIDStr := r.URL.Query().Get("artifact_id")
		artifactID, err := uuid.Parse(artifactIDStr)
		if err != nil {
			http.Error(w, "invalid artifact_id", http.StatusBadRequest)
			return
		}

		var payload TokenGeneratePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("artifact_id", artifactID.String()),
			attribute.String("token_name", payload.Name),
		)

		tokenVal := "fused-sdk-" + uuid.NewString()
		tokenHash := auth.HashToken(tokenVal)

		tok, err := s.CreateSDKToken(ctx, artifactID, tokenHash, payload.Name)
		if err != nil {
			slog.ErrorContext(ctx, "failed to create sdk token", slog.Any("error", err))
			http.Error(w, "failed to create token", http.StatusInternalServerError)
			return
		}

		setOneTimeSecretResponseHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":          tok.ID,
			"artifact_id": tok.ArtifactID,
			"name":        tok.Name,
			"token":       tokenVal, // ONLY RETURNED ONCE!
			"created_at":  tok.CreatedAt,
		})
	}
}

func RevokeSDKTokenHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.sdk_tokens.revoke")
		defer span.End()

		_, err := controlActorAccount(ctx)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		artifactIDStr := r.URL.Query().Get("artifact_id")
		artifactID, err := uuid.Parse(artifactIDStr)
		if err != nil {
			http.Error(w, "invalid artifact_id", http.StatusBadRequest)
			return
		}

		tokenName := r.URL.Query().Get("name")
		if tokenName == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("artifact_id", artifactID.String()),
			attribute.String("token_name", tokenName),
		)

		if err := s.RevokeSDKToken(ctx, artifactID, tokenName); err != nil {
			slog.ErrorContext(ctx, "failed to revoke token", slog.Any("error", err))
			http.Error(w, "failed to revoke token", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
