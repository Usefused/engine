package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/google/uuid"
)

type namedCapabilityStore struct {
	*capabilityRouteStore
	name string
}

// ResolveUnifiedAppName models the SQL token/name boundary while leaving version authorization real in the handler.
func (s *namedCapabilityStore) ResolveUnifiedAppName(_ context.Context, name, hash string) (uuid.UUID, error) {
	digest := sha256.Sum256([]byte("fsk_test"))
	canonicalName, _, err := canonical.AppName(s.name)
	// A token for a different family cannot discover this app, even when its name matches.
	if err != nil || name != canonicalName || hash != hex.EncodeToString(digest[:]) {
		return uuid.Nil, store.ErrAppFamilyNotFound
	}
	return s.familyID, nil
}

// namedExecutionRequest exercises real path escaping and routing rather than injecting a pre-decoded route parameter.
func namedExecutionRequest(router http.Handler, name, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+url.PathEscape(name)+"/executions", strings.NewReader(`{"operation":"issues.get","input":{"id":7}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// TestUnifiedAppNameExecutionFollowsPromotion preserves canonical names, URL escaping and per-request traffic selection.
func TestUnifiedAppNameExecutionFollowsPromotion(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	router, fixture, _ := newCapabilityRouteFixture(runtime, nil)
	names := &namedCapabilityStore{capabilityRouteStore: fixture, name: "Café: Billing/100%"}
	fixture.server.store = names
	for _, name := range []string{"CAFÉ: BILLING/100%", "Cafe\u0301: Billing/100%"} {
		response := namedExecutionRequest(router, name, "fsk_test")
		// Canonical spelling changes must retain the exact same family while the active version can change.
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), fixture.familyID.String()) {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		promoteCapabilityFixture(fixture)
	}
	// Both calls must actually reach runtime, not only pass an isolated resolver check.
	if runtime.connects != 2 {
		t.Fatalf("dispatch count=%d", runtime.connects)
	}
}

// TestUnifiedAppNameExecutionRejectsWrongAuthority proves lookup cannot bypass token validation or select a fallback version.
func TestUnifiedAppNameExecutionRejectsWrongAuthority(t *testing.T) {
	router, fixture, _ := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	fixture.server.store = &namedCapabilityStore{capabilityRouteStore: fixture, name: "billing"}
	for _, test := range []struct {
		name, token string
		status      int
	}{
		{"billing", "", http.StatusUnauthorized}, {"billing", "foreign", http.StatusUnauthorized},
		{"unknown", "fsk_test", http.StatusUnauthorized}, {uuid.Nil.String(), "fsk_test", http.StatusBadRequest},
		{"billing\x00", "fsk_test", http.StatusBadRequest},
	} {
		response := namedExecutionRequest(router, test.name, test.token)
		// Lookup failures remain bounded and never reveal another app's identity.
		if response.Code != test.status {
			t.Fatalf("%q: status=%d body=%s", test.name, response.Code, response.Body.String())
		}
	}
	fixture.activeAppID = uuid.Nil
	response := namedExecutionRequest(router, "billing", "fsk_test")
	// A cleared deployment cannot resurrect a retained version through its name.
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cleared target: %d %s", response.Code, response.Body.String())
	}
}

// TestUnifiedAppNameHistoricalResultRequiresReadHandle retains family reads after promotion without loosening receipt authorization.
func TestUnifiedAppNameHistoricalResultRequiresReadHandle(t *testing.T) {
	router, fixture, familyID := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	fixture.server.store = &namedCapabilityStore{capabilityRouteStore: fixture, name: "billing"}
	handle, hash, err := newExecutionReadHandle()
	// The retained receipt must have a genuine digest to exercise read-handle validation.
	if err != nil {
		t.Fatal(err)
	}
	identity := fixture.server.tokenValidator.(appTestValidator).identity
	record := store.ExecutionResult{ID: uuid.New(), AccountID: identity.AccountID, AppFamilyID: familyID, AppID: identity.AppID, AppVersion: "1.0.0", ReadHandleHash: hash, Status: "succeeded"}
	fixture.records = map[uuid.UUID]store.ExecutionResult{record.ID: record}
	promoteCapabilityFixture(fixture)
	for _, test := range []struct {
		handle string
		status int
	}{{handle, http.StatusOK}, {"wrong", http.StatusNotFound}} {
		request := httptest.NewRequest(http.MethodGet, "/v1/apps/billing/executions/"+record.ID.String(), nil)
		request.Header.Set("Authorization", "Bearer fsk_test")
		request.Header.Set("X-Execution-Read-Handle", test.handle)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		// Names must preserve the same success and denial outcomes as family-ID result URLs.
		if response.Code != test.status {
			t.Fatalf("result status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
