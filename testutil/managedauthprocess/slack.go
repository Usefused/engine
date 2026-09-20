package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// verifySlack checks a real consumer-owned grant without logging Slack workspace or user identity.
func verifySlack(ctx context.Context, pool *pgxpool.Pool) {
	var id uuid.UUID
	must(pool.QueryRow(ctx, `SELECT id FROM fused_auth_connections WHERE end_user_ref='slack-process-test'`).Scan(&id))
	runtime := store.NewPostgresStore(pool).(store.AuthConnectionRefreshStore)
	conn, err := runtime.GetAuthConnectionByID(ctx, id)
	must(err)
	// Standard Slack applications may issue non-expiring tokens unless rotation is enabled.
	if !conn.ManagedAuth || conn.EncryptedAccessToken == "" {
		log.Fatal("missing managed Slack connection")
	}
	dek, err := store.UnwrapDEK(masterKey(), conn.EncryptedDEK)
	must(err)
	access, err := store.DecryptWithDEK(dek, conn.EncryptedAccessToken)
	must(err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/auth.test", nil)
	must(err)
	request.Header.Set("Authorization", "Bearer "+access)
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: rejectProbeRedirect}
	response, err := client.Do(request)
	must(err)
	defer response.Body.Close()
	// Slack API failures can use HTTP 200, so both transport and provider success must be checked.
	if response.StatusCode != 200 {
		log.Fatalf("Slack auth.test HTTP %d", response.StatusCode)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	must(json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result))
	// Provider error and identity bodies are intentionally kept out of fixture output.
	if !result.OK {
		log.Fatal("Slack auth.test rejected the stored grant")
	}
	log.Printf("encrypted consumer grant: Slack auth.test ok=true; refresh token present=%t", conn.EncryptedRefreshToken != "")
}

// rejectProbeRedirect prevents a provider redirect from moving a fixture token to another endpoint.
func rejectProbeRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
