package store

import (
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// webhookActivityReceipt constructs synthetic canonical webhook receipts without provider payloads or credentials.
func webhookActivityReceipt(base models.EngineExecutionEvent, registration uuid.UUID, delivery string, delay int64) models.EngineExecutionEvent {
	base.ID, base.WebhookID = uuid.New(), registration
	base.AppFamilyID, base.AppID, base.AppTokenID, base.AppVersion = uuid.Nil, uuid.Nil, uuid.Nil, ""
	base.Transport, base.Direction = models.EngineExecutionTransportWebhook, models.EngineExecutionDirectionInbound
	base.EventName, base.EndpointName, base.ExternalID = "invoice.paid", "invoice.paid", uuid.NewString()
	base.DeliveryStatus, base.VerificationStatus, base.LatencyMs = delivery, "verified", delay
	base.Status = models.EngineExecutionStatusSuccess
	// Canonical failure status includes rejected ingress as well as failed subscriber delivery.
	if delivery == "failed" || delivery == "rejected" {
		base.Status = models.EngineExecutionStatusFailed
		base.FailureReason = "delivery_failed"
	}
	return base
}

// TestWebhookRegistrationSQLIsolation verifies account, service, registration, dates, totals, and page filters against PostgreSQL.
func TestWebhookRegistrationSQLIsolation(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	registration, sibling := uuid.New(), uuid.New()
	target := []models.EngineExecutionEvent{
		webhookActivityReceipt(fixture.event, registration, "delivered", 10),
		webhookActivityReceipt(fixture.event, registration, "failed", 20),
		webhookActivityReceipt(fixture.event, registration, "rejected", 30),
		webhookActivityReceipt(fixture.event, registration, "acked", 40),
	}
	// Controls share the event name and timestamp; only exact identity predicates can exclude them.
	otherRegistration := webhookActivityReceipt(fixture.event, sibling, "failed", 900)
	otherAccount := webhookActivityReceipt(fixture.event, registration, "failed", 900)
	otherAccount.AccountID = uuid.New()
	otherService := webhookActivityReceipt(fixture.event, registration, "failed", 900)
	otherService.ServiceID = uuid.New()
	earlier := webhookActivityReceipt(fixture.event, registration, "failed", 900)
	earlier.StartedAt = fixture.event.StartedAt.Add(-48 * time.Hour)
	events := append(target, otherRegistration, otherAccount, otherService, earlier)
	require.NoError(t, fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, events))
	start, end := fixture.event.StartedAt.Add(-time.Hour), fixture.event.StartedAt.Add(time.Hour)
	filter := WebhookActivityFilter{AccountID: fixture.event.AccountID, ServiceID: fixture.event.ServiceID, RegistrationID: &registration, StartDate: &start, EndDate: &end, Limit: 2}
	// The real cache wrapper must preserve the optional reporting capability and exact scope.
	reader := &cachedStore{Store: fixture.repository}
	first, total, err := reader.ListWebhookActivity(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 4, total)
	require.Len(t, first, 2)
	filter.Offset = 2
	second, total, err := reader.ListWebhookActivity(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 4, total)
	require.Len(t, second, 2)
	require.NotEqual(t, first[0].ID, second[0].ID)
	// A complete traversal at tied timestamps must neither duplicate nor omit a receipt.
	ids := map[uuid.UUID]bool{}
	for _, row := range append(first, second...) {
		ids[row.ID] = true
		require.Equal(t, fixture.event.AccountID, row.AccountID)
		require.Equal(t, fixture.event.ServiceID, row.ServiceID)
		require.Equal(t, "invoice.paid", row.EventType)
	}
	require.Len(t, ids, 4)
	analytics, err := reader.GetWebhookActivityAnalytics(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 4, analytics.TotalIngested)
	require.EqualValues(t, 2, analytics.TotalDelivered)
	require.EqualValues(t, 1, analytics.TotalFailed)
	require.EqualValues(t, 1, analytics.TotalRejected)
	require.Equal(t, 25.0, analytics.AverageLatencyMs)
	require.InDelta(t, 38.5, analytics.P95LatencyMs, 0.001)
	// Existing service Activity still includes sibling registrations within the same tenant and date bounds.
	serviceRows, serviceTotal, err := fixture.repository.ListWebhookEventsByService(fixture.ctx, filter.AccountID, filter.ServiceID, "", 20, 0, &start, &end)
	require.NoError(t, err)
	require.EqualValues(t, 5, serviceTotal)
	require.Len(t, serviceRows, 5)
	serviceAnalytics, err := fixture.repository.GetWebhookAnalytics(fixture.ctx, filter.AccountID, filter.ServiceID, "", &start, &end)
	require.NoError(t, err)
	require.EqualValues(t, 5, serviceAnalytics.TotalIngested)
	filter.Offset = 0
	filter.Status = "failed"
	failures, total, err := reader.ListWebhookActivity(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, failures, 2)
	filtered, err := reader.GetWebhookActivityAnalytics(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, filtered.TotalIngested)
	filter.Status = "rejected"
	rejected, total, err := reader.ListWebhookActivity(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "rejected", rejected[0].DeliveryStatus)
	filter.Status = "success"
	successful, total, err := reader.ListWebhookActivity(fixture.ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, successful, 2)
	filter.EventName = "invoice.missing"
	_, total, err = reader.ListWebhookActivity(fixture.ctx, filter)
	require.NoError(t, err)
	require.Zero(t, total)
}

// TestWebhookRegistrationLookupSQL prevents partial slug searches or service-filter mismatches on detail routes.
func TestWebhookRegistrationLookupSQL(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	service := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services(service_id,service_name,service_slug) VALUES($1,'Payments','@stripe/payments')`, service)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_service_versions(service_id,service_version_id,version) VALUES($1,gen_random_uuid(),'1.0.0')`, service)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_webhooks(service_id,service_version_id,label,slug,owning_config_key) VALUES($1,gen_random_uuid(),'Events','exact-route','webhook:events'),($1,gen_random_uuid(),'Other','exact-route-longer','webhook:other')`, service)
	require.NoError(t, err)
	scope := accesscontrol.AuthorizedScope{IDs: []uuid.UUID{service}}
	rows, total, err := repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{ServiceID: &service, Slug: "exact-route"}, 1, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, rows, 1)
	require.Equal(t, "Events", rows[0].Label)
	require.NotEqual(t, uuid.Nil, rows[0].ID)
	rows, total, err = repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{ServiceID: &service, Slug: "exact"}, 1, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)
	other := uuid.New()
	rows, total, err = repository.ListAuthorizedWorkspaceWebhookPage(ctx, scope, WorkspaceWebhookFilter{ServiceID: &other, Slug: "exact-route"}, 1, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)
}
