package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPrepareServiceContractRows(t *testing.T) {
	endpoint := fusedobject.Endpoint{
		ID: uuid.New(), Name: "listWidgets", Method: "QUERY", Path: "/widgets/{id}", NormalizedPath: "/widgets/{id}",
	}
	webhook := fusedobject.Webhook{ID: uuid.New(), Name: "widget.created", Method: "POST", Description: "Created"}

	endpointRows := mustPrepareServiceContractEndpointRows(t, endpoint)
	webhookRows := mustPrepareServiceContractWebhookRows(t, webhook)
	if len(endpointRows) != 1 || endpointRows[0].EndpointID != endpoint.ID || endpointRows[0].Method != endpoint.Method {
		t.Fatalf("prepared endpoint rows = %#v", endpointRows)
	}
	if len(webhookRows) != 1 || webhookRows[0].WebhookID != webhook.ID || webhookRows[0].Method != webhook.Method {
		t.Fatalf("prepared webhook rows = %#v", webhookRows)
	}
	assertPreparedJSONRoundTrip(t, endpointRows[0].OperationJSON, endpoint)
	assertPreparedJSONRoundTrip(t, webhookRows[0].WebhookJSON, webhook)
}

func TestPrepareServiceContractRowsRejectMissingIDs(t *testing.T) {
	if _, err := prepareServiceContractEndpointRows([]fusedobject.Endpoint{{Name: "missingID"}}); err == nil {
		t.Fatal("endpoint preparation accepted a missing id")
	}
	if _, err := prepareServiceContractWebhookRows([]fusedobject.Webhook{{Name: "missingID"}}); err == nil {
		t.Fatal("webhook preparation accepted a missing id")
	}
}

func mustPrepareServiceContractEndpointRows(t *testing.T, endpoint fusedobject.Endpoint) []serviceContractEndpointRow {
	t.Helper()
	rows, err := prepareServiceContractEndpointRows([]fusedobject.Endpoint{endpoint})
	if err != nil {
		t.Fatalf("prepareServiceContractEndpointRows: %v", err)
	}
	return rows
}

func mustPrepareServiceContractWebhookRows(t *testing.T, webhook fusedobject.Webhook) []serviceContractWebhookRow {
	t.Helper()
	rows, err := prepareServiceContractWebhookRows([]fusedobject.Webhook{webhook})
	if err != nil {
		t.Fatalf("prepareServiceContractWebhookRows: %v", err)
	}
	return rows
}

func assertPreparedJSONRoundTrip[T any](t *testing.T, payload []byte, expected T) {
	t.Helper()
	var actual T
	if err := json.Unmarshal(payload, &actual); err != nil {
		t.Fatalf("decode prepared row: %v", err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("prepared row = %#v, want %#v", actual, expected)
	}
}

func TestWriteServiceContractBatchesUsesDocumentedFormula(t *testing.T) {
	rows := make([]int, 2*serviceContractSnapshotWriteBatchSize+1)
	var batchSizes []int
	err := writeServiceContractBatches(rows, func(payload []byte) error {
		var batch []int
		if err := json.Unmarshal(payload, &batch); err != nil {
			return err
		}
		batchSizes = append(batchSizes, len(batch))
		return nil
	})
	if err != nil {
		t.Fatalf("writeServiceContractBatches: %v", err)
	}
	want := []int{serviceContractSnapshotWriteBatchSize, serviceContractSnapshotWriteBatchSize, 1}
	if !slices.Equal(batchSizes, want) {
		t.Fatalf("batch sizes = %v, want %v", batchSizes, want)
	}
}

func TestServiceContractHashIgnoresEndpointAndWebhookOrder(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	firstEndpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "listWidgets", Method: "GET", Path: "/widgets"}
	secondEndpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "createWidget", Method: "POST", Path: "/widgets"}
	firstWebhook := fusedobject.Webhook{ID: uuid.New(), Name: "widget.created", Method: "POST"}
	secondWebhook := fusedobject.Webhook{ID: uuid.New(), Name: "widget.deleted", Method: "POST"}

	base := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 serviceID,
		ServiceVersionID:          versionID,
		Version:                   "2026-07-23",
		ServiceMetadata: fusedobject.ServiceMetadata{
			ID:               serviceID,
			ServiceVersionID: versionID,
			Name:             "Widgets",
		},
		Endpoints: []fusedobject.Endpoint{firstEndpoint, secondEndpoint},
		Webhooks:  []fusedobject.Webhook{firstWebhook, secondWebhook},
	}
	reordered := base
	reordered.Endpoints = []fusedobject.Endpoint{secondEndpoint, firstEndpoint}
	reordered.Webhooks = []fusedobject.Webhook{secondWebhook, firstWebhook}

	firstHash, err := serviceContractHash(base)
	if err != nil {
		t.Fatalf("serviceContractHash(base): %v", err)
	}
	secondHash, err := serviceContractHash(reordered)
	if err != nil {
		t.Fatalf("serviceContractHash(reordered): %v", err)
	}
	if firstHash != secondHash {
		t.Fatalf("expected stable hash independent of operation ordering, got %s and %s", firstHash, secondHash)
	}
	changedEnvelope := base
	changedEnvelope.ContractVersion++
	changedHash, err := serviceContractHash(changedEnvelope)
	if err != nil {
		t.Fatalf("serviceContractHash(changed envelope): %v", err)
	}
	if firstHash == changedHash {
		t.Fatal("contract envelope must contribute to the snapshot hash")
	}
}

