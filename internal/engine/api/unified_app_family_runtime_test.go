package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestUnifiedAppFamilyURLRunsPromotedCode exercises worker replacement behind an unchanged caller URL.
func TestUnifiedAppFamilyURLRunsPromotedCode(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	router, fixture, familyID := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	first := invokeCapabilityRouteForTest(t, router, fixture, familyID)
	oldID := fixture.record.AppID
	promoteCapabilityFixture(fixture)
	fixture.bundle.BundleJS = strings.Replace(fixture.bundle.BundleJS, "return {value:input.value}", `return {value:"promoted"}`, 1)
	res := performRESTExecution(t, fixture.server, familyID, "fsk_test", `{"operation":"execute","input":{"value":"Jane"}}`, "")
	var second capabilityExecutionEnvelope
	err := json.Unmarshal(res.Body.Bytes(), &second)
	// Output must come from the replacement bundle while the caller-facing ID remains stable.
	if err != nil || res.Code != http.StatusOK || second.Status != "succeeded" || string(second.Output) != `{"value":"promoted"}` || second.AppFamilyID != first.AppFamilyID || second.Version != "2.0.0" || fixture.record.AppID == oldID {
		t.Fatalf("promoted execution status=%d body=%s error=%v", res.Code, res.Body.String(), err)
	}
}

// UnifiedAppTrafficTarget models the stable family identity independently from immutable versions.
func (fixture *capabilityRouteStore) UnifiedAppTrafficTarget(_ context.Context, familyID uuid.UUID) (uuid.UUID, error) {
	// Exact version IDs and unrelated families cannot select this deployment pointer.
	if familyID != fixture.familyID {
		return uuid.Nil, store.ErrAppFamilyNotFound
	}
	return fixture.activeAppID, nil
}

// GetFamilyExecutionResult enforces the same tenant/family scope as production SQL.
func (fixture *capabilityRouteStore) GetFamilyExecutionResult(ctx context.Context, accountID, familyID, executionID uuid.UUID) (*store.ExecutionResult, error) {
	record, err := fixture.GetExecutionResult(ctx, accountID, uuid.Nil, executionID)
	// A valid handle cannot make another family's record visible.
	if err != nil || record.AccountID != accountID || record.AppFamilyID != familyID {
		return nil, store.ErrExecutionResultNotFound
	}
	return record, nil
}

// promoteCapabilityFixture replaces code identity while preserving the caller's family and credential.
func promoteCapabilityFixture(fixture *capabilityRouteStore) {
	base := fixture.Store.(*grpcRuntimeStore)
	base.appID = uuid.New()
	base.scope.AppID, base.scope.Version = base.appID, "2.0.0"
	fixture.activeAppID, fixture.bundle.AppID = base.appID, base.appID
	validator := fixture.server.tokenValidator.(appTestValidator)
	validator.identity.AppID, validator.identity.AppVersion = base.appID, "2.0.0"
	fixture.server.tokenValidator = validator
}

// TestUnifiedAppFamilyURLFollowsPromotion proves one URL and token resolve new code identities per request.
func TestUnifiedAppFamilyURLFollowsPromotion(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	router, fixture, familyID := newCapabilityRouteFixture(runtime, nil)
	first := fixture.activeAppID
	for _, version := range []string{"1.0.0", "2.0.0"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/apps/"+familyID.String()+"/executions", strings.NewReader(`{"operation":"issues.get","input":{"id":7}}`))
		req.Header.Set("Authorization", "Bearer fsk_test")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		// Both calls must dispatch successfully through the same stable endpoint.
		if res.Code != http.StatusOK {
			t.Fatalf("version %s: status=%d body=%s", version, res.Code, res.Body.String())
		}
		scope, identity, requestErr := fixture.server.authenticateRESTEndpoint(req, familyID)
		// Receipts and runtime scope retain the selected exact version, not the URL identity.
		if requestErr != nil || scope.Version != version || identity.AppID != fixture.activeAppID || identity.AppFamilyID != familyID {
			t.Fatalf("version %s identity=%+v scope=%+v error=%v", version, identity, scope, requestErr)
		}
		// The second request observes a new pointer without replacing its URL or token.
		if version == "1.0.0" {
			promoteCapabilityFixture(fixture)
		}
	}
	// Promotion must actually have changed the immutable identity under test.
	if first == fixture.activeAppID || runtime.connects != 2 {
		t.Fatalf("promotion did not dispatch twice: first=%s active=%s connects=%d", first, fixture.activeAppID, runtime.connects)
	}
}

// TestUnifiedAppFamilyRouteFailsClosed covers absent deployments and mismatched token families.
func TestUnifiedAppFamilyRouteFailsClosed(t *testing.T) {
	_, fixture, familyID := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer fsk_test")
	validator := fixture.server.tokenValidator.(appTestValidator)
	validator.identity.AppFamilyID = uuid.New()
	fixture.server.tokenValidator = validator
	_, _, requestErr := fixture.server.authenticateRESTEndpoint(req, familyID)
	// A token for another family cannot inherit the resolved destination.
	if requestErr == nil || requestErr.status != http.StatusForbidden {
		t.Fatalf("cross-family token error=%v", requestErr)
	}
	req.Header.Set("Authorization", "Bearer revoked")
	_, _, requestErr = fixture.server.authenticateRESTEndpoint(req, familyID)
	// Resolution must never bypass the canonical token validator.
	if requestErr == nil || requestErr.status != http.StatusUnauthorized {
		t.Fatalf("revoked token error=%v", requestErr)
	}
	fixture.activeAppID = uuid.Nil
	_, _, requestErr = fixture.server.authenticateRESTEndpoint(req, familyID)
	// No target must not silently select the only retained bundle.
	if requestErr == nil || requestErr.status != http.StatusServiceUnavailable {
		t.Fatalf("cleared deployment error=%v", requestErr)
	}
}

// TestUnifiedAppHistoricalFamilyRead preserves completed receipts after promotion without running old code.
func TestUnifiedAppHistoricalFamilyRead(t *testing.T) {
	router, fixture, familyID := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	handle, hash, err := newExecutionReadHandle()
	// The fixture needs a real read credential to exercise the public authorization path.
	if err != nil {
		t.Fatal(err)
	}
	identity := fixture.server.tokenValidator.(appTestValidator).identity
	record := store.ExecutionResult{ID: uuid.New(), AccountID: identity.AccountID, AppFamilyID: familyID, AppID: identity.AppID, AppVersion: "1.0.0", ReadHandleHash: hash, Status: "succeeded"}
	fixture.records = map[uuid.UUID]store.ExecutionResult{record.ID: record}
	promoteCapabilityFixture(fixture)
	req := httptest.NewRequest(http.MethodGet, "/v1/apps/"+familyID.String()+"/executions/"+record.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer fsk_test")
	req.Header.Set("X-Execution-Read-Handle", handle)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	// The stable response exposes family identity and historical version, never an invocation version ID.
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"version":"1.0.0"`) || !strings.Contains(res.Body.String(), familyID.String()) || strings.Contains(res.Body.String(), record.AppID.String()) {
		t.Fatalf("historical read status=%d body=%s", res.Code, res.Body.String())
	}
	// Reusing a valid handle for a record from another family must still fail.
	record.AppFamilyID = uuid.New()
	fixture.records[record.ID] = record
	res = httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("cross-family read status=%d body=%s", res.Code, res.Body.String())
	}
}
