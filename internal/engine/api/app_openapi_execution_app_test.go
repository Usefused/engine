package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type appOpenAPIBundleFixture struct {
	*appOpenAPITestStore
	bundle *store.UnifiedAppBundle
}

// CreateUnifiedAppBundle is unavailable because schema export reads only immutable bundle state.
func (fixture *appOpenAPIBundleFixture) CreateUnifiedAppBundle(context.Context, store.UnifiedAppBundle) error {
	return errors.New("bundle fixture is read only")
}

// GetUnifiedAppBundle returns only the exact fixture version's persisted artifact.
func (fixture *appOpenAPIBundleFixture) GetUnifiedAppBundle(_ context.Context, appID uuid.UUID) (*store.UnifiedAppBundle, error) {
	// Another version must never receive the fixture's authored schema.
	if fixture.bundle == nil || fixture.bundle.AppID != appID {
		return nil, store.ErrUnifiedAppBundleNotFound
	}
	copy := *fixture.bundle
	return &copy, nil
}

// newAppOpenAPIBundleFixture models a planned version with one singular authored execute contract.
func newAppOpenAPIBundleFixture(t *testing.T) *appOpenAPIBundleFixture {
	t.Helper()
	base, _ := newAppOpenAPIFixture(t)
	bundle := &store.UnifiedAppBundle{
		AppID: base.app.AppID, SourceHash: "sha256:authored-source", BundleJS: "compiled script",
		Manifest: json.RawMessage(`{"schemaVersion":1,"inputSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},"outputSchema":{"type":"object","properties":{"greeting":{"type":"string"}},"required":["greeting"]},"searchable":[],"selectedOperations":[{"service":"issues","operation":"createIssue","serviceId":"11111111-1111-4111-8111-111111111111","serviceVersionId":"22222222-2222-4222-8222-222222222222","endpointId":"33333333-3333-4333-8333-333333333333"}]}`),
	}
	base.app.SourceHash = bundle.SourceHash
	base.app.BundleDigest = store.UnifiedAppBundleDigest([]byte(bundle.BundleJS))
	base.family.Kind = store.AppKindUnifiedApp
	return &appOpenAPIBundleFixture{appOpenAPITestStore: base, bundle: bundle}
}

// TestAppOpenAPIExportsAuthoredExecuteAlongsideRawOperation keeps one REST route and both exact schemas.
func TestAppOpenAPIExportsAuthoredExecuteAlongsideRawOperation(t *testing.T) {
	fixture := newAppOpenAPIBundleFixture(t)
	document, err := buildAppOpenAPIDocumentWithBundle(context.Background(), fixture, fixture, fixture.app, fixture.family, "")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	// The authored operation must retain typed input/output while the original raw branch remains present.
	request := openAPITestOperationRequest(t, decoded, "execute")
	input := request["properties"].(map[string]any)["input"].(map[string]any)
	if _, ok := input["properties"].(map[string]any)["name"]; !ok {
		t.Fatal("authored input schema is missing name")
	}
	components := decoded["components"].(map[string]any)["schemas"].(map[string]any)
	security := decoded["components"].(map[string]any)["securitySchemes"].(map[string]any)
	// The distinct app kind must not claim to issue SDK tokens.
	if _, ok := security["UnifiedAppToken"]; !ok {
		t.Fatal("Unified App bearer scheme is unavailable")
	}
	response := components[openAPIOperationComponentKey("execute")+"Response"].(map[string]any)
	output := response["properties"].(map[string]any)["output"].(map[string]any)
	if _, ok := output["properties"].(map[string]any)["greeting"]; !ok {
		t.Fatal("authored output schema is missing greeting")
	}
	// Generated contracts must not promise the private search document as response data.
	if _, ok := response["properties"].(map[string]any)["data"]; ok {
		t.Fatal("authored response schema exposes stored search data")
	}
	openAPITestOperationRequest(t, decoded, "createIssue")
}

// TestAppOpenAPIExportsFilteredExecute keeps a bound app's selected raw operation out of one filtered export.
func TestAppOpenAPIExportsFilteredExecute(t *testing.T) {
	fixture := newAppOpenAPIBundleFixture(t)
	document, err := buildAppOpenAPIDocumentWithBundle(context.Background(), fixture, fixture, fixture.app, fixture.family, "execute")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	openAPITestOperationRequest(t, decoded, "execute")
	// The filter is pushed into the immutable provider lookup while retaining only execute in the document.
	if len(fixture.queryNames) != 1 || fixture.queryNames[0] != "execute" || decoded["x-fused-operation-count"] != float64(1) {
		t.Fatalf("filtered export query=%#v count=%#v", fixture.queryNames, decoded["x-fused-operation-count"])
	}
}

// TestSDKOpenAPIExcludesAuthoredExecute preserves the SDK's raw-only export when a fixture has bundle bytes.
func TestSDKOpenAPIExcludesAuthoredExecute(t *testing.T) {
	fixture := newAppOpenAPIBundleFixture(t)
	fixture.family.Kind = store.AppKindSDK
	document, err := buildAppOpenAPIDocumentWithBundle(context.Background(), fixture, fixture, fixture.app, fixture.family, "")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	components := decoded["components"].(map[string]any)["schemas"].(map[string]any)
	// Only the selected physical SDK operation remains in the generated contract.
	if _, exists := components[openAPIOperationComponentKey("execute")+"Request"]; exists {
		t.Fatal("SDK export acquired authored execute")
	}
	openAPITestOperationRequest(t, decoded, "createIssue")
}
