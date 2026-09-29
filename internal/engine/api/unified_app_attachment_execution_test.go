package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type attachedExecutionFixture struct {
	*capabilityRouteStore
	target  store.AppRuntime
	binding models.UnifiedAppBinding
}

// GetAppRuntime exposes only the selected target to catch accidental consumer-scope execution.
func (f *attachedExecutionFixture) GetAppRuntime(_ context.Context, id uuid.UUID) (*store.AppRuntime, error) {
	// Arbitrary aliases must never widen the target lookup.
	if id != f.target.AppID {
		return nil, store.ErrAppNotFound
	}
	return &f.target, nil
}

// ResolveUnifiedAppBindings is unused by execution but satisfies the same storage contract as planning.
func (f *attachedExecutionFixture) ResolveUnifiedAppBindings(context.Context, uuid.UUID, map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error) {
	return []models.UnifiedAppBinding{f.binding}, nil
}

// ReadUnifiedAppBindings supplies the immutable consumer's reviewed delegation.
func (f *attachedExecutionFixture) ReadUnifiedAppBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.UnifiedAppBinding, error) {
	return []models.UnifiedAppBinding{f.binding}, nil
}

// TestUnifiedAppAttachmentExecution records real authored execution under the target with consumer-token attribution.
func TestUnifiedAppAttachmentExecution(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	runtime := &restRuntimeTestDouble{}
	server, targetID := newRESTPhysicalServer(runtime)
	base := server.store.(*grpcRuntimeStore)
	familyID := uuid.New()
	bundleJS := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},async execute({input}){return {value:input.value}}};`
	bundle := store.UnifiedAppBundle{AppID: targetID, SourceHash: "sha256:test", BundleJS: bundleJS, Manifest: json.RawMessage(`{"schemaVersion":1,"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"searchable":[],"selectedOperations":[{"service":"crm","operation":"createIssue","serviceId":"11111111-1111-4111-8111-111111111111","serviceVersionId":"22222222-2222-4222-8222-222222222222","endpointId":"33333333-3333-4333-8333-333333333333"}]}`)}
	// Match real compiler output so the resident worker can inspect the manifest before accepting invocations.
	bundle.BundleJS = "globalThis.FusedExecutionManifest=" + string(bundle.Manifest) + ";" + bundleJS
	digest := store.UnifiedAppBundleDigest([]byte(bundle.BundleJS))
	fixture := &attachedExecutionFixture{capabilityRouteStore: &capabilityRouteStore{Store: base, bundle: bundle, activeAppID: targetID}, target: store.AppRuntime{AppID: targetID, AccountID: base.accountID, AppFamilyID: familyID, Kind: store.AppKindUnifiedApp, Status: store.AppStatusActive, Version: "1.0.0", BundleDigest: digest}, binding: models.UnifiedAppBinding{Alias: "lookup", AppID: targetID, AppFamilyID: familyID, SourceHash: bundle.SourceHash, BundleDigest: digest}}
	server.store = fixture
	tokenID := uuid.New()
	consumer := auth.RuntimeIdentity{AccountID: base.accountID, AppID: uuid.New(), AppFamilyID: uuid.New(), TokenID: tokenID, Kind: store.AppKindSDK, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	result, err := server.ExecuteAttachedUnifiedApp(context.Background(), consumer, "lookup", json.RawMessage(`{"value":"works"}`))
	// A successful call must use the durable executor, including its terminal output and trace identity.
	if err != nil {
		t.Fatal(err)
	}
	var envelope capabilityExecutionEnvelope
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "succeeded" || string(envelope.Output) != `{"value":"works"}` || fixture.record.AppID != targetID || fixture.record.AppTokenID != tokenID {
		t.Fatalf("result=%s record=%+v", result, fixture.record)
	}
	fixture.activeAppID = uuid.New()
	// Promotion revokes new traffic to the old pinned target without replaying the call.
	if _, err := server.ExecuteAttachedUnifiedApp(context.Background(), consumer, "lookup", json.RawMessage(`{}`)); err == nil {
		t.Fatal("promoted-away target executed")
	}
}
