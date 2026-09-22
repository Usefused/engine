package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type testOperationClassifier struct {
	calls      int
	name       string
	err        error
	operations []ClassifierOperation
}

// ClassifyOperations captures precisely what may leave the Engine and returns a controlled provider decision.
func (c *testOperationClassifier) ClassifyOperations(_ context.Context, _ string, operations []ClassifierOperation) (string, error) {
	c.calls++
	c.operations = operations
	return c.name, c.err
}

// TestMCPClassifierPreservesLocalLookupAndRejectsScopeExpansion covers the shared modern/legacy discovery path.
func TestMCPClassifierPreservesLocalLookupAndRejectsScopeExpansion(t *testing.T) {
	prior := mcpClassifier
	t.Cleanup(func() { mcpClassifier = prior }) // Restore process wiring so unrelated transport tests remain isolated.
	client := &testOperationClassifier{name: "allowed"}
	mcpClassifier = client
	fixture := &Fixture{Server: FixtureServerMetadata{FusedIntelligentClassifier: true}, Operations: []FixtureOperation{{OperationID: "allowed", Description: "Read permitted data", Method: "GET", Path: "/records", Responses: models.Responses{}}}}
	for _, args := range []map[string]any{{}, {"query": " "}, {"query": 12}, {"query": "intent", "operationId": "allowed"}, {"query": "intent", "operationId": "allowed", "section": "params_schema"}} {
		_, err := runMCPDocumentation(context.Background(), fixture, args)
		if err != nil {
			t.Fatal(err)
		} // All deterministic lookup modes must stay available without provider I/O.
	}
	if client.calls != 0 {
		t.Fatal("local lookup contacted classifier")
	} // Browsing, exact lookup, and malformed queries never disclose data.
	result, err := runMCPDocumentation(context.Background(), fixture, map[string]any{"query": "semantically unrelated words"})
	encoded, _ := json.Marshal(result)
	if err != nil || client.calls != 1 || !strings.Contains(string(encoded), "allowed") {
		t.Fatalf("classified result=%s err=%v calls=%d", encoded, err, client.calls)
	} // Semantic selection must bypass lexical filtering.
	if !reflect.DeepEqual(client.operations, []ClassifierOperation{{OperationName: "allowed", Description: "Read permitted data"}}) {
		t.Fatalf("disclosed catalogue=%+v", client.operations)
	} // No schema, token, or out-of-scope candidate leaves Engine.
	for _, name := range []string{"forbidden", ""} {
		client.name = name
		result, err = runMCPDocumentation(context.Background(), fixture, map[string]any{"query": "intent"})
		if err != nil {
			t.Fatal(err)
		} // Classifier errors are bounded tool failures, not raw transport errors.
		if name == "forbidden" && result["isError"] != true {
			t.Fatal("scope expansion accepted")
		} // A provider cannot introduce a new callable.
		if name == "" && result["isError"] == true {
			t.Fatal("no-match treated as error")
		} // Abstention is successful discovery with no operations.
	}
	client.err = errors.New("secret upstream content")
	result, _ = runMCPDocumentation(context.Background(), fixture, map[string]any{"query": "intent"})
	encoded, _ = json.Marshal(result)
	if result["isError"] != true || strings.Contains(string(encoded), "secret upstream") {
		t.Fatal("upstream error leaked or silently fell back")
	} // No raw provider content or lexical fallback may hide classifier failure.
}

// TestClassifierRegistryClientUsesExistingLicense checks the authenticated wire boundary without a Jev key in Engine.
func TestClassifierRegistryClientUsesExistingLicense(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/engine/fused-intelligent-classifier" || r.Header.Get("Authorization") != "Bearer engine-license" {
			t.Error("wrong Registry auth or route")
		} // Reuse the existing license contract.
		_, _ = w.Write([]byte(`{"provider":"fused-intelligent-classifier","operationName":"allowed"}`))
	}))
	defer server.Close()
	client := &HTTPRegistryClient{endpoint: server.URL + "/graphql", licenseKey: "engine-license", httpClient: server.Client()}
	name, err := client.ClassifyOperations(context.Background(), "intent", []ClassifierOperation{{OperationName: "allowed"}})
	if err != nil || name != "allowed" {
		t.Fatalf("name=%q err=%v", name, err)
	} // The public result carries only an exact operation identity.
}

// TestMCPClassifierDescriptionRetainsUTF8 bounds multilingual metadata without corrupting transport JSON.
func TestMCPClassifierDescriptionRetainsUTF8(t *testing.T) {
	description := classifierDescription(strings.Repeat("界", 200))
	if len(description) > 512 || !strings.HasSuffix(description, "界") {
		t.Fatal("invalid UTF-8 truncation")
	} // Clipping must occur at a complete code point.
}

// TestMCPClassifierBridgeRevalidatesTheSession proves legacy transport search cannot disclose data after revocation.
func TestMCPClassifierBridgeRevalidatesTheSession(t *testing.T) {
	priorClassifier, priorValidator := mcpClassifier, globalTokenValidator
	// Restore the shared runtime wiring after exercising the real private HTTP handler.
	t.Cleanup(func() { mcpClassifier, globalTokenValidator = priorClassifier, priorValidator })
	client := &testOperationClassifier{name: "allowed"}
	mcpClassifier = client
	fixture := &Fixture{Server: FixtureServerMetadata{FusedIntelligentClassifier: true}, Operations: []FixtureOperation{{OperationID: "allowed", Description: "Read permitted data", Method: "GET", Path: "/records", Responses: models.Responses{}}}}
	sessionID := registerTestMCPSession(t, "session-token", fixture)
	sess, _ := lookupMCPSession(sessionID)
	appID := uuid.MustParse(sess.appID)
	sess.tokenID = uuid.New()
	validator := &mcpModernTokenValidator{token: sess.token, identity: auth.RuntimeIdentity{AppID: appID, TokenID: sess.tokenID, Kind: store.AppKindMCP}}
	globalTokenValidator = validator
	request := httptest.NewRequest(http.MethodPost, "/mcp/search", strings.NewReader(`{"query":"intent"}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	response := httptest.NewRecorder()
	mcpSearchHandler(response, request)
	if response.Code != http.StatusOK || client.calls != 1 {
		t.Fatalf("bridge status=%d calls=%d body=%s", response.Code, client.calls, response.Body.String())
	} // An authenticated compatibility child reaches the shared semantic path.
	validator.token = "revoked"
	request = httptest.NewRequest(http.MethodPost, "/mcp/search", strings.NewReader(`{"query":"intent"}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	response = httptest.NewRecorder()
	mcpSearchHandler(response, request)
	if response.Code != http.StatusUnauthorized || client.calls != 1 {
		t.Fatal("revoked session reached classifier")
	} // Revocation must stop paid inference and catalogue transfer together.
}
