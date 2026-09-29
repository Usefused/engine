package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/trace"
)

type sdkQueuePublisher struct{ count atomic.Int64 }

// PublishMsgJS counts concurrent receipts without sharing mutable payloads across callers.
func (publisher *sdkQueuePublisher) PublishMsgJS(*nats.Msg) (*nats.PubAck, error) {
	publisher.count.Add(1)
	return &nats.PubAck{}, nil
}

// recordSDKQueuePeak keeps the observed provider high-water mark monotonic across handlers.
func recordSDKQueuePeak(peak *atomic.Int64, current int64) {
	for {
		previous := peak.Load()
		// A newer handler may already have established this or a higher peak.
		if current <= previous || peak.CompareAndSwap(previous, current) {
			return
		}
	}
}

// awaitSDKQueueResults requires every admitted child to finish within its caller deadline.
func awaitSDKQueueResults(t *testing.T, ctx context.Context, results <-chan error) {
	t.Helper()
	for range 8 {
		select {
		case err := <-results:
			// A transferred slot must lead to a successful physical completion.
			if err != nil {
				t.Fatalf("queued SDK call failed: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("SDK physical queue did not drain")
		}
	}
}

// TestExecuteResolvedPhysicalJSONAccountsCollectorFailureOnce protects the one durable receipt used for downstream accounting.
func TestExecuteResolvedPhysicalJSONAccountsCollectorFailureOnce(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`not-json`))
	}))
	defer vendor.Close()

	identity, operation := physicalExecutionTestOperation(vendor.URL)
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(5)})
	activeExecutions.Delete(identity.AccountID)
	t.Cleanup(func() { activeExecutions.Delete(identity.AccountID) })

	auditCapture := &captureJetStreamPublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(auditCapture))
	t.Cleanup(func() { executionevent.SetPublisher(nil) })

	_, err := ExecuteResolvedPhysicalJSON(context.Background(), engine.NewDispatcher(), identity, operation, PhysicalExecutionRequest{
		IdempotencyKey: "child-key", RequestBodyHash: "strict-body-hash",
	})
	if !errors.Is(err, ErrPhysicalResponseNotJSON) {
		t.Fatalf("ExecuteResolvedPhysicalJSON() error = %v, want ErrPhysicalResponseNotJSON", err)
	}
	assertPhysicalFailureAudit(t, auditCapture, identity, operation)
}

// TestSDKPhysicalQueueRunsEightAtFourSlots proves the resolved SDK
// boundary waits through account saturation without exceeding the plan limit.
func TestSDKPhysicalQueueRunsEightAtFourSlots(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(4)})
	audit := &sdkQueuePublisher{}
	executionevent.SetPublisher(executionevent.NewPublisher(audit))
	t.Cleanup(func() { executionevent.SetPublisher(nil) })
	releaseProvider := make(chan struct{})
	var releaseOnce sync.Once
	var active, peak, calls atomic.Int64
	vendor := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		calls.Add(1)
		recordSDKQueuePeak(&peak, current)
		// Keep the first four provider calls active so the next four must enter the account queue.
		<-releaseProvider
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer func() {
		// Release blocked handlers before closing the test server, including on assertion failure.
		releaseOnce.Do(func() { close(releaseProvider) })
		vendor.Close()
	}()
	identity, operation := physicalExecutionTestOperation(vendor.URL)
	operation.match.endpoint.Responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/json"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := ExecuteResolvedPhysicalJSON(ctx, engine.NewDispatcher(), identity, operation, PhysicalExecutionRequest{})
			results <- err
		}()
	}
	waitForExecutionWaiters(t, identity.AccountID, 4)
	// Queue admission can precede HTTP dispatch, so wait until all four granted slots reach the provider.
	for calls.Load() < 4 {
		// A stalled provider path must end the test through its bounded context.
		if ctx.Err() != nil {
			t.Fatal("four admitted calls did not reach the provider")
		}
		time.Sleep(time.Millisecond)
	}
	releaseOnce.Do(func() { close(releaseProvider) })
	awaitSDKQueueResults(t, ctx, results)
	// All callers must reach the provider, but none may exceed the four-slot plan.
	if got := calls.Load(); got != 8 {
		t.Fatalf("provider calls = %d, want 8", got)
	}
	if got := peak.Load(); got != 4 {
		t.Fatalf("peak provider concurrency = %d, want 4", got)
	}
	// Shared queue admission still produces one canonical physical receipt per call.
	if got := audit.count.Load(); got != 8 {
		t.Fatalf("physical execution receipts = %d, want 8", got)
	}
}

