package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestExecuteResolvedPhysicalJSONAccountsCollectorFailureOnce protects the rule
// that response-validation failures finalize exactly one physical audit and usage outcome.
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
	previousUsage := globalExecutionUsageRecorder
	usageCapture := &captureExecutionUsageRecorder{}
	SetExecutionUsageRecorder(usageCapture)
	t.Cleanup(func() { SetExecutionUsageRecorder(previousUsage) })

	_, err := ExecuteResolvedPhysicalJSON(context.Background(), engine.NewDispatcher(), identity, operation, PhysicalExecutionRequest{
		IdempotencyKey: "child-key", RequestBodyHash: "strict-body-hash",
	})
	if !errors.Is(err, ErrPhysicalResponseNotJSON) {
		t.Fatalf("ExecuteResolvedPhysicalJSON() error = %v, want ErrPhysicalResponseNotJSON", err)
	}
	assertPhysicalFailureAudit(t, auditCapture, identity, operation)
	if len(usageCapture.increments) != 2 || usageCapture.increments[0].Metric != models.EngineUsageMetricExecutionTotal || usageCapture.increments[1].Metric != models.EngineUsageMetricExecutionFailed {
		t.Fatalf("physical usage increments = %#v", usageCapture.increments)
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
