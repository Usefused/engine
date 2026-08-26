package store

import (
	"context"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type executionAnalyticsQueryTracer struct {
	queries atomic.Int64
}

// TraceQueryStart counts statements issued by the dedicated traced repository;
// fixture writes and schema setup use a separate untraced pool.
func (tracer *executionAnalyticsQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tracer.queries.Add(1)
	return ctx
}

// TraceQueryEnd completes pgx tracing without changing query outcomes.
func (*executionAnalyticsQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestEngineExecutionWhereClauseScopesAppFamilyAndVersionInSQL(t *testing.T) {
	accountID := uuid.New()
	appFamilyID := uuid.New()
	appID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: appFamilyID, AppID: appID, Transport: "sdk", Status: "failed",
	})

	if !strings.HasPrefix(whereClause, "WHERE account_id = $1 AND app_family_id = $2 AND app_id = $3") {
		t.Fatalf("where clause does not enforce account, family, and app scope: %s", whereClause)
	}
	wantArgs := []any{accountID, appFamilyID, appID, "sdk", "failed"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestEngineExecutionWhereClauseScopesWholeAppFamilyInSQL locks tenant and provider-accounting predicates together.
func TestEngineExecutionWhereClauseScopesWholeAppFamilyInSQL(t *testing.T) {
	accountID := uuid.New()
	appFamilyID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: appFamilyID,
	})

	// Aggregate reads must never count the logical envelope in addition to its children.
	if whereClause != "WHERE account_id = $1 AND app_family_id = $2 AND execution_kind = 'physical'" {
		t.Fatalf("where clause = %s, want family scope", whereClause)
	}
	// Kind is a server-owned constant, not an untrusted query parameter.
	if !reflect.DeepEqual(args, []any{accountID, appFamilyID}) {
		t.Fatalf("args = %#v", args)
	}
}

func TestEngineExecutionWhereClauseDefaultsToServiceScope(t *testing.T) {
	serviceID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: uuid.New(), ServiceID: serviceID,
	})

	if !strings.Contains(whereClause, "service_id = $2") || strings.Contains(whereClause, "app_family_id") {
		t.Fatalf("where clause = %s, want service scope", whereClause)
	}
	if args[1] != serviceID {
		t.Fatalf("scope arg = %v, want %s", args[1], serviceID)
	}
}

func TestValidateExecutionEventIdentity(t *testing.T) {
	valid := models.EngineExecutionEvent{
		Transport:   models.EngineExecutionTransportSDK,
		AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.2.0",
	}
	if err := validateExecutionEventIdentity(valid); err != nil {
		t.Fatalf("valid SDK identity rejected: %v", err)
	}
	rest := valid
	rest.Transport = models.EngineExecutionTransportREST
	if err := validateExecutionEventIdentity(rest); err != nil {
		t.Fatalf("valid REST identity rejected: %v", err)
	}

	missing := valid
	missing.AppFamilyID = uuid.Nil
	if err := validateExecutionEventIdentity(missing); err == nil {
		t.Fatal("SDK event without family identity was accepted")
	}

	webhook := models.EngineExecutionEvent{Transport: models.EngineExecutionTransportWebhook}
	if err := validateExecutionEventIdentity(webhook); err != nil {
		t.Fatalf("webhook without app identity rejected: %v", err)
	}
}

func TestEngineExecutionEventPersistenceUsesAppIdentity(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	persistExecutionActivityFixture(t, fixture)
	assertExecutionActivityProjection(t, fixture)
	assertExecutionActivityExactAppScope(t, fixture)
	assertRemovedServiceExecutionHistory(t, fixture)
	assertAppIndependentWebhookPersistence(t, fixture)
}