// TestSDKPhysicalQueueKeepsMCPImmediate checks that the same SDK-kind
// app does not queue when the Engine's MCP adapter owns the call.
func TestSDKPhysicalQueueKeepsMCPImmediate(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(1)})
	identity, operation := physicalExecutionTestOperation("https://provider.invalid")
	hold, err := trackAuthenticatedExecution(context.Background(), identity, trace.SpanFromContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	_, err = ExecuteResolvedPhysicalJSON(context.Background(), engine.NewDispatcher(), identity, operation, PhysicalExecutionRequest{Transport: models.EngineExecutionTransportMCP})
	var denied *entitlement.LimitExceeded
	// MCP retains immediate rejection while the sole account slot is occupied.
	if !errors.As(err, &denied) {
		t.Fatalf("MCP call error = %v, want sandbox concurrency denial", err)
	}
}

// TestValidateResolvedPhysicalPaginationIntentUsesExactOperation proves admission reads the resolved endpoint policy without provider work.
func TestValidateResolvedPhysicalPaginationIntentUsesExactOperation(t *testing.T) {
	_, operation := physicalExecutionTestOperation("https://provider.invalid")
	operation.match.endpoint.Pagination = testCursorPagination("cursor", "$.next")

	if err := ValidateResolvedPhysicalPaginationIntent(operation, &engine.PaginationIntent{MaxPages: 1}); err != nil {
		t.Fatalf("strict pagination intent error = %v", err)
	}
	operation.match.endpoint.Pagination = nil
	if !errors.Is(ValidateResolvedPhysicalPaginationIntent(operation, &engine.PaginationIntent{MaxPages: 1}), engine.ErrPaginationIntentInvalid) {
		t.Fatal("non-paginated operation accepted a pagination intent")
	}
}

// TestPreparePhysicalExecutionContextBindsPaginationOnce proves replay identity changes at the shared physical boundary.
func TestPreparePhysicalExecutionContextBindsPaginationOnce(t *testing.T) {
	intent := &engine.PaginationIntent{MaxPages: 2}
	ctx := preparePhysicalExecutionContext(context.Background(), PhysicalExecutionRequest{
		RequestBodyHash: "base-hash",
		Pagination:      intent,
	})

	wantHash := engine.BindPaginationIntentRequestHash("base-hash", intent)
	if got := requestBodyHashFromContext(ctx); got != wantHash {
		t.Fatalf("request hash = %q, want %q", got, wantHash)
	}
	gotIntent, ok := engine.PaginationIntentFromContext(ctx)
	if !ok || gotIntent.MaxPages != 2 {
		t.Fatalf("pagination intent = %+v, found %t", gotIntent, ok)
	}
}

// physicalExecutionTestOperation builds an exact app-scoped operation that still
// traverses the production dispatcher, authorization, and accounting boundary.
func physicalExecutionTestOperation(providerURL string) (auth.RuntimeIdentity, ResolvedPhysicalOperation) {
	appID, accountID := uuid.New(), uuid.New()
	serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	service := &fusedobject.ServiceMetadata{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ID:                        serviceID, ServiceVersionID: versionID, BaseURL: providerURL,
	}
	endpoint := fusedobject.Endpoint{
		ID: endpointID, Name: "items.get", Method: http.MethodGet,
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}},
	}
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, EndpointIDs: []uuid.UUID{endpointID}}
	identity := auth.RuntimeIdentity{
		AccountID: accountID, AppFamilyID: uuid.New(), AppID: appID, AppVersion: "1.0.0",
		Kind: store.AppKindSDK, Status: store.AppStatusActive, TokenPolicy: store.AppTokenPolicy{AllowAll: true},
	}
	match := &scopedEndpoint{service: service, endpoint: endpoint, allowed: true, serviceVersionID: versionID.String(), selection: selection}
	return identity, ResolvedPhysicalOperation{appID: appID, match: match}
}

// assertPhysicalFailureAudit proves collector rejection retains the physical
// identity and replay hashes without recording a successful execution.
func assertPhysicalFailureAudit(t *testing.T, capture *captureJetStreamPublisher, identity auth.RuntimeIdentity, operation ResolvedPhysicalOperation) {
	t.Helper()
	if capture.message == nil {
		t.Fatal("physical execution audit was not published")
	}
	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(capture.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	event := envelope.Event
	if event.AccountID != identity.AccountID || event.AppID != identity.AppID || event.OperationID != operation.match.endpoint.ID {
		t.Fatalf("physical audit identity = %#v", event)
	}
	if event.Status != models.EngineExecutionStatusFailed || event.IdempotencyKeyHash == "" || event.RequestBodyHash != "strict-body-hash" {
		t.Fatalf("physical audit accounting = %#v", event)
	}
}