func TestServiceContractHashCanonicalizesSchemaAndExtensionJSON(t *testing.T) {
	endpointID := uuid.New()
	schemaHash, err := canonicaljson.HexSHA256([]byte(`{"a":true,"b":1}`))
	if err != nil {
		t.Fatal(err)
	}
	base := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceMetadata:           fusedobject.ServiceMetadata{},
		Endpoints: []fusedobject.Endpoint{hashFixtureEndpoint(endpointID,
			[]byte(`{"b":1.0,"a":true}`), schemaHash, []byte(`{"z":1.0,"a":true}`))},
	}
	reordered := base
	reordered.Endpoints = []fusedobject.Endpoint{hashFixtureEndpoint(endpointID,
		[]byte(` { "a": true, "b": 10e-1 } `), schemaHash, []byte(` { "a": true, "z": 10e-1 } `))}
	first, err := serviceContractHash(base)
	if err != nil {
		t.Fatalf("serviceContractHash(base): %v", err)
	}
	second, err := serviceContractHash(reordered)
	if err != nil {
		t.Fatalf("serviceContractHash(reordered): %v", err)
	}
	if first != second {
		t.Fatalf("semantic raw JSON changed snapshot hash: %s != %s", first, second)
	}
	if string(base.Endpoints[0].Responses["200"].Representations[0].Schema.Raw) != `{"b":1.0,"a":true}` {
		t.Fatal("hash normalization mutated the caller's schema contract")
	}
	if string(base.Endpoints[0].Documentation.Extensions["x-fused-test"].Value) != `{"z":1.0,"a":true}` {
		t.Fatal("hash normalization mutated the caller's extension contract")
	}
}

func TestServiceContractHashAllowsAggregateLargerThanCanonicalValueLimit(t *testing.T) {
	description := strings.Repeat("x", canonicaljson.MaxInputBytes/2+1024)
	raw := []byte(`{"description":"` + description + `"}`)
	if len(raw) >= canonicaljson.MaxInputBytes || len(raw)*2 <= canonicaljson.MaxInputBytes {
		t.Fatalf("test schema sizes do not straddle aggregate bound: %d", len(raw))
	}
	hash, err := canonicaljson.HexSHA256(raw)
	if err != nil {
		t.Fatalf("hash individual schema: %v", err)
	}
	snapshot := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		Endpoints: []fusedobject.Endpoint{
			hashFixtureEndpoint(uuid.New(), raw, hash, nil),
			hashFixtureEndpoint(uuid.New(), raw, hash, nil),
		},
	}
	if _, err := serviceContractHash(snapshot); err != nil {
		t.Fatalf("serviceContractHash rejected bounded schemas in a large aggregate: %v", err)
	}
}

func hashFixtureEndpoint(id uuid.UUID, raw []byte, contentHash string, extensionRaw []byte) fusedobject.Endpoint {
	endpoint := fusedobject.Endpoint{
		ID: id,
		Responses: fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{
			MediaType: "application/json",
			Schema:    &fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw, ContentHash: contentHash},
		}}}},
	}
	if extensionRaw != nil {
		endpoint.Documentation = &fusedobject.OperationDocumentation{Extensions: fusedobject.NamespacedExtensions{
			"x-fused-test": {Value: extensionRaw, Provenance: "source"},
		}}
	}
	return endpoint
}

func TestPrepareServiceContractSnapshotRejectsIncompatibleEnvelope(t *testing.T) {
	snapshot := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{ContractVersion: 3, RequiredCapabilities: []string{}},
		ServiceID:                 uuid.New(),
		ServiceVersionID:          uuid.New(),
		Version:                   "2026-08-11",
	}
	if _, _, err := prepareServiceContractSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), fusedobject.ExecutionCapabilityRequiredCode) {
		t.Fatalf("prepareServiceContractSnapshot() error = %v", err)
	}
}

func TestPrepareServiceContractSnapshotCanonicalizesCapabilityHashInput(t *testing.T) {
	snapshot := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{
			ContractVersion: fusedobject.CurrentExecutionContractVersion,
			RequiredCapabilities: []string{
				fusedobject.ExecutionCapabilityRetryPolicyV3,
				fusedobject.ExecutionCapabilityHTTPDigestV1,
				fusedobject.ExecutionCapabilityRetryPolicyV3,
			},
		},
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Version: "2026-08-11",
	}
	prepared, _, err := prepareServiceContractSnapshot(snapshot)
	if err != nil {
		t.Fatalf("prepareServiceContractSnapshot: %v", err)
	}
	want := []string{fusedobject.ExecutionCapabilityHTTPDigestV1, fusedobject.ExecutionCapabilityRetryPolicyV3}
	if !reflect.DeepEqual(prepared.RequiredCapabilities, want) {
		t.Fatalf("canonical snapshot capabilities = %#v, want %#v", prepared.RequiredCapabilities, want)
	}
	snapshot.RequiredCapabilities = want
	wantHash, err := serviceContractHash(snapshot)
	if err != nil || prepared.ContractHash != wantHash {
		t.Fatalf("canonical capability hash = %q, want %q, err %v", prepared.ContractHash, wantHash, err)
	}
}

func TestPrepareServiceContractSnapshotRejectsMismatchedSchemaHash(t *testing.T) {
	invalid := mustHashFixtureSchema(t, []byte(`{"type":"object"}`))
	invalid.ContentHash = strings.Repeat("0", 64)
	snapshot := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 uuid.New(), ServiceVersionID: uuid.New(), Version: "2026-08-11",
		Endpoints: []fusedobject.Endpoint{{ID: uuid.New(), Responses: fusedobject.Responses{"200": {
			Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/json", Schema: invalid}},
		}}}},
	}
	if _, _, err := prepareServiceContractSnapshot(snapshot); err == nil {
		t.Fatal("prepareServiceContractSnapshot accepted a mismatched schema hash")
	}
}

