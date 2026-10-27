package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/capability"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type bridgeOpenAPIFixture struct {
	*appOpenAPITestStore
	bindings []models.BridgeBinding
	account  uuid.UUID
	reads    int
}

// ResolveBridgeBindings models the account-scoped immutable manifest lookup used during planning.
func (s *bridgeOpenAPIFixture) ResolveBridgeBindings(_ context.Context, account uuid.UUID, _ map[string]models.BridgeReference) ([]models.BridgeBinding, error) {
	s.account = account
	return s.bindings, nil
}

// ReadBridgeBindings supplies only the previously published bindings to the production export path.
func (s *bridgeOpenAPIFixture) ReadBridgeBindings(_ context.Context, account uuid.UUID, _ uuid.UUID) ([]models.BridgeBinding, error) {
	s.account = account
	s.reads++
	return s.bindings, nil
}

// newBridgeOpenAPIFixture includes local references and scalar output to catch invalid schema relocation or envelopes.
func newBridgeOpenAPIFixture(t *testing.T) (*bridgeOpenAPIFixture, accesscontrol.Actor) {
	t.Helper()
	base, _ := newAppOpenAPIFixture(t)
	actor := registryPolicyActor(t, accesscontrol.PermissionBridgeManage)
	binding := models.BridgeBinding{Alias: "echo_device", Kind: "read", WorkspaceID: actor.WorkspaceID.String(),
		BridgeReference: models.BridgeReference{PairingID: uuid.NewString(), ProfileID: "echo-live", ConnectionID: "speaker", OperationID: "device.info", ManifestHash: "sha256:manifest"}, ContractHash: "sha256:contract",
		InputSchema: json.RawMessage(`{"$ref":"#/$defs/input","$defs":{"input":{"type":"object","properties":{"name":{"type":"string"}}}}}`), OutputSchema: json.RawMessage(`{"type":"boolean"}`)}
	base.app.Selections = []byte(`[]`)
	base.app.AccountID = actor.AccountID
	base.family.AccountID = actor.AccountID
	keys, hash, err := capability.KeysAndHashWithBridges(base.app.Selections, []models.BridgeBinding{binding})
	require.NoError(t, err)
	base.app.CapabilityKeys, base.app.CapabilityHash = keys, hash
	return &bridgeOpenAPIFixture{appOpenAPITestStore: base, bindings: []models.BridgeBinding{binding}}, actor
}

// TestBridgeOnlyDirectAPIPlanAndExport exercises the exact admission and production-export paths that rejected the live app.
func TestBridgeOnlyDirectAPIPlanAndExport(t *testing.T) {
	s, actor := newBridgeOpenAPIFixture(t)
	s.contractErr = errors.New("Bridge-only apps must not query provider schema storage")
	generate := false
	doc := sdkConfigDocument{Kind: "sdk", Name: "echo-live", Version: "1.0.0", Generate: &generate, Bridges: map[string]models.BridgeReference{"echo_device": s.bindings[0].BridgeReference}}
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	require.NoError(t, validateDirectAPIOpenAPIPlan(ctx, s, doc, uuid.Nil, nil))
	require.Equal(t, actor.AccountID, s.account)
	encoded, err := buildStoredAppOpenAPIDocument(ctx, s, s, nil, s.app, s.family, "")
	require.NoError(t, err)
	require.Equal(t, 1, s.reads)
	parsed, err := openapi3.NewLoader().LoadFromData(encoded)
	require.NoError(t, err)
	require.NoError(t, parsed.Validate(ctx))
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	request := openAPITestOperationRequest(t, document, "bridge:echo_device")
	properties := request["properties"].(map[string]any)
	require.NotContains(t, properties, "selector")
	require.NotContains(t, properties, "pagination")
	schemas := document["components"].(map[string]any)["schemas"].(map[string]any)
	response := schemas[openAPIOperationComponentKey("bridge:echo_device")+"Response"].(map[string]any)
	require.Contains(t, response["properties"].(map[string]any)["results"].(map[string]any)["items"], "$ref")
	require.Contains(t, string(encoded), `"type":"boolean"`)
}