// TestWorkspaceExecutionAnalyticsRanksCanonicalReceiptDimensions verifies the
// bounded workspace overview against PostgreSQL rather than an in-memory fake.
func TestWorkspaceExecutionAnalyticsRanksCanonicalReceiptDimensions(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	seedWorkspaceExecutionAnalyticsDimensions(t, fixture)
	events := workspaceExecutionAnalyticsEvents(fixture.event)
	if err := fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, events); err != nil {
		t.Fatalf("persist workspace analytics events: %v", err)
	}
	tracer := &executionAnalyticsQueryTracer{}
	config := fixture.pool.Config()
	config.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		t.Fatalf("open traced workspace analytics pool: %v", err)
	}
	t.Cleanup(tracedPool.Close)
	repository := NewPostgresStore(tracedPool).(*postgresStore)
	startDate, endDate := fixture.event.StartedAt.Add(-time.Hour), fixture.event.StartedAt.Add(time.Hour)
	analytics, err := repository.GetWorkspaceExecutionAnalytics(fixture.ctx, fixture.event.AccountID, startDate, endDate)
	if err != nil {
		t.Fatalf("get workspace execution analytics: %v", err)
	}
	assertWorkspaceExecutionAnalytics(t, analytics)
	if got := tracer.queries.Load(); got != 2 {
		t.Fatalf("workspace analytics query count = %d, want 2", got)
	}
}

// seedWorkspaceExecutionAnalyticsDimensions creates only Engine-local labels
// and family-to-bucket mappings required by the aggregate SQL joins.
func seedWorkspaceExecutionAnalyticsDimensions(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	accountID := fixture.event.AccountID
	ownerTeamID := seedAppOwnerTeam(t, fixture.ctx, fixture.pool)
	serviceB, secondSDKFamilyID, mcpFamilyID := workspaceAnalyticsDimensionIDs(fixture.event)
	sdkFamilyID := fixture.event.AppFamilyID
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $4, 'sdk', 'jira', 'Jira SDK', 'typescript', $5),
		       ($2, $4, 'sdk', 'plunk', 'Plunk SDK', 'typescript', $5),
		       ($3, $4, 'mcp', 'nimble', 'Nimble MCP', NULL, $5)`,
		sdkFamilyID, secondSDKFamilyID, mcpFamilyID, accountID, ownerTeamID); err != nil {
		t.Fatalf("seed workspace analytics app families: %v", err)
	}
	primaryBucketID := uuid.NewSHA1(sdkFamilyID, []byte("primary-bucket"))
	secondaryBucketID := uuid.NewSHA1(sdkFamilyID, []byte("secondary-bucket"))
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, 'Primary'), ($2, 'Secondary')`, primaryBucketID, secondaryBucketID); err != nil {
		t.Fatalf("seed workspace analytics buckets: %v", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
		VALUES ($1, $4), ($2, $5), ($3, $4)`, sdkFamilyID, secondSDKFamilyID, mcpFamilyID, primaryBucketID, secondaryBucketID); err != nil {
		t.Fatalf("seed workspace analytics bucket mappings: %v", err)
	}
	serviceA := fixture.event.ServiceID
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_workspace_services (service_id, service_slug, service_name)
		VALUES ($1, 'jira', 'Jira'), ($2, 'nimble', 'Nimble')`, serviceA, serviceB); err != nil {
		t.Fatalf("seed workspace analytics services: %v", err)
	}
}

// workspaceAnalyticsDimensionIDs derives stable fixture identities shared by
// seeding and event construction without mutable package-level state.
func workspaceAnalyticsDimensionIDs(base models.EngineExecutionEvent) (uuid.UUID, uuid.UUID, uuid.UUID) {
	return uuid.NewSHA1(base.ServiceID, []byte("secondary-service")),
		uuid.NewSHA1(base.AppFamilyID, []byte("secondary-sdk")),
		uuid.NewSHA1(base.AppFamilyID, []byte("mcp-family"))
}