// TestValidateServiceContractSnapshotRejectsChildConflictsBeforePersistence proves preflight sees child-table conflicts without a transaction.
func TestValidateServiceContractSnapshotRejectsChildConflictsBeforePersistence(t *testing.T) {
	snapshot := serviceContractBatchFixture(1)
	snapshot.Webhooks = append(snapshot.Webhooks, snapshot.Webhooks[0])
	// Duplicate immutable child identity must fail at validate-only admission rather than at PostgreSQL commit.
	if _, err := ValidateServiceContractSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("ValidateServiceContractSnapshot() error = %v", err)
	}
}

// TestValidateServiceContractSnapshotDoesNotWriteBeforeUpsert exercises the validate-only and persistence phases against one database.
func TestValidateServiceContractSnapshotDoesNotWriteBeforeUpsert(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	// Integration coverage never guesses a developer database when no isolated test DSN was supplied.
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	// Schema initialization must succeed before observing whether validation wrote anything.
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()
	snapshotStore := NewPostgresStore(pool).(*postgresStore)
	snapshot := serviceContractBatchFixture(1)
	defer deleteServiceContractFixture(t, pool, snapshot.ServiceVersionID)

	validated, err := ValidateServiceContractSnapshot(snapshot)
	// A valid fixture must reach the no-write assertion with a computed hash.
	if err != nil {
		t.Fatalf("ValidateServiceContractSnapshot: %v", err)
	}
	// Validate-only admission must leave the local snapshot table untouched.
	if count := countServiceContractSnapshotRows(t, ctx, pool, snapshot.ServiceVersionID); count != 0 {
		t.Fatalf("snapshot rows after validate-only admission = %d", count)
	}
	saved, err := snapshotStore.UpsertServiceContractSnapshot(ctx, validated)
	// The same admitted value must remain acceptable at the transactional boundary.
	if err != nil {
		t.Fatalf("UpsertServiceContractSnapshot: %v", err)
	}
	persistedRows := countServiceContractSnapshotRows(t, ctx, pool, snapshot.ServiceVersionID)
	// Persistence must retain the exact hash produced by the shared validate-only path.
	if saved.ContractHash != validated.ContractHash || persistedRows != 1 {
		t.Fatalf("persisted contract hash=%q rows=%d, want hash=%q rows=1", saved.ContractHash, persistedRows, validated.ContractHash)
	}
}

