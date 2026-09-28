package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type executionCompileSelectionStore struct {
	requests []store.ServiceContractEndpointSelection
	matches  []store.ServiceContractEndpointMatch
	calls    int
}

type executionHTTPTestStore struct{ *workspaceTestStore }

// TestValidateExecutionSourceModePreservesPrecompiled admits existing digest-pinned app authoring without Engine source.
func TestValidateExecutionSourceModePreservesPrecompiled(t *testing.T) {
	doc := sdkConfigDocument{BundleDigest: store.ExecutionAppBundleDigest([]byte("precompiled app"))}
	// The manual attach workflow remains valid while describe sends inline source by default.
	if err := validateExecutionSourceMode(doc); err != nil {
		t.Fatalf("precompiled digest rejected: %v", err)
	}
	// A caller cannot claim both a compiler output and source for one immutable version.
	doc.Source = "export default {}"
	if err := validateExecutionSourceMode(doc); err == nil {
		t.Fatal("source and caller digest were both accepted")
	}
}

// ResolveGenerationSelections models the local name-to-ID projection performed after a reviewed plan.
func (fixture *executionHTTPTestStore) ResolveGenerationSelections(_ context.Context, selections []models.SDKSelection) ([]models.SDKSelection, error) {
	for index := range selections {
		for _, name := range selections[index].OperationNames {
			selections[index].EndpointIDs = append(selections[index].EndpointIDs, uuid.NewSHA1(selections[index].ServiceVersionID, []byte(name)))
		}
	}
	return selections, nil
}

// ListServiceContractEndpointsForSelections records the bounded local snapshot read used for compiler pins.
func (fixture *executionCompileSelectionStore) ListServiceContractEndpointsForSelections(_ context.Context, requests []store.ServiceContractEndpointSelection, _ []string) ([]store.ServiceContractEndpointMatch, error) {
	fixture.calls++
	fixture.requests = requests
	return fixture.matches, nil
}

// TestExecutionCompilerSelectionsPinsExactEndpoint proves the Engine builds its own immutable operation map in one batch.
func TestExecutionCompilerSelectionsPinsExactEndpoint(t *testing.T) {
	serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	fixture := &executionCompileSelectionStore{matches: []store.ServiceContractEndpointMatch{{SelectionIndex: 0, Endpoint: fusedobject.Endpoint{ID: endpointID, Name: "greet"}}}}
	selections := []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: []string{"greet"}}}
	services := []sdkResolvedService{{ServiceID: serviceID, ServiceVersionID: versionID, PublicTarget: "greeting"}}
	pins, err := executionCompilerSelections(context.Background(), fixture, selections, services)
	// The exact endpoint ID is the physical authority that the authored method name alone lacks.
	if err != nil || len(pins) != 1 || pins[0].EndpointID != endpointID.String() || pins[0].Service != "greeting" {
		t.Fatalf("pins/error = %#v/%v", pins, err)
	}
	// One set-based store call protects multi-operation planning from N+1 reads.
	if fixture.calls != 1 || len(fixture.requests) != 1 || fixture.requests[0].ServiceVersionID != versionID {
		t.Fatalf("requests = %#v, calls = %d", fixture.requests, fixture.calls)
	}
}

// TestExecutionCompilerSelectionsRejectsPartialScope prevents a missing local operation from becoming a smaller executable app.
func TestExecutionCompilerSelectionsRejectsPartialScope(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	fixture := &executionCompileSelectionStore{}
	_, err := executionCompilerSelections(context.Background(), fixture, []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: []string{"greet"}}}, []sdkResolvedService{{ServiceID: serviceID, PublicTarget: "greeting"}})
	// A partial result is a revision conflict, not permission to omit a method.
	if err == nil {
		t.Fatal("partial operation scope was admitted")
	}
}

// TestValidateCompiledExecutionPinsRejectsRepeatedBinding requires one-to-one coverage of every reviewed operation.
func TestValidateCompiledExecutionPinsRejectsRepeatedBinding(t *testing.T) {
	first := executionCompilerSelection{Service: "greeting", Operation: "greet", ServiceID: uuid.New().String(), ServiceVersionID: uuid.New().String(), EndpointID: uuid.New().String()}
	second := executionCompilerSelection{Service: "greeting", Operation: "farewell", ServiceID: first.ServiceID, ServiceVersionID: first.ServiceVersionID, EndpointID: uuid.New().String()}
	manifest := map[string]any{"schemaVersion": 1, "inputSchema": map[string]any{"type": "object"}, "outputSchema": map[string]any{"type": "object"}, "searchable": []string{}, "selectedOperations": []executionCompilerSelection{first, first}}
	raw, _ := json.Marshal(manifest)
	// Repeating one pin cannot replace the other even when manifest count is unchanged.
	if err := validateCompiledExecutionPins(raw, []executionCompilerSelection{first, second}); err == nil {
		t.Fatal("duplicate compiler pin was admitted")
	}
}