// TestBridgeOpenAPIFiltersAndFailures keeps mixed exports complete and excludes event-only or unselected operations.
func TestBridgeOpenAPIFiltersAndFailures(t *testing.T) {
	s, _ := newBridgeOpenAPIFixture(t)
	physical, _ := newAppOpenAPIFixture(t)
	s.app.Selections = physical.app.Selections
	s.matches = physical.matches
	encoded, err := buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "", s.bindings...)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "createIssue")
	require.Contains(t, string(encoded), "bridge:echo_device")
	encoded, err = buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "bridge:echo_device", s.bindings...)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "createIssue")
	_, err = buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "bridge:unselected", s.bindings...)
	require.Error(t, err)
	s.app.Selections = []byte(`[]`)
	s.bindings[0].Kind = "event"
	_, err = buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "", s.bindings...)
	require.Error(t, err)
	s.bindings[0].Kind = "read"
	s.bindings[0].InputSchema = []byte(`{"$ref":"#/$defs/missing"}`)
	_, err = buildAppOpenAPIDocument(context.Background(), s, s.app, s.family, "", s.bindings...)
	require.Error(t, err)
	s.bindings = nil
	_, err = storedOpenAPIBridgeBindings(context.Background(), s, s.app)
	require.Error(t, err)
}

type bridgePlanRouteFixture struct {
	*generationPlanningTestStore
	bindings []models.BridgeBinding
}

// ResolveBridgeBindings returns the already admitted manifest contracts for the HTTP plan regression.
func (s *bridgePlanRouteFixture) ResolveBridgeBindings(context.Context, uuid.UUID, map[string]models.BridgeReference) ([]models.BridgeBinding, error) {
	return s.bindings, nil
}

// ReadBridgeBindings satisfies the shared store contract while the plan path resolves references independently.
func (s *bridgePlanRouteFixture) ReadBridgeBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.BridgeBinding, error) {
	return s.bindings, nil
}

// TestBridgeOnlyDirectAPIHTTPPlan preserves a full plan receipt through all production handler admission gates.
func TestBridgeOnlyDirectAPIHTTPPlan(t *testing.T) {
	schemas, _ := newBridgeOpenAPIFixture(t)
	s := &bridgePlanRouteFixture{generationPlanningTestStore: newGenerationPlanningTestStore(), bindings: schemas.bindings}
	actor := controlTestOwnerActor(s.accountID)
	s.bindings[0].WorkspaceID = actor.WorkspaceID.String()
	s.bindings[0].ManifestHash = "sha256:" + strings.Repeat("a", 64)
	configs := &mockConfigStore{}
	body, err := json.Marshal(map[string]any{"source_hash": "bridge-test", "config_key": "sdk:echo:1.0.0", "owner_team": "platform", "config": map[string]any{
		"apiVersion": "fused/v1", "kind": "sdk", "name": "echo", "version": "1.0.0", "language": "typescript", "generate": false, "bucket": "default", "bridges": map[string]models.BridgeReference{"echo_device": s.bindings[0].BridgeReference},
	}})
	require.NoError(t, err)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body)).WithContext(ctx)
	response := httptest.NewRecorder()
	registry := &generationPlanningRegistryTrap{RegistryClient: &mockRegistryClient{}, t: t}
	SDKConfigPlanHandler(configs, s, registry)(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotNil(t, configs.createdPlan)
	var payload appResolvedPayload
	require.NoError(t, json.Unmarshal(configs.createdPlan.ResolvedPayload, &payload))
	require.Len(t, payload.Bridges, 1)
	require.Equal(t, "echo_device", payload.Bridges[0].Alias)
	require.Empty(t, payload.Selections)
	require.Contains(t, string(configs.createdPlan.RequiredPermissions), "bridge.manage")
	assertBridgeOnlyHTTPApply(t, ctx, s, configs, registry)
}

// ResolveGenerationSelections mirrors the real store's empty physical projection for a Bridge-only plan.
func (s *bridgePlanRouteFixture) ResolveGenerationSelections(context.Context, []models.SDKSelection) ([]models.SDKSelection, error) {
	return nil, nil
}

// assertBridgeOnlyHTTPApply follows the reviewed receipt through every apply gate into the atomic persistence boundary.
func assertBridgeOnlyHTTPApply(t *testing.T, ctx context.Context, s *bridgePlanRouteFixture, configs *mockConfigStore, registry *generationPlanningRegistryTrap) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"plan_id": configs.plan.ID, "source_hash": configs.plan.SourceHash, "skip_token": true})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/apply", bytes.NewReader(body)).WithContext(ContextWithAuthorizedPlanRevision(ctx, configs.plan.Revision))
	response := httptest.NewRecorder()
	SDKConfigApplyHandler(configs, s, nil, registry)(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.True(t, configs.markApplied)
	require.NotNil(t, configs.artifactApply)
	require.Len(t, configs.artifactApply.Scope.Bridges, 1)
	require.JSONEq(t, `[]`, string(configs.artifactApply.Scope.Selections))
}