// workspaceExecutionAnalyticsEvents builds high-cardinality service data plus
// out-of-range and cross-account controls without querying during construction.
func workspaceExecutionAnalyticsEvents(base models.EngineExecutionEvent) []models.EngineExecutionEvent {
	serviceB, secondSDKFamilyID, mcpFamilyID := workspaceAnalyticsDimensionIDs(base)
	events := []models.EngineExecutionEvent{
		workspaceAnalyticsEvent(base, base.AppFamilyID, base.AppID, base.ServiceID, models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt),
		workspaceAnalyticsEvent(base, base.AppFamilyID, base.AppID, base.ServiceID, models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusFailed, base.StartedAt),
		workspaceAnalyticsEvent(base, base.AppFamilyID, base.AppID, serviceB, models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusFailed, base.StartedAt),
		workspaceAnalyticsEvent(base, secondSDKFamilyID, uuid.New(), serviceB, models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt),
		workspaceAnalyticsEvent(base, mcpFamilyID, uuid.New(), serviceB, models.EngineExecutionTransportMCP, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt),
		workspaceAnalyticsEvent(base, mcpFamilyID, uuid.New(), serviceB, models.EngineExecutionTransportMCP, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt),
		workspaceAnalyticsEvent(base, mcpFamilyID, uuid.New(), serviceB, models.EngineExecutionTransportMCP, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt),
		workspaceAnalyticsEvent(base, uuid.Nil, uuid.Nil, base.ServiceID, models.EngineExecutionTransportWebhook, models.EngineExecutionDirectionInbound, models.EngineExecutionStatusFailed, base.StartedAt),
	}
	for index := 0; index < 30; index++ {
		events = append(events, workspaceAnalyticsEvent(base, base.AppFamilyID, base.AppID, uuid.New(), models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt))
	}
	events = append(events,
		workspaceAnalyticsEvent(base, base.AppFamilyID, base.AppID, base.ServiceID, models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt.Add(-2*time.Hour)),
		workspaceAnalyticsEventWithAccount(base, uuid.New()),
	)
	return events
}

// workspaceAnalyticsEvent creates one valid canonical receipt with the exact
// dimensions needed by an aggregate test and no provider-controlled payload.
func workspaceAnalyticsEvent(base models.EngineExecutionEvent, familyID, appID, serviceID uuid.UUID, transport, direction, status string, startedAt time.Time) models.EngineExecutionEvent {
	event := base
	event.ID, event.AppFamilyID, event.AppID = uuid.New(), familyID, appID
	event.ServiceID, event.Transport, event.Direction, event.Status = serviceID, transport, direction, status
	event.StartedAt, event.EndedAt, event.CreatedAt = startedAt, startedAt, startedAt
	event.EndpointName, event.LatencyMs, event.AttemptCount = "fixture.operation", 10, 1
	if transport == models.EngineExecutionTransportWebhook {
		// Webhook receipts intentionally carry no SDK/MCP family identity.
		event.AppFamilyID, event.AppID, event.AppVersion = uuid.Nil, uuid.Nil, ""
	}
	return event
}

// workspaceAnalyticsEventWithAccount creates a same-range tenant-isolation
// control that must not affect any overview metric.
func workspaceAnalyticsEventWithAccount(base models.EngineExecutionEvent, accountID uuid.UUID) models.EngineExecutionEvent {
	event := workspaceAnalyticsEvent(base, base.AppFamilyID, base.AppID, base.ServiceID, models.EngineExecutionTransportSDK, models.EngineExecutionDirectionOutbound, models.EngineExecutionStatusSuccess, base.StartedAt)
	event.AccountID = accountID
	return event
}

// assertWorkspaceExecutionAnalytics checks counts, SQL-ranked highlights, and
// the hard service-card bound from one isolated PostgreSQL result.
func assertWorkspaceExecutionAnalytics(t *testing.T, analytics models.WorkspaceExecutionAnalytics) {
	t.Helper()
	if analytics.TotalCalls != 38 || analytics.InboundCalls != 1 || analytics.SuccessfulCalls != 35 || analytics.FailedCalls != 3 {
		t.Fatalf("workspace summary = %#v", analytics.EngineExecutionAnalytics)
	}
	if len(analytics.ByService) != workspaceExecutionBreakdownLimit || analytics.ByService[0].Label != "Nimble" || analytics.ByService[0].TotalCalls != 5 {
		t.Fatalf("workspace service breakdown = %#v", analytics.ByService)
	}
	assertExecutionHighlight(t, analytics.MostUsedSDK, "Jira SDK", 33, 2)
	assertExecutionHighlight(t, analytics.MostUsedService, "Nimble", 5, 1)
	assertExecutionHighlight(t, analytics.MostFailedService, "Jira", 3, 2)
	assertExecutionHighlight(t, analytics.MostUsedBucket, "Primary", 36, 2)
}