// countServiceContractSnapshotRows reads one exact immutable version so admission tests never filter database state in Go.
func countServiceContractSnapshotRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, serviceVersionID uuid.UUID) int {
	t.Helper()
	var count int
	// SQL performs the exact-version filter so the test observes only its own fixture.
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_service_contract_snapshots WHERE service_version_id = $1`, serviceVersionID).Scan(&count); err != nil {
		t.Fatalf("count service contract snapshots: %v", err)
	}
	return count
}

func TestValidatePersistedExecutionContractUsesStableClassification(t *testing.T) {
	err := validatePersistedExecutionContract(fusedobject.ExecutionContractEnvelope{
		ContractVersion:      fusedobject.CurrentExecutionContractVersion,
		RequiredCapabilities: []string{"http.future.v1"},
	})
	details, ok := fusedobject.ExecutionContractCompatibilityDetails(err)
	if !ok || details.Reason != fusedobject.ExecutionContractReasonUnsupportedCapability || !strings.Contains(err.Error(), fusedobject.ExecutionCapabilityRequiredCode) {
		t.Fatalf("persisted compatibility error = %#v", err)
	}
	if strings.Contains(err.Error(), "http.future.v1") {
		t.Fatalf("persisted compatibility error leaked untrusted capability: %v", err)
	}
}

type serviceContractWriteTracer struct {
	mu     sync.Mutex
	counts map[string]int
}

func newServiceContractWriteTracer() *serviceContractWriteTracer {
	return &serviceContractWriteTracer{counts: make(map[string]int)}
}

func (tracer *serviceContractWriteTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := serviceContractWriteStatement(data.SQL)
	if statement == "" {
		return ctx
	}
	tracer.mu.Lock()
	tracer.counts[statement]++
	tracer.mu.Unlock()
	return ctx
}

func (*serviceContractWriteTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tracer *serviceContractWriteTracer) reset() {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	tracer.counts = make(map[string]int)
}

func (tracer *serviceContractWriteTracer) snapshot() map[string]int {
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	out := make(map[string]int, len(tracer.counts))
	for statement, count := range tracer.counts {
		out[statement] = count
	}
	return out
}

func serviceContractWriteStatement(query string) string {
	switch {
	case strings.Contains(query, "INSERT INTO fused_service_contract_snapshots"):
		return "snapshot_upsert"
	case strings.Contains(query, "DELETE FROM fused_service_contract_endpoints"):
		return "endpoint_delete"
	case strings.Contains(query, "INSERT INTO fused_service_contract_endpoints"):
		return "endpoint_insert"
	case strings.Contains(query, "DELETE FROM fused_service_contract_webhooks"):
		return "webhook_delete"
	case strings.Contains(query, "INSERT INTO fused_service_contract_webhooks"):
		return "webhook_insert"
	default:
		return ""
	}
}

func TestPostgresStoreServiceContractSnapshotBatchQueryCount(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()

	tracer := newServiceContractWriteTracer()
	tracedPool := openServiceContractTracedPool(t, ctx, pool, tracer)
	defer tracedPool.Close()
	tracedStore := NewPostgresStore(tracedPool).(*postgresStore)
	readStore := NewPostgresStore(pool).(*postgresStore)

	for _, rowCount := range []int{1, 100, 1000} {
		t.Run(fmt.Sprintf("rows_%d", rowCount), func(t *testing.T) {
			snapshot := serviceContractBatchFixture(rowCount)
			defer deleteServiceContractFixture(t, pool, snapshot.ServiceVersionID)
			tracer.reset()
			if _, err := tracedStore.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
				t.Fatalf("UpsertServiceContractSnapshot(%d): %v", rowCount, err)
			}
			assertServiceContractWriteFormula(t, tracer.snapshot(), rowCount, rowCount)
			assertServiceContractBatchRoundTrip(t, ctx, readStore, pool, snapshot)
		})
	}

	t.Run("late batch failure rolls back the whole snapshot", func(t *testing.T) {
		assertServiceContractBatchRollback(t, ctx, readStore, pool)
	})
}

func TestPostgresStoreServiceContractSnapshotAllocationMatrix(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()
	store := NewPostgresStore(pool).(*postgresStore)

	for _, rowCount := range []int{1, 100, 1000} {
		snapshot := serviceContractBatchFixture(rowCount)
		defer deleteServiceContractFixture(t, pool, snapshot.ServiceVersionID)
		allocations := serviceContractUpsertAllocs(t, ctx, store, snapshot)
		t.Logf("snapshot allocations rows=%d allocations=%.0f", rowCount, allocations)
	}

	selected := serviceContractBatchFixture(100)
	defer deleteServiceContractFixture(t, pool, selected.ServiceVersionID)
	before := serviceContractUpsertAllocs(t, ctx, store, selected)
	irrelevant := serviceContractBatchFixture(1000)
	defer deleteServiceContractFixture(t, pool, irrelevant.ServiceVersionID)
	if _, err := store.UpsertServiceContractSnapshot(ctx, irrelevant); err != nil {
		t.Fatalf("seed irrelevant snapshot rows: %v", err)
	}
	after := serviceContractUpsertAllocs(t, ctx, store, selected)
	assertServiceContractAllocationTolerance(t, before, after)
	t.Logf("snapshot irrelevant-row allocations=%.0f/%.0f", before, after)
}

func serviceContractUpsertAllocs(t *testing.T, ctx context.Context, store *postgresStore, snapshot ServiceContractSnapshot) float64 {
	t.Helper()
	var upsertErr error
	allocations := testing.AllocsPerRun(3, func() {
		if upsertErr != nil {
			return
		}
		_, upsertErr = store.UpsertServiceContractSnapshot(ctx, snapshot)
	})
	if upsertErr != nil {
		t.Fatalf("measured service contract upsert: %v", upsertErr)
	}
	return allocations
}

func assertServiceContractAllocationTolerance(t *testing.T, before, after float64) {
	t.Helper()
	const tolerance = 0.10
	if after > before*(1+tolerance) {
		t.Fatalf("snapshot allocations grew %.2f%%, tolerance %.0f%%", serviceContractAllocationGrowth(before, after), tolerance*100)
	}
}

func serviceContractAllocationGrowth(before, after float64) float64 {
	if before == 0 {
		return 0
	}
	return (after - before) / before * 100
}

func openServiceContractTracedPool(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	tracer *serviceContractWriteTracer,
) *pgxpool.Pool {
	t.Helper()
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open traced database pool: %v", err)
	}
	return tracedPool
}

func serviceContractBatchFixture(rowCount int) ServiceContractSnapshot {
	serviceID, versionID := uuid.New(), uuid.New()
	endpoints := make([]fusedobject.Endpoint, rowCount)
	webhooks := make([]fusedobject.Webhook, rowCount)
	for index := range rowCount {
		endpoints[index] = fusedobject.Endpoint{
			ID: uuid.New(), Name: fmt.Sprintf("endpoint-%04d", index), Method: "GET", Path: fmt.Sprintf("/items/%04d", index),
		}
		webhooks[index] = fusedobject.Webhook{
			ID: uuid.New(), Name: fmt.Sprintf("webhook-%04d", index), Method: "POST", Description: fmt.Sprintf("event %04d", index),
		}
	}
	return ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 serviceID,
		ServiceVersionID:          versionID,
		Version:                   "2026-08-11",
		ServiceMetadata: fusedobject.ServiceMetadata{
			ID: serviceID, ServiceVersionID: versionID, Name: fmt.Sprintf("Batch %d", rowCount),
		},
		Endpoints: endpoints,
		Webhooks:  webhooks,
	}
}

func assertServiceContractWriteFormula(t *testing.T, got map[string]int, endpointCount, webhookCount int) {
	t.Helper()
	want := map[string]int{
		"snapshot_upsert": 1,
		"endpoint_delete": 1,
		"endpoint_insert": serviceContractBatchCount(endpointCount),
		"webhook_delete":  1,
		"webhook_insert":  serviceContractBatchCount(webhookCount),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot DML counts = %v, want %v", got, want)
	}
}

func serviceContractBatchCount(rowCount int) int {
	return (rowCount + serviceContractSnapshotWriteBatchSize - 1) / serviceContractSnapshotWriteBatchSize
}

func assertServiceContractBatchRoundTrip(
	t *testing.T,
	ctx context.Context,
	store *postgresStore,
	pool *pgxpool.Pool,
	snapshot ServiceContractSnapshot,
) {
	t.Helper()
	operations, err := store.ListServiceContractOperations(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	if err != nil {
		t.Fatalf("ListServiceContractOperations: %v", err)
	}
	if len(operations) != len(snapshot.Endpoints) {
		t.Fatalf("operation count = %d, want %d", len(operations), len(snapshot.Endpoints))
	}
	for index, operation := range operations {
		if operation.Name != fmt.Sprintf("endpoint-%04d", index) {
			t.Fatalf("operation %d name = %q", index, operation.Name)
		}
	}
	var webhookCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM fused_service_contract_webhooks webhooks
		JOIN fused_service_contract_snapshots snapshots ON snapshots.id = webhooks.snapshot_id
		WHERE snapshots.service_version_id = $1`, snapshot.ServiceVersionID).Scan(&webhookCount); err != nil {
		t.Fatalf("count persisted webhooks: %v", err)
	}
	if webhookCount != len(snapshot.Webhooks) {
		t.Fatalf("webhook count = %d, want %d", webhookCount, len(snapshot.Webhooks))
	}
}

