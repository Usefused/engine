package db

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLState extracts the PostgreSQL SQLSTATE code from a driver error chain
// without exposing pgconn types to HTTP, authorization, or worker packages.
func SQLState(err error) (string, bool) {
	var pgErr *pgconn.PgError
	// Only wrapped PostgreSQL server errors carry a reviewed SQLSTATE code.
	if !errors.As(err, &pgErr) || pgErr.Code == "" {
		return "", false
	}
	return pgErr.Code, true
}

// IsDatabaseUnavailable reports whether err represents a PostgreSQL read-only,
// shutdown, crash-recovery, or cannot-connect-now state and returns its SQLSTATE.
func IsDatabaseUnavailable(err error) (string, bool) {
	code, ok := SQLState(err)
	// Non-PostgreSQL failures keep their caller-specific classification.
	if !ok {
		return "", false
	}
	// Restrict unavailable classification to reviewed non-writable and lifecycle SQLSTATEs.
	switch code {
	case "25006", "57P01", "57P02", "57P03":
		return code, true
	default:
		return "", false
	}
}