// assertExecutionHighlight validates one SQL-selected top-card projection.
func assertExecutionHighlight(t *testing.T, item *models.EngineExecutionBreakdown, label string, totalCalls, failedCalls int64) {
	t.Helper()
	if item == nil || item.Label != label || item.TotalCalls != totalCalls || item.FailedCalls != failedCalls {
		t.Fatalf("execution highlight = %#v, want %s total=%d failed=%d", item, label, totalCalls, failedCalls)
	}
}

type executionActivityFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	repository *postgresStore
	event      models.EngineExecutionEvent
}

// newExecutionActivityFixture owns one isolated schema and a complete event identity.
func newExecutionActivityFixture(t *testing.T) executionActivityFixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	t.Cleanup(pool.Close)
	repository := NewPostgresStore(pool).(*postgresStore)
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	return executionActivityFixture{ctx: ctx, pool: pool, repository: repository, event: models.EngineExecutionEvent{
		ID: uuid.New(), AccountID: accountID, AppFamilyID: familyID, AppID: appID, AppTokenID: uuid.New(),
		AppVersion: "2.0.0", Transport: models.EngineExecutionTransportMCP,
		ProviderProtocol: "graphql", Direction: models.EngineExecutionDirectionOutbound,
		ServiceID: serviceID, ServiceVersionID: serviceVersionID.String(), EndpointName: "issues.list", Status: models.EngineExecutionStatusSuccess,
		LatencyMs: 12, AttemptCount: 1, StartedAt: now, EndedAt: now, CreatedAt: now,
	}}
}

// persistExecutionActivityFixture writes current display metadata and one canonical receipt.
func persistExecutionActivityFixture(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	event := fixture.event
	serviceVersionID := uuid.MustParse(event.ServiceVersionID)
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_workspace_services (service_id, service_slug, service_name) VALUES ($1, $2, $3)`, event.ServiceID, "linear", "Linear"); err != nil {
		t.Fatalf("persist execution service display fixture: %v", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version) VALUES ($1, $2, $3)`, event.ServiceID, serviceVersionID, "2026-07-21"); err != nil {
		t.Fatalf("persist execution version display fixture: %v", err)
	}
	if err := fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, []models.EngineExecutionEvent{event}); err != nil {
		t.Fatalf("persist app execution event: %v", err)
	}
}

// assertExecutionActivityProjection verifies exact receipt and display identity in one page.
func assertExecutionActivityProjection(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	event := fixture.event
	rows, total, err := fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, EngineExecutionFilter{
		AccountID: event.AccountID, AppFamilyID: event.AppFamilyID, AppID: event.AppID, Limit: 10,
	})
	projected := requireSingleExecutionEvent(t, rows, total, err)
	assertExecutionCoreIdentity(t, projected)
	assertExecutionDisplayIdentity(t, projected)
	assertExecutionNonNilDimensions(t, projected)
}

// assertExecutionActivityExactAppScope keeps unrelated app identities outside the page.
func assertExecutionActivityExactAppScope(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	event := fixture.event
	_, total, err := fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, EngineExecutionFilter{
		AccountID: event.AccountID, AppFamilyID: event.AppFamilyID, AppID: uuid.New(), Limit: 10,
	})
	if err != nil || total != 0 {
		t.Fatalf("exact app filter total = %d, error %v", total, err)
	}

}