func assertServiceContractBatchRollback(t *testing.T, ctx context.Context, store *postgresStore, pool *pgxpool.Pool) {
	t.Helper()
	snapshot := serviceContractBatchFixture(1)
	defer deleteServiceContractFixture(t, pool, snapshot.ServiceVersionID)
	saved, err := store.UpsertServiceContractSnapshot(ctx, snapshot)
	if err != nil {
		t.Fatalf("seed rollback snapshot: %v", err)
	}
	originalEndpoint := snapshot.Endpoints[0]
	originalWebhook := snapshot.Webhooks[0]

	replacement := snapshot
	replacement.ServiceMetadata.Name = "must roll back"
	replacement.Endpoints = []fusedobject.Endpoint{{ID: uuid.New(), Name: "replacement", Method: "POST", Path: "/replacement"}}
	replacement.Webhooks = []fusedobject.Webhook{
		{ID: uuid.New(), Name: "duplicate", Method: "POST"},
		{ID: uuid.New(), Name: "duplicate", Method: "POST"},
	}
	if _, err := store.UpsertServiceContractSnapshot(ctx, replacement); err == nil {
		t.Fatal("duplicate webhook names did not fail the replacement")
	}
	assertServiceContractRollbackState(t, ctx, store, pool, snapshot, saved.ContractHash, originalEndpoint, originalWebhook)
}

