package store

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestWorkspaceWebhookPageFiltersBeforePagination verifies real SQL scope, filtering, stable pages, and totals.
func TestWorkspaceWebhookPageFiltersBeforePagination(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	allowed, denied, deprecated := uuid.New(), uuid.New(), uuid.New()
	// Each service has independent membership so joins cannot widen a resource-scoped grant.
	for index, id := range []uuid.UUID{allowed, denied, deprecated} {
		_, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services(service_id,service_name,service_slug) VALUES($1,'Payments',$2)`, id, []string{"@stripe/payments", "@denied/payments", "@deprecated/payments"}[index])
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_service_versions(service_id,service_version_id,version) VALUES($1,gen_random_uuid(),'1.0.0')`, id)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_webhooks(service_id,service_version_id,label,slug,owning_config_key) VALUES($1,gen_random_uuid(),'alpha', $2,'webhook:alpha')`, id, uuid.NewString())
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx, `UPDATE fused_workspace_service_versions SET status='deprecated' WHERE service_id=$1`, deprecated)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_webhooks(service_id,service_version_id,label,slug,owning_config_key) VALUES($1,gen_random_uuid(),'beta%', $2,'webhook:beta')`, allowed, uuid.NewString())
	require.NoError(t, err)
	scope := accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowed}}
	first, total, err := repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{}, 1, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, first, 1)
	require.Equal(t, "alpha", first[0].Label)
	require.Equal(t, "@stripe/payments", first[0].ServiceRef)
	second, total, err := repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{}, 1, 1)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, second, 1)
	require.Equal(t, "beta%", second[0].Label)
	empty, total, err := repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{}, 1, 10)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Empty(t, empty)
	// An explicit filter cannot grant a service outside the caller's authorization scope.
	empty, total, err = repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{ServiceID: &denied}, 20, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, empty)
	found, total, err := repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{Search: "%"}, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, found, 1)
	found, total, err = repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{Search: "@STRIPE"}, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, found, 2)
	all, total, err := repository.ListAuthorizedWorkspaceWebhookPage(ctx, accesscontrol.AuthorizedScope{All: true}, WorkspaceWebhookFilter{}, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, all, 3)
	empty, total, err = repository.ListAuthorizedWorkspaceWebhookPage(ctx, accesscontrol.AuthorizedScope{}, WorkspaceWebhookFilter{}, 20, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, empty)
}
