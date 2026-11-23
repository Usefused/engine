package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestSQLStateAndDatabaseUnavailable verifies PostgreSQL SQLSTATE extraction and outage classification.
func TestSQLStateAndDatabaseUnavailable(t *testing.T) {
	// Non-driver errors must not report a SQLSTATE or unavailable classification.
	if code, ok := SQLState(errors.New("plain failure")); ok || code != "" {
		t.Fatalf("expected no SQLSTATE for plain error, got %q (%v)", code, ok)
	}
	if code, ok := IsDatabaseUnavailable(errors.New("plain failure")); ok || code != "" {
		t.Fatalf("expected plain error not unavailable, got %q (%v)", code, ok)
	}

	// Constraint violations expose SQLSTATE for safe telemetry but are not database outages.
	uniqueErr := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"})
	if code, ok := SQLState(uniqueErr); !ok || code != "23505" {
		t.Fatalf("expected SQLSTATE 23505, got %q (%v)", code, ok)
	}
	if _, ok := IsDatabaseUnavailable(uniqueErr); ok {
		t.Fatal("unique constraint violation must not be classified as database unavailable")
	}

	// Read-only and shutdown/recovery SQLSTATEs must classify as database unavailable.
	for _, want := range []string{"25006", "57P01", "57P02", "57P03"} {
		wrapped := fmt.Errorf("query: %w", &pgconn.PgError{Code: want})
		code, ok := IsDatabaseUnavailable(wrapped)
		if !ok || code != want {
			t.Fatalf("expected %s to classify as unavailable, got %q (%v)", want, code, ok)
		}
	}
}