// assertRemovedServiceExecutionHistory keeps history and summaries consistent after removal.
func assertRemovedServiceExecutionHistory(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	event := fixture.event
	// Historical receipts remain queryable after current workspace metadata is
	// removed; only the query-time display labels become unavailable.
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM fused_workspace_services WHERE service_id = $1`, event.ServiceID); err != nil {
		t.Fatalf("remove execution service display fixture: %v", err)
	}
	rows, total, err := fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, EngineExecutionFilter{
		AccountID: event.AccountID, AppFamilyID: event.AppFamilyID, AppID: event.AppID, Limit: 1,
	})
	projected := requireSingleExecutionEvent(t, rows, total, err)
	if projected.ServiceName != "" || projected.ServiceSlug != "" || projected.ServiceVersion != "" {
		t.Fatalf("removed service unexpectedly retained current display metadata: %#v", projected)
	}
	analytics, err := fixture.repository.GetWorkspaceExecutionAnalytics(fixture.ctx, event.AccountID, event.StartedAt.Add(-time.Minute), event.StartedAt.Add(time.Minute))
	assertRemovedServiceAnalytics(t, analytics, err)
}

// requireSingleExecutionEvent validates one bounded result page and returns its row.
func requireSingleExecutionEvent(t *testing.T, rows []models.EngineExecutionEvent, total int64, err error) models.EngineExecutionEvent {
	t.Helper()
	if err != nil {
		t.Fatalf("list execution events: %v", err)
	}
	if total != 1 {
		t.Fatalf("execution event total = %d, want 1", total)
	}
	if len(rows) != 1 {
		t.Fatalf("execution event rows = %#v, want one", rows)
	}
	return rows[0]
}

// assertExecutionCoreIdentity verifies persisted app and protocol fields.
func assertExecutionCoreIdentity(t *testing.T, event models.EngineExecutionEvent) {
	t.Helper()
	if event.AppVersion != "2.0.0" || event.ProviderProtocol != "graphql" || event.AppTokenID == uuid.Nil {
		t.Fatalf("persisted identity = %#v", event)
	}
}

// assertExecutionDisplayIdentity verifies the Engine-local service projection.
func assertExecutionDisplayIdentity(t *testing.T, event models.EngineExecutionEvent) {
	t.Helper()
	if event.ServiceName != "Linear" || event.ServiceSlug != "linear" || event.ServiceVersion != "2026-07-21" {
		t.Fatalf("execution display identity = %#v", event)
	}
}

// assertExecutionNonNilDimensions keeps absent bounded arrays normalized.
func assertExecutionNonNilDimensions(t *testing.T, event models.EngineExecutionEvent) {
	t.Helper()
	if event.AuthSchemeNames == nil || event.RateLimitScopeKinds == nil || event.RateLimitUnits == nil || event.RateLimitUnitTotals == nil {
		t.Fatalf("non-null execution dimensions decoded as nil: %#v", event)
	}
}

// assertRemovedServiceAnalytics keeps removed-service history in workspace totals.
func assertRemovedServiceAnalytics(t *testing.T, analytics models.WorkspaceExecutionAnalytics, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("get removed-service workspace analytics: %v", err)
	}
	if analytics.TotalCalls != 1 || len(analytics.ByService) != 1 {
		t.Fatalf("removed-service workspace analytics = %#v", analytics)
	}
	if analytics.ByService[0].Label != "Service metadata unavailable" || analytics.MostUsedService == nil || analytics.MostUsedService.Label != "Service metadata unavailable" {
		t.Fatalf("removed-service labels = %#v / %#v", analytics.ByService, analytics.MostUsedService)
	}
}

// assertAppIndependentWebhookPersistence retains the app-free webhook identity contract.
func assertAppIndependentWebhookPersistence(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	webhook := fixture.event
	webhook.ID, webhook.Transport = uuid.New(), models.EngineExecutionTransportWebhook
	webhook.AppFamilyID, webhook.AppID, webhook.AppVersion = uuid.Nil, uuid.Nil, ""
	if err := fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, []models.EngineExecutionEvent{webhook}); err != nil {
		t.Fatalf("persist app-independent webhook event: %v", err)
	}
}
