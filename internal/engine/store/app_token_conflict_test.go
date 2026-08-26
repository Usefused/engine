package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type appTokenInsertTx struct {
	pgx.Tx
	tag pgconn.CommandTag
	err error
	sql string
}

// Exec captures the authoritative insert without requiring a live database for classification tests.
func (tx *appTokenInsertTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	tx.sql = sql
	return tx.tag, tx.err
}

// TestInsertActiveAppTokenClassifiesOnlyNameConflicts keeps dependency failures distinct from rejected names.
func TestInsertActiveAppTokenClassifiesOnlyNameConflicts(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	cases := []struct {
		name string
		tag  string
		err  error
		want error
	}{
		{name: "created", tag: "INSERT 0 1"},
		{name: "duplicate", tag: "INSERT 0 0", want: ErrAppTokenNameConflict},
		{name: "database", err: databaseErr, want: databaseErr},
	}
	// The database result, not a preflight read or error text, determines classification.
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { // Each result owns its captured transaction.
			tx := &appTokenInsertTx{tag: pgconn.NewCommandTag(test.tag), err: test.err}
			err := insertActiveAppToken(context.Background(), tx, AppTokenIssue{})
			// Only the expected typed domain error may cross the store boundary.
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			// Restrict conflict handling to names; other unique violations must still fail.
			if !strings.Contains(tx.sql, "ON CONFLICT (app_family_id, name) DO NOTHING") {
				t.Fatalf("missing exact conflict target: %s", tx.sql)
			}
		})
	}
}

// TestAppTokenDuplicateNameRollsBackAndPreservesExisting verifies real PostgreSQL
// conflict detection, history rollback, and deliberate name reuse after revocation.
func TestAppTokenDuplicateNameRollsBackAndPreservesExisting(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	existing := fixture.createToken(t, "agent", AppTokenPolicy{AllowAll: true})
	issue := AppTokenIssue{ID: uuid.New(), AppFamilyID: fixture.familyID, Name: "agent",
		TokenHash: "replacement-hash", Policy: AppTokenPolicy{AllowAll: true}, BindingMode: AppTokenBindingDynamic}
	_, err := fixture.repository.CreateAppToken(fixture.ctx, issue)
	// A duplicate must be a typed conflict and leave no second history or active row.
	if !errors.Is(err, ErrAppTokenNameConflict) {
		t.Fatalf("duplicate issuance = %v", err)
	}
	fixture.assertTokenTransactionRolledBack(t, issue.ID)
	fixture.assertListedTokenCount(t, 1)
	// Rejected issuance must not replace or revoke the original credential.
	if _, err := fixture.repository.AuthorizeApp(fixture.ctx, fixture.appID, "agent-hash"); err != nil {
		t.Fatalf("existing token was changed: %v", err)
	}
	fixture.assertAuthorizationDenied(t, issue.TokenHash, "rejected replacement")
	fixture.revokeAndAssertDenied(t, existing.ID, existing.Name, "agent-hash")
	// Explicit revocation releases the name while retaining its credential-free history.
	if _, err := fixture.repository.CreateAppToken(fixture.ctx, issue); err != nil {
		t.Fatalf("reuse after explicit revocation: %v", err)
	}
	fixture.assertListedTokenCount(t, 2)
}