func assertServiceContractRollbackState(
	t *testing.T,
	ctx context.Context,
	store *postgresStore,
	pool *pgxpool.Pool,
	snapshot ServiceContractSnapshot,
	contractHash string,
	endpoint fusedobject.Endpoint,
	webhook fusedobject.Webhook,
) {
	t.Helper()
	metadata, err := store.GetServiceContractMetadata(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	if err != nil || metadata.Name != snapshot.ServiceMetadata.Name {
		t.Fatalf("metadata after rollback = %#v, err %v", metadata, err)
	}
	operations, err := store.ListServiceContractOperations(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	if err != nil || len(operations) != 1 || operations[0].ID != endpoint.ID {
		t.Fatalf("endpoints after rollback = %#v, err %v", operations, err)
	}
	var storedHash, webhookName string
	if err := pool.QueryRow(ctx, `
		SELECT snapshots.contract_hash, webhooks.name
		FROM fused_service_contract_snapshots snapshots
		JOIN fused_service_contract_webhooks webhooks ON webhooks.snapshot_id = snapshots.id
		WHERE snapshots.service_version_id = $1`, snapshot.ServiceVersionID).Scan(&storedHash, &webhookName); err != nil {
		t.Fatalf("read rollback hash and webhook: %v", err)
	}
	if storedHash != contractHash || webhookName != webhook.Name {
		t.Fatalf("rollback state hash=%q webhook=%q, want hash=%q webhook=%q", storedHash, webhookName, contractHash, webhook.Name)
	}
}

func deleteServiceContractFixture(t *testing.T, pool *pgxpool.Pool, serviceVersionID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM fused_service_contract_snapshots WHERE service_version_id = $1`, serviceVersionID); err != nil {
		t.Errorf("delete service contract fixture: %v", err)
	}
}

func TestPostgresStoreServiceContractSnapshotRoundTrip(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool).(*postgresStore)
	fixture := newContractRoundTripFixture(t)
	defer pool.Exec(context.Background(), `DELETE FROM fused_service_contract_snapshots WHERE service_version_id = $1`, fixture.snapshot.ServiceVersionID) //nolint:errcheck
	assertInitialContractRoundTrip(t, ctx, s, fixture)
	updatedEndpoint := assertContractReplacement(t, ctx, s, fixture.snapshot)
	secondServiceID, secondVersionID, secondEndpoint := insertSecondContractSnapshot(t, ctx, s)
	defer pool.Exec(context.Background(), `DELETE FROM fused_service_contract_snapshots WHERE service_version_id = $1`, secondVersionID) //nolint:errcheck
	assertContractSelectionQueries(t, ctx, s, fixture.snapshot, updatedEndpoint, secondServiceID, secondVersionID, secondEndpoint)
}

type contractRoundTripFixture struct {
	query    string
	endpoint fusedobject.Endpoint
	snapshot ServiceContractSnapshot
}

func newContractRoundTripFixture(t *testing.T) contractRoundTripFixture {
	serviceID, versionID := uuid.New(), uuid.New()
	query := `query ListWidgets { widgets { id } }`
	endpoint := postgresContractFixtureEndpoint(t, query)
	return contractRoundTripFixture{query: query, endpoint: endpoint, snapshot: ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 serviceID, ServiceVersionID: versionID, Version: "2026-07-23",
		ServiceMetadata: fusedobject.ServiceMetadata{
			ID: serviceID, ServiceVersionID: versionID, Name: "Widgets", BaseURL: "https://api.example.com",
			Documentation: &fusedobject.ServiceDocumentation{Tags: []fusedobject.TagDocumentation{{
				Name: "orders", Summary: "Order operations", Description: "Order APIs",
				Parent: "commerce", Kind: "badge",
			}}},
		},
		Endpoints: []fusedobject.Endpoint{endpoint},
		Webhooks:  []fusedobject.Webhook{{ID: uuid.New(), Name: "widget.created", Method: "POST"}},
	}}
}

func assertInitialContractRoundTrip(t *testing.T, ctx context.Context, store *postgresStore, fixture contractRoundTripFixture) {
	t.Helper()
	saved, err := store.UpsertServiceContractSnapshot(ctx, fixture.snapshot)
	if err != nil {
		t.Fatalf("UpsertServiceContractSnapshot: %v", err)
	}
	assertSavedContractEnvelope(t, saved, fixture.snapshot)
	assertSavedContractMetadata(t, ctx, store, fixture.snapshot)
	gotEndpoint, err := store.GetServiceContractEndpointByName(ctx, fixture.snapshot.ServiceID, fixture.snapshot.ServiceVersionID, "listWidgets")
	if err != nil {
		t.Fatalf("GetServiceContractEndpointByName: %v", err)
	}
	assertPersistedEndpointIdentity(t, fixture.endpoint, *gotEndpoint, fixture.query)
	assertPersistedEndpointContract(t, fixture.endpoint, *gotEndpoint)
	assertRichContractSelectionQueries(t, ctx, store, fixture.snapshot, fixture.endpoint)
	reloaded := fixture.snapshot
	reloaded.Endpoints = []fusedobject.Endpoint{*gotEndpoint}
	reloadedHash, err := serviceContractHash(reloaded)
	if err != nil || reloadedHash != saved.ContractHash {
		t.Fatalf("persisted contract hash = %q, want %q, err %v", reloadedHash, saved.ContractHash, err)
	}
}

func assertRichContractSelectionQueries(t *testing.T, ctx context.Context, store *postgresStore, snapshot ServiceContractSnapshot, expected fusedobject.Endpoint) {
	t.Helper()
	operations, err := store.ListServiceContractOperations(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	assertRichContractSelection(t, operations, err, expected)
	byName, err := store.ListServiceContractEndpointsByNames(ctx, snapshot.ServiceID, snapshot.ServiceVersionID, []string{expected.Name})
	assertRichContractSelection(t, byName, err, expected)
	byID, err := store.ListServiceContractEndpointsByIDs(ctx, snapshot.ServiceID, snapshot.ServiceVersionID, []uuid.UUID{expected.ID})
	assertRichContractSelection(t, byID, err, expected)
	matches, err := store.ListServiceContractEndpointsForSelections(ctx, []ServiceContractEndpointSelection{{
		SelectionIndex: 0, ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID, SelectAll: true,
	}}, []string{expected.Name})
	if err != nil || len(matches) != 1 {
		t.Fatalf("rich selection intersection = %#v, err %v", matches, err)
	}
	assertPersistedEndpointContract(t, expected, matches[0].Endpoint)
}

func assertRichContractSelection(t *testing.T, endpoints []fusedobject.Endpoint, err error, expected fusedobject.Endpoint) {
	t.Helper()
	if err != nil || len(endpoints) != 1 {
		t.Fatalf("rich endpoint selection = %#v, err %v", endpoints, err)
	}
	assertPersistedEndpointContract(t, expected, endpoints[0])
}

func assertSavedContractEnvelope(t *testing.T, saved *ServiceContractSnapshot, expected ServiceContractSnapshot) {
	t.Helper()
	if saved.ContractVersion != expected.ContractVersion || !slices.Equal(saved.RequiredCapabilities, expected.RequiredCapabilities) {
		t.Fatalf("execution contract envelope did not round-trip: %#v", saved.ExecutionContractEnvelope)
	}
}

func assertSavedContractMetadata(t *testing.T, ctx context.Context, store *postgresStore, snapshot ServiceContractSnapshot) {
	t.Helper()
	metadata, err := store.GetServiceContractMetadata(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	if err != nil {
		t.Fatalf("GetServiceContractMetadata: %v", err)
	}
	if metadata.Name != "Widgets" || metadata.ServiceVersionID != snapshot.ServiceVersionID {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if metadata.ContractVersion != snapshot.ContractVersion || !slices.Equal(metadata.RequiredCapabilities, snapshot.RequiredCapabilities) {
		t.Fatalf("metadata lost its validated execution envelope: %#v", metadata.ExecutionContractEnvelope)
	}
	assertSavedTagHierarchy(t, metadata.Documentation)
}

func assertSavedTagHierarchy(t *testing.T, documentation *fusedobject.ServiceDocumentation) {
	t.Helper()
	if documentation == nil || len(documentation.Tags) != 1 {
		t.Fatalf("metadata lost its tag hierarchy: %#v", documentation)
	}
	tag := documentation.Tags[0]
	if tag.Summary != "Order operations" || tag.Parent != "commerce" || tag.Kind != "badge" {
		t.Fatalf("metadata lost tag hierarchy fields: %#v", tag)
	}
}

func assertPersistedEndpointIdentity(t *testing.T, expected, actual fusedobject.Endpoint, query string) {
	t.Helper()
	if actual.ID != expected.ID || actual.GraphQLQuery == nil || *actual.GraphQLQuery != query {
		t.Fatalf("unexpected endpoint identity: %#v", actual)
	}
	if actual.ProviderProtocol != expected.ProviderProtocol || actual.OperationKind != expected.OperationKind {
		t.Fatalf("unexpected endpoint protocol: %#v", actual)
	}
}

func assertContractReplacement(t *testing.T, ctx context.Context, store *postgresStore, snapshot ServiceContractSnapshot) fusedobject.Endpoint {
	t.Helper()
	snapshot.ServiceMetadata.Name = "Widgets v2"
	updated := fusedobject.Endpoint{ID: uuid.New(), Name: "createWidget", Method: "POST", Path: "/widgets"}
	snapshot.Endpoints = []fusedobject.Endpoint{updated}
	if _, err := store.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("UpsertServiceContractSnapshot replacement: %v", err)
	}
	operations, err := store.ListServiceContractOperations(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	if err != nil || len(operations) != 1 || operations[0].Name != updated.Name {
		t.Fatalf("replacement operations = %#v, err %v", operations, err)
	}
	byName, err := store.ListServiceContractEndpointsByNames(ctx, snapshot.ServiceID, snapshot.ServiceVersionID, []string{updated.Name, "missingWidget"})
	if err != nil || len(byName) != 1 || byName[0].ID != updated.ID {
		t.Fatalf("SQL-filtered name lookup = %#v, err %v", byName, err)
	}
	if _, err := store.GetServiceContractEndpointByName(ctx, snapshot.ServiceID, snapshot.ServiceVersionID, "listWidgets"); err != ErrServiceContractEndpointNotFound {
		t.Fatalf("expected old endpoint removed, got %v", err)
	}
	return updated
}

// insertSecondContractSnapshot persists a different service/version with the
// same operation name to catch cross-snapshot name matching.
func insertSecondContractSnapshot(t *testing.T, ctx context.Context, store *postgresStore) (uuid.UUID, uuid.UUID, fusedobject.Endpoint) {
	t.Helper()
	serviceID, versionID := uuid.New(), uuid.New()
	endpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "listGadgets", Method: "GET", Path: "/gadgets"}
	unrelated := fusedobject.Endpoint{ID: uuid.New(), Name: "deleteGadget", Method: "DELETE", Path: "/gadgets/{id}"}
	_, err := store.UpsertServiceContractSnapshot(ctx, ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 serviceID, ServiceVersionID: versionID, Version: "2026-07-24",
		ServiceMetadata: fusedobject.ServiceMetadata{ID: serviceID, ServiceVersionID: versionID, Name: "Gadgets"},
		Endpoints:       []fusedobject.Endpoint{endpoint, unrelated},
	})
	if err != nil {
		t.Fatalf("UpsertServiceContractSnapshot second selection: %v", err)
	}
	return serviceID, versionID, endpoint
}

// assertContractSelectionQueries proves mixed select-all and exact-ID requests
// stay isolated by service version even when operation names collide.
func assertContractSelectionQueries(t *testing.T, ctx context.Context, store *postgresStore, snapshot ServiceContractSnapshot, updated fusedobject.Endpoint, secondServiceID, secondVersionID uuid.UUID, second fusedobject.Endpoint) {
	t.Helper()
	metadata, err := store.ListServiceContractMetadata(ctx, []ServiceContractMetadataRef{
		{ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID},
		{ServiceID: secondServiceID, ServiceVersionID: secondVersionID},
	})
	firstMetadata := metadata[ServiceContractMetadataRef{ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID}]
	if err != nil || len(metadata) != 2 || firstMetadata == nil || firstMetadata.Name != "Widgets v2" {
		t.Fatalf("batched metadata = %#v, err %v", metadata, err)
	}
	selections := []ServiceContractEndpointSelection{
		{SelectionIndex: 0, ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID, EndpointIDs: []uuid.UUID{updated.ID}},
		{SelectionIndex: 1, ServiceID: secondServiceID, ServiceVersionID: secondVersionID, SelectAll: true},
	}
	intersection, err := store.ListServiceContractEndpointsForSelections(ctx, selections, []string{updated.Name, second.Name, "missingWidget"})
	if err != nil {
		t.Fatalf("ListServiceContractEndpointsForSelections: %v", err)
	}
	assertContractIntersection(t, intersection, updated, second)
	perSelection := append([]ServiceContractEndpointSelection(nil), selections...)
	perSelection[0].EndpointNames = []string{updated.Name}
	perSelection[1].EndpointNames = []string{second.Name}
	bound, err := store.ListServiceContractEndpointsForSelections(ctx, perSelection, nil)
	if err != nil {
		t.Fatalf("per-selection endpoint lookup: %v", err)
	}
	assertContractIntersection(t, bound, updated, second)
	unrestricted, err := store.ListServiceContractEndpointsForSelections(ctx, selections, nil)
	if err != nil || len(unrestricted) != 3 {
		t.Fatalf("batched unrestricted app-scope intersection = %#v, %v", unrestricted, err)
	}
	assertMissingContractSelection(t, ctx, store, updated.Name)
	byID, err := store.ListServiceContractEndpointsByIDs(ctx, snapshot.ServiceID, snapshot.ServiceVersionID, []uuid.UUID{updated.ID, uuid.New()})
	if err != nil || len(byID) != 1 || byID[0].Name != updated.Name {
		t.Fatalf("SQL-filtered ID lookup = %#v, err %v", byID, err)
	}
}

func assertContractIntersection(t *testing.T, values []ServiceContractEndpointMatch, first, second fusedobject.Endpoint) {
	t.Helper()
	if len(values) != 2 || values[0].SelectionIndex != 0 || values[0].Endpoint.ID != first.ID {
		t.Fatalf("first batched intersection result = %#v", values)
	}
	if values[1].SelectionIndex != 1 || values[1].Endpoint.ID != second.ID {
		t.Fatalf("second batched intersection result = %#v", values)
	}
}

func assertMissingContractSelection(t *testing.T, ctx context.Context, store *postgresStore, endpointName string) {
	t.Helper()
	_, err := store.ListServiceContractEndpointsForSelections(ctx, []ServiceContractEndpointSelection{{
		SelectionIndex: 0, ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true,
	}}, []string{endpointName})
	if !errors.Is(err, ErrServiceContractSnapshotNotFound) {
		t.Fatalf("missing strict snapshot error = %v, want %v", err, ErrServiceContractSnapshotNotFound)
	}
}

func mustHashFixtureSchema(t *testing.T, raw []byte) *fusedobject.SchemaContract {
	t.Helper()
	hash, err := canonicaljson.HexSHA256(raw)
	if err != nil {
		t.Fatalf("hash fixture schema: %v", err)
	}
	return &fusedobject.SchemaContract{
		Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw, ContentHash: hash,
	}
}

func postgresContractFixtureEndpoint(t *testing.T, query string) fusedobject.Endpoint {
	return fusedobject.Endpoint{
		ID: uuid.New(), Name: "listWidgets", Method: "POST", Path: "/graphql", NormalizedPath: "/graphql",
		GraphQLQuery: &query, ProviderProtocol: "graphql", OperationKind: "query",
		OperationServers: fusedobject.Servers{{URL: "https://api.example.com", Name: "production", IsDefault: true}},
		Parameters: fusedobject.Parameters{{Name: "filter", In: "query", Schema: mustHashFixtureSchema(t,
			[]byte(`{"type":"object","maxProperties":1.0}`))}},
		RequestContent: &fusedobject.RequestContent{DefaultMediaType: "application/json", Representations: []fusedobject.RequestRepresentation{
			{MediaType: "application/json", Schema: mustHashFixtureSchema(t, []byte(`{"type":"object","maxProperties":1.0}`))},
			{MediaType: "application/x-ndjson", ItemSchema: mustHashFixtureSchema(t, []byte(`{"type":"string","minLength":1.0}`))},
			{MediaType: "multipart/form-data", Schema: mustHashFixtureSchema(t, []byte(`{"type":"object","required":["payload"]}`)),
				Encoding: map[string]fusedobject.RequestEncoding{"payload": {ContentType: "application/json", Headers: map[string]fusedobject.HeaderContract{
					"X-Part": {Schema: mustHashFixtureSchema(t, []byte(`{"type":"integer","minimum":1.0}`))},
				}}}},
		}},
		Responses: fusedobject.Responses{"200": {
			Headers: map[string]fusedobject.HeaderContract{"X-Trace": {Schema: mustHashFixtureSchema(t, []byte(`{"type":"string","minLength":1.0}`))}},
			Representations: []fusedobject.ResponseRepresentation{
				{MediaType: "application/json", Schema: mustHashFixtureSchema(t, []byte(`{"type":"object","maximum":1.0}`))},
				{MediaType: "text/event-stream", ItemSchema: mustHashFixtureSchema(t, []byte(`{"type":"object","minimum":1.0}`))},
			},
		}},
		Documentation: &fusedobject.OperationDocumentation{Tags: []string{"widgets", "read"}, Extensions: fusedobject.NamespacedExtensions{
			"x-fused-test": {Value: []byte(`{"z":1.0,"a":true}`), Provenance: "source"},
		}},
	}
}

func assertPersistedEndpointContract(t *testing.T, expected, actual fusedobject.Endpoint) {
	t.Helper()
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	equal, err := canonicaljson.Equal(expectedJSON, actualJSON)
	if err != nil || !equal {
		t.Fatalf("persisted endpoint semantic equality = %v, err %v", equal, err)
	}
	expectedSchemas, actualSchemas := contractFixtureSchemas(expected), contractFixtureSchemas(actual)
	for index := range expectedSchemas {
		assertPersistedSchemaHash(t, index, expectedSchemas[index], actualSchemas[index])
	}
}

func contractFixtureSchemas(endpoint fusedobject.Endpoint) []*fusedobject.SchemaContract {
	multipartHeader := endpoint.RequestContent.Representations[2].Encoding["payload"].Headers["X-Part"]
	response := endpoint.Responses["200"]
	return []*fusedobject.SchemaContract{
		endpoint.Parameters[0].Schema,
		endpoint.RequestContent.Representations[0].Schema,
		endpoint.RequestContent.Representations[1].ItemSchema,
		endpoint.RequestContent.Representations[2].Schema,
		multipartHeader.Schema,
		response.Headers["X-Trace"].Schema,
		response.Representations[0].Schema,
		response.Representations[1].ItemSchema,
	}
}

func assertPersistedSchemaHash(t *testing.T, index int, expected, actual *fusedobject.SchemaContract) {
	t.Helper()
	equal, err := canonicaljson.Equal(expected.Raw, actual.Raw)
	if err != nil || !equal {
		t.Fatalf("persisted schema %d semantic equality = %v, err %v", index, equal, err)
	}
	hash, err := canonicaljson.HexSHA256(actual.Raw)
	if err != nil || hash != actual.ContentHash || actual.ContentHash != expected.ContentHash {
		t.Fatalf("persisted schema %d hash invariant = %q/%q/%q, err %v", index, hash, actual.ContentHash, expected.ContentHash, err)
	}
}
