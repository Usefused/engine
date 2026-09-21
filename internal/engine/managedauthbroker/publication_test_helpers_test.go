package managedauthbroker

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// publicationConsumerContext creates a real verified installation so catalog tests exercise consumer authorization.
func publicationConsumerContext(t *testing.T, pool *pgxpool.Pool) context.Context {
	t.Helper()
	account := uuid.New()
	repository := NewStore(pool)
	_, tokens := enrollTestInstallation(t, repository, account)
	row, err := repository.GetByAccessToken(t.Context(), hashCredential(tokens.AccessToken))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_installations WHERE registry_account_id=$1`, account)
	})
	return context.WithValue(t.Context(), installationContextKey{}, row)
}
