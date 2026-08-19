package store

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestIdempotentExecutionInsertContract locks INSERT columns and argument order
// so status/media additions cannot corrupt existing replay rows.
func TestIdempotentExecutionInsertContract(t *testing.T) {
	execution := &models.IdempotentExecution{
		ID: uuid.New(), AppID: uuid.New(), IdempotencyKeyHash: "key-hash",
		RequestBodyHash: "body-hash", Environment: "sandbox", ResponseBody: []byte(`{"ok":true}`),
		ResponseStatus: 201, ResponseMediaFamily: "json", ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	want := []any{
		execution.ID, execution.AppID, execution.IdempotencyKeyHash,
		execution.RequestBodyHash, execution.Environment, execution.ResponseBody,
		execution.ResponseStatus, execution.ResponseMediaFamily, execution.ExpiresAt,
	}
	args := idempotentExecutionInsertArgs(execution)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("idempotency INSERT args = %#v, want %#v", args, want)
	}
	for _, placeholder := range []string{"$1", "$2", "$3", "$4", "$5", "$6", "$7", "$8", "$9"} {
		if !strings.Contains(saveIdempotentExecutionQuery, placeholder) {
			t.Fatalf("idempotency INSERT query is missing placeholder %s", placeholder)
		}
	}
	if strings.Contains(saveIdempotentExecutionQuery, "$10") || len(args) != 9 {
		t.Fatalf("idempotency INSERT contract has %d args, want exactly 9", len(args))
	}
}

// TestPostgresIdempotentExecutionRoundTrip protects the rule that canonical private bytes rehydrate into the same executable mapping.
func TestPostgresIdempotentExecutionRoundTrip(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()

	appID := seedIdempotencyTestApp(t, ctx, pool)
	repository := NewPostgresStore(pool).(*postgresStore)
	execution := &models.IdempotentExecution{
		ID: uuid.New(), AppID: appID, IdempotencyKeyHash: "idempotency-" + uuid.NewString(),
		RequestBodyHash: "request-body-hash", Environment: "sandbox", ResponseBody: []byte(`{"id":"cached"}`),
		ResponseStatus: 201, ResponseMediaFamily: "json", ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
	}
	if err := repository.SaveIdempotentExecution(ctx, execution); err != nil {
		t.Fatalf("SaveIdempotentExecution() error = %v", err)
	}

	got, err := repository.GetIdempotentExecution(
		ctx, execution.AppID, execution.IdempotencyKeyHash, execution.RequestBodyHash,
	)
	if err != nil {
		t.Fatalf("GetIdempotentExecution() error = %v", err)
	}
	assertIdempotentExecutionRoundTrip(t, got, execution)
}

// seedIdempotencyTestApp inserts the minimum rows needed to exercise physical idempotency persistence through PostgreSQL.
func seedIdempotencyTestApp(t *testing.T, ctx context.Context, pool execer) uuid.UUID {
	t.Helper()
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	ownerTeamID := seedAppOwnerTeam(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'sdk', $3, 'Idempotency persistence test', 'go', $4)
	`, familyID, accountID, "idempotency-"+familyID.String(), ownerTeamID); err != nil {
		t.Fatalf("seed idempotency test app family: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key, source_hash, status)
		VALUES ($1, $2, $3, '1.0.0', $4, 'idempotency-test', 'active')
	`, appID, familyID, accountID, "sdk:idempotency:"+familyID.String()); err != nil {
		t.Fatalf("seed idempotency test app: %v", err)
	}
	return appID
}

// assertIdempotentExecutionRoundTrip compares replay body, status, media family,
// environment, and hashes after a real PostgreSQL read.
func assertIdempotentExecutionRoundTrip(t *testing.T, got, want *models.IdempotentExecution) {
	t.Helper()
	gotArgs, wantArgs := idempotentExecutionInsertArgs(got), idempotentExecutionInsertArgs(want)
	if !reflect.DeepEqual(gotArgs[:8], wantArgs[:8]) {
		t.Fatalf("idempotency round trip = %#v, want %#v", got, want)
	}
	if got.CreatedAt.IsZero() || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("idempotency timestamps = created:%s expires:%s, want expires:%s", got.CreatedAt, got.ExpiresAt, want.ExpiresAt)
	}
}
