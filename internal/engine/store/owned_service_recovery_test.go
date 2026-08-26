package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recoveryQueryCounter counts only the membership query, not fixture setup or pool traffic.
type recoveryQueryCounter struct{ calls int }

// TraceQueryStart measures SQL batching without relying on timing.
func (c *recoveryQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// Only the production anti-join belongs to the constant-query assertion.
	if data.SQL == missingOwnedServiceIDsSQL {
		c.calls++
	}
	return ctx
}

// TraceQueryEnd leaves completion behavior unchanged because only query count is under test.
func (*recoveryQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestMissingOwnedServicesEmptyInput avoids a database lookup for an empty discovery result.
func TestMissingOwnedServicesEmptyInput(t *testing.T) {
	ids, err := (&postgresStore{}).MissingOwnedServiceIDs(context.Background(), nil)
	// A nil pool deliberately proves the guard occurs before database access.
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

// TestMissingOwnedServicesSQL uses a session-local table, never modifying existing tenant data.
func TestMissingOwnedServicesSQL(t *testing.T) {
	url := os.Getenv("FUSED_TEST_DATABASE_URL")
	// Integration execution requires an explicitly selected test database, not the application's DATABASE_URL.
	if url == "" {
		t.Skip("FUSED_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(url)
	// Invalid test configuration must fail before making any SQL changes.
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	counter := &recoveryQueryCounter{}
	config.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, config)
	// One connection keeps the temporary table scoped to this test's entire lifetime.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, "CREATE TEMP TABLE fused_workspace_services(service_id uuid PRIMARY KEY)")
	// The temporary shadow table guarantees even an accidentally reused database loses no rows.
	if err != nil {
		t.Fatal(err)
	}
	existing, absent, unrelated := uuid.New(), uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, "INSERT INTO fused_workspace_services SELECT unnest($1::uuid[])", []uuid.UUID{existing, unrelated})
	// Seed both a selected pin and an unselected row to exercise SQL scope.
	if err != nil {
		t.Fatal(err)
	}
	persistence := &cachedStore{Store: &postgresStore{db: pool}}
	missing, err := persistence.MissingOwnedServiceIDs(ctx, []uuid.UUID{existing, absent})
	// Only selected absent IDs may be returned, in one SQL query through the production cache wrapper.
	if err != nil || len(missing) != 1 || missing[0] != absent || counter.calls != 1 {
		t.Fatalf("missing=%v calls=%d err=%v", missing, counter.calls, err)
	}
}