// TestRunExecutionCompilerProducesPlanArtifact exercises Engine-owned TypeScript compilation without a CLI bundle.
func TestRunExecutionCompilerProducesPlanArtifact(t *testing.T) {
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	pins := []executionCompilerSelection{{Service: "greeting", Operation: "greet", ServiceID: uuid.New().String(), ServiceVersionID: uuid.New().String(), EndpointID: uuid.New().String()}}
	source := `import * as z from "zod/mini";
import { buildExecutionApp } from "@fused/execution";
export default buildExecutionApp({
  input: z.object({ name: z.string() }),
  output: z.object({ greeting: z.string() }),
  fetch: { searchable: ["name"] },
  async execute({ input }) { return { greeting: input.name }; },
});`
	artifact, err := runExecutionCompiler(context.Background(), source, pins)
	// The compiler must produce a digest-pinned script and its public contract from source alone.
	if err != nil || artifact == nil || artifact.Digest != store.ExecutionAppBundleDigest([]byte(artifact.BundleJS)) {
		t.Fatalf("artifact/error = %#v/%v", artifact, err)
	}
	var manifest executionAppManifest
	// The selected method retains the exact ID supplied by the Engine snapshot.
	if err := json.Unmarshal(artifact.Manifest, &manifest); err != nil || len(manifest.SelectedOperations) != 1 || manifest.SelectedOperations[0].EndpointID.String() != pins[0].EndpointID {
		t.Fatalf("manifest/error = %#v/%v", manifest, err)
	}
	// The plan artifact must be small enough for durable immutable storage.
	if len(artifact.BundleJS) > 2<<20 || !strings.Contains(artifact.BundleJS, "FusedExecutionManifest") {
		t.Fatalf("bundle length/content = %d", len(artifact.BundleJS))
	}
}

// TestExecutionConfigHTTPPlanCompilesInlineSource checks the public cart path and retained apply artifact.
func TestExecutionConfigHTTPPlanCompilesInlineSource(t *testing.T) {
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	serviceID, versionID := uuid.New(), uuid.New()
	workspace := &executionHTTPTestStore{&workspaceTestStore{accountID: uuid.New(), workspaceID: uuid.New(), workspaceServices: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "greeting", Version: "1.0"}}, workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "1.0"}}}}}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{serviceID.String() + "|1.0": {ServiceID: serviceID, ServiceVersionID: versionID, Version: "1.0", Revision: 1, SourceHash: "contract-hash"}}}
	configStore := &mockConfigStore{}
	router := newControlTestRouter(workspace.accountID)
	router.Post("/execution-config/plan", ExecutionConfigPlanHandler(configStore, workspace, registry))
	router.Post("/execution-config/apply", ExecutionConfigApplyHandler(configStore, workspace, registry))
	source := `import * as z from "zod/mini";
import { buildExecutionApp } from "@fused/execution";
export default buildExecutionApp({input:z.object({name:z.string()}),output:z.object({greeting:z.string()}),fetch:{searchable:["name"]},async execute({input}){return {greeting:input.name}}});`
	requestBody, _ := json.Marshal(map[string]any{"source_hash": "sha256:cart", "config_key": "execution:greeting-app:1.0.0", "owner_team": "platform", "config": map[string]any{"apiVersion": "fused/v1", "kind": "execution", "name": "greeting-app", "version": "1.0.0", "language": "typescript", "generate": false, "bucket": "default", "source": source, "services": map[string]any{"greeting": map[string]any{"version": "1.0", "operations": []string{"greet"}}}}})
	request := httptest.NewRequest(http.MethodPost, "/execution-config/plan", bytes.NewReader(requestBody))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// The public plan route must compile the authored source before it returns a reviewable plan ID.
	if response.Code != http.StatusOK || configStore.createdPlan == nil {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	var desired sdkConfigDocument
	var resolved appResolvedPayload
	_ = json.Unmarshal(configStore.createdPlan.DesiredState, &desired)
	_ = json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved)
	// The cart contains source, while only Engine writes a digest and bundle into its plan.
	if desired.BundleDigest == "" || resolved.ExecutionBundle == nil || desired.BundleDigest != resolved.ExecutionBundle.Digest {
		t.Fatalf("desired digest and retained artifact = %q/%#v", desired.BundleDigest, resolved.ExecutionBundle)
	}
	applyBody, _ := json.Marshal(map[string]any{"plan_id": configStore.plan.ID.String(), "source_hash": "sha256:cart", "skip_token": true})
	applyRequest := httptest.NewRequest(http.MethodPost, "/execution-config/apply", bytes.NewReader(applyBody))
	applyRequest.Header.Set("X-API-Key", "fsk_test")
	applyResponse := httptest.NewRecorder()
	router.ServeHTTP(applyResponse, applyRequest)
	assertExecutionHTTPApply(t, applyResponse, configStore)
}

// assertExecutionHTTPApply checks that the public apply route passes the compiler artifact into persistence.
func assertExecutionHTTPApply(t *testing.T, response *httptest.ResponseRecorder, configStore *mockConfigStore) {
	t.Helper()
	// A successful response requires an exact bundle in the same app publication call.
	if response.Code != http.StatusOK || configStore.artifactApply == nil || configStore.artifactApply.ExecutionBundle == nil {
		t.Fatalf("apply status/body/artifact = %d/%s/%#v", response.Code, response.Body.String(), configStore.artifactApply)
	}
	// The artifact must retain the reviewed source identity and one generated app version ID.
	if configStore.artifactApply.ExecutionBundle.AppID == uuid.Nil || configStore.artifactApply.ExecutionBundle.SourceHash != "sha256:cart" {
		t.Fatal("apply did not bind the compiled artifact to an app ID")
	}
}
