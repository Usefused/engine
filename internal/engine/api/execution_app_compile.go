package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const maxUnifiedAppSourceBytes = 256 << 10

// validateExecutionSourceMode separates Engine compilation from the retained precompiled attachment workflow.
func validateExecutionSourceMode(doc sdkConfigDocument) error {
	// Inline source must be finite and may not smuggle a caller-chosen compiled digest.
	if strings.TrimSpace(doc.Source) != "" {
		if doc.BundleDigest != "" || len(doc.Source) > maxUnifiedAppSourceBytes {
			return errors.New("execution source must be at most 256 KiB and omit bundle_digest")
		}
		return nil
	}
	// Older authored deployments still require the exact bundle identity they attach separately.
	if !store.IsCanonicalUnifiedAppBundleDigest(doc.BundleDigest) {
		return errors.New("Unified App config requires source or a canonical bundle_digest")
	}
	return nil
}

type executionPlanArtifact struct {
	Digest   string          `json:"digest"`
	BundleJS string          `json:"bundle_js"`
	Manifest json.RawMessage `json:"manifest"`
}

type executionCompilerSelection struct {
	Service          string `json:"service"`
	Operation        string `json:"operation"`
	ServiceID        string `json:"serviceId"`
	ServiceVersionID string `json:"serviceVersionId"`
	EndpointID       string `json:"endpointId"`
}

// compileExecutionPlanAndPinDigest records only an Engine-produced digest in the desired app state.
func compileExecutionPlanAndPinDigest(ctx context.Context, s store.Store, doc sdkConfigDocument, selections []models.SDKSelection, services []sdkResolvedService, stateDoc *sdkConfigDocument) (*executionPlanArtifact, error) {
	artifact, err := compileExecutionPlanSource(ctx, s, doc, selections, services)
	if err != nil {
		return nil, err
	}
	// A precompiled config retains its already-reviewed digest unchanged.
	if artifact != nil {
		stateDoc.BundleDigest = artifact.Digest
	}
	return artifact, nil
}

// compileExecutionPlanSource freezes authored code and exact local operation IDs before an apply can publish it.
func compileExecutionPlanSource(ctx context.Context, s store.Store, doc sdkConfigDocument, selections []models.SDKSelection, services []sdkResolvedService) (*executionPlanArtifact, error) {
	// Explicit precompiled configurations retain their existing attachment workflow.
	if doc.Kind != store.AppKindUnifiedApp.String() || strings.TrimSpace(doc.Source) == "" {
		return nil, nil
	}
	repository, ok := s.(store.ServiceContractEndpointSelectionBatchStore)
	// Compilation cannot infer endpoint IDs from names or a remote catalogue.
	if !ok {
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app contract lookup is unavailable"}
	}
	pins, err := executionCompilerSelections(ctx, repository, selections, services)
	if err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.compile")
	defer span.End()
	span.SetAttributes(attribute.Int("execution.operation_count", len(pins)))
	artifact, err := runExecutionCompiler(ctx, doc.Source, pins)
	// Authored source and compiler diagnostics are deliberately absent from telemetry.
	if err != nil {
		span.SetStatus(codes.Error, "compile_failed")
		return nil, err
	}
	if err := validateCompiledExecutionPins(artifact.Manifest, pins); err != nil {
		span.SetStatus(codes.Error, "compile_scope_mismatch")
		return nil, err
	}
	span.SetAttributes(attribute.String("outcome", "compiled"), attribute.Int("execution.bundle_bytes", len(artifact.BundleJS)))
	return artifact, nil
}

// validateCompiledExecutionPins requires the compiler declaration to preserve every Engine-selected physical identity.
func validateCompiledExecutionPins(raw json.RawMessage, pins []executionCompilerSelection) error {
	manifest, err := parseUnifiedAppManifest(raw)
	// A malformed descriptor cannot be admitted even when the emitted JavaScript has a valid hash.
	if err != nil || len(manifest.SelectedOperations) != len(pins) {
		return workspaceConfigHTTPError{status: 503, message: "unified app compiler changed selected operations"}
	}
	expected := make(map[string]executionCompilerSelection, len(pins))
	for _, pin := range pins {
		expected[executionOperationKey(pin.Service, pin.Operation)] = pin
	}
	for _, actual := range manifest.SelectedOperations {
		key := executionOperationKey(actual.Service, actual.Operation)
		pin, found := expected[key]
		// A compiler mismatch must fail before the plan can grant provider calls.
		if !found || pin.ServiceID != actual.ServiceID.String() || pin.ServiceVersionID != actual.ServiceVersionID.String() || pin.EndpointID != actual.EndpointID.String() {
			return workspaceConfigHTTPError{status: 503, message: "unified app compiler changed selected operations"}
		}
		delete(expected, key)
	}
	// Equal counts alone cannot prove coverage when the compiler repeats one selected operation.
	if len(expected) != 0 {
		return workspaceConfigHTTPError{status: 503, message: "unified app compiler changed selected operations"}
	}
	return nil
}

// executionPlanBundleForApply promotes only the compiler output pinned inside the reviewed immutable plan.
func executionPlanBundleForApply(doc sdkConfigDocument, artifact *executionPlanArtifact, sourceHash string, appID uuid.UUID) (*store.UnifiedAppBundle, error) {
	// Legacy precompiled versions use the explicit attachment API; inline source always needs a plan artifact.
	if strings.TrimSpace(doc.Source) == "" {
		return nil, nil
	}
	if artifact == nil || artifact.Digest != doc.BundleDigest || store.UnifiedAppBundleDigest([]byte(artifact.BundleJS)) != doc.BundleDigest {
		return nil, workspaceConfigHTTPError{status: 409, message: "unified app compiled plan artifact is missing or changed"}
	}
	// The Engine compiler inspected declarations in its bounded child before the plan was persisted;
	// re-evaluating top-level author code during apply would add a second execution boundary.
	return &store.UnifiedAppBundle{AppID: appID, SourceHash: sourceHash, BundleJS: artifact.BundleJS, Manifest: artifact.Manifest}, nil
}

// executionCompilerSelections uses one set-based contract read and requires an exact result for every reviewed operation.
func executionCompilerSelections(ctx context.Context, repository store.ServiceContractEndpointSelectionBatchStore, selections []models.SDKSelection, services []sdkResolvedService) ([]executionCompilerSelection, error) {
	keys, err := executionCompilerServiceKeys(selections, services)
	if err != nil {
		return nil, err
	}
	requests := make([]store.ServiceContractEndpointSelection, 0, len(selections))
	want := make(map[int]map[string]bool, len(selections))
	for index, selection := range selections {
		// Select-all and empty operation sets cannot be represented by a stable source-level method binding.
		if selection.SelectAll || len(selection.OperationNames) == 0 {
			return nil, workspaceConfigHTTPError{status: 400, message: "unified app source requires explicit operations"}
		}
		want[index] = make(map[string]bool, len(selection.OperationNames))
		for _, name := range selection.OperationNames {
			want[index][name] = true
		}
		requests = append(requests, store.ServiceContractEndpointSelection{SelectionIndex: index, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID, OperationNames: selection.OperationNames, EndpointNames: selection.OperationNames})
	}
	matches, err := repository.ListServiceContractEndpointsForSelections(ctx, requests, nil)
	// A failed local snapshot read cannot be replaced by broader workspace discovery.
	if err != nil {
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app operation contracts are unavailable"}
	}
	return bindExecutionCompilerMatches(selections, keys, want, matches)
}

// executionCompilerServiceKeys binds authored aliases by resolver index instead of collapsing identical service IDs.
func executionCompilerServiceKeys(selections []models.SDKSelection, services []sdkResolvedService) ([]string, error) {
	// Resolver output preserves authored selection order, including two aliases of one service ID.
	if len(services) != len(selections) {
		return nil, workspaceConfigHTTPError{status: 409, message: "unified app service scope changed during planning"}
	}
	keys := make([]string, len(services))
	for index, service := range services {
		// Index binding prevents one alias from replacing a sibling with the same provider identity.
		if service.ServiceID != selections[index].ServiceID || service.ServiceVersionID != selections[index].ServiceVersionID || service.PublicTarget == "" {
			return nil, workspaceConfigHTTPError{status: 409, message: "unified app service scope changed during planning"}
		}
		keys[index] = service.PublicTarget
	}
	return keys, nil
}

// bindExecutionCompilerMatches checks cardinality and identity before producing a deterministic compiler specification.
func bindExecutionCompilerMatches(selections []models.SDKSelection, keys []string, want map[int]map[string]bool, matches []store.ServiceContractEndpointMatch) ([]executionCompilerSelection, error) {
	pins := make([]executionCompilerSelection, 0, len(matches))
	for _, match := range matches {
		index := match.SelectionIndex
		// A row outside the requested batch, missing ID, or duplicate name cannot gain capability authority.
		if index < 0 || index >= len(selections) || match.Endpoint.ID == [16]byte{} || !want[index][match.Endpoint.Name] {
			return nil, workspaceConfigHTTPError{status: 409, message: "unified app operation scope changed during planning"}
		}
		delete(want[index], match.Endpoint.Name)
		selection := selections[index]
		key := keys[index]
		// A missing authored alias would make fused.fetch route to a different method name.
		if key == "" {
			return nil, workspaceConfigHTTPError{status: 409, message: "unified app service scope changed during planning"}
		}
		pins = append(pins, executionCompilerSelection{Service: key, Operation: match.Endpoint.Name, ServiceID: selection.ServiceID.String(), ServiceVersionID: selection.ServiceVersionID.String(), EndpointID: match.Endpoint.ID.String()})
	}
	for _, remaining := range want {
		// Partial snapshot results must fail instead of silently dropping an authored method.
		if len(remaining) != 0 {
			return nil, workspaceConfigHTTPError{status: 409, message: "unified app operation scope changed during planning"}
		}
	}
	sort.Slice(pins, func(left, right int) bool {
		if pins[left].Service == pins[right].Service {
			return pins[left].Operation < pins[right].Operation
		}
		return pins[left].Service < pins[right].Service
	})
	return pins, nil
}

// runExecutionCompiler invokes the Engine-owned compiler with bounded source and private temporary artifacts.
func runExecutionCompiler(ctx context.Context, source string, pins []executionCompilerSelection) (*executionPlanArtifact, error) {
	release, err := admitExecutionCompile(ctx)
	// Limit the aggregate compiler and inspection footprint before creating temporary files or processes.
	if err != nil {
		return nil, err
	}
	defer release()
	compiler := os.Getenv("FUSED_EXECUTION_COMPILER")
	// Packaged Engine images pin one compiler; local development can use the same repository package.
	if compiler == "" {
		compiler = "/app/runtime/execution/dist/src/cli.js"
		if _, err := os.Stat(compiler); errors.Is(err, os.ErrNotExist) {
			compiler = "runtime/execution/dist/src/cli.js"
		}
	}
	dir, err := os.MkdirTemp("", "fused-execution-compile-")
	if err != nil {
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler storage is unavailable"}
	}
	defer os.RemoveAll(dir)
	paths := []string{filepath.Join(dir, "app.ts"), filepath.Join(dir, "spec.json"), filepath.Join(dir, "bundle.js"), filepath.Join(dir, "manifest.json"), filepath.Join(dir, "digest.json")}
	spec, _ := json.Marshal(map[string]any{"entryFile": paths[0], "selectedOperations": pins})
	// Source and spec remain private to this one bounded compiler invocation.
	if err := os.WriteFile(paths[0], []byte(source), 0600); err != nil {
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler storage is unavailable"}
	}
	if err := os.WriteFile(paths[1], spec, 0600); err != nil {
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler storage is unavailable"}
	}
	// One deadline covers trusted compilation and the subsequent confined declaration inspection.
	compileCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	command := exec.CommandContext(compileCtx, "node", "--max-old-space-size=256", compiler, "--config", paths[1], "--out", paths[2], "--bundle-only", "true", "--digest", paths[4])
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	output := &executionCompilerOutput{}
	command.Stdout, command.Stderr = output, output
	err = command.Run()
	// Compiler diagnostics are returned in bounded form for source correction, never written to OTEL.
	if err != nil {
		return nil, executionCompilerFailure(compileCtx, err, output.bytes)
	}
	return readExecutionCompilerArtifacts(compileCtx, paths[2], paths[4])
}

// executionCompilerFailure distinguishes authoring errors from missing or stalled Engine compiler infrastructure.
func executionCompilerFailure(ctx context.Context, err error, output []byte) error {
	// A bounded compiler deadline should tell the caller the Engine build stalled, not blame TypeScript syntax.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(string(output), "ETIMEDOUT") {
		return workspaceConfigHTTPError{status: 504, message: "unified app compiler timed out"}
	}
	var executableError *exec.Error
	// A missing Node runtime or compiler executable is a deployment fault.
	if errors.As(err, &executableError) {
		return workspaceConfigHTTPError{status: 503, message: "unified app compiler is unavailable"}
	}
	// Build errors can include source locations, but never full source or unbounded child output.
	return workspaceConfigHTTPError{status: 400, message: fmt.Sprintf("unified app compile failed: %.1024s", strings.TrimSpace(string(output)))}
}

// readExecutionCompilerArtifacts validates byte identity before a plan can retain executable source.
func readExecutionCompilerArtifacts(ctx context.Context, bundlePath, digestPath string) (*executionPlanArtifact, error) {
	bundle, digest, err := readBoundedCompilerOutputs(bundlePath, digestPath)
	if err != nil {
		return nil, err
	}
	var identity struct {
		BundleDigest string `json:"bundle_digest"`
	}
	// The digest sidecar must identify exactly the bytes retained in the plan.
	if json.Unmarshal(digest, &identity) != nil || identity.BundleDigest != store.UnifiedAppBundleDigest(bundle) {
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler artifact digest mismatch"}
	}
	manifest, err := sandbox.InspectCapabilityBundle(ctx, bundle)
	// Top-level source is executable too: evaluate it only inside the OS-confined worker.
	if err != nil {
		return nil, unifiedAppBundleInspectionError(err)
	}
	if _, err := parseUnifiedAppManifest(manifest); err != nil {
		return nil, workspaceConfigHTTPError{status: 400, message: "unified app manifest is invalid"}
	}
	return &executionPlanArtifact{Digest: identity.BundleDigest, BundleJS: string(bundle), Manifest: manifest}, nil
}

// readBoundedCompilerOutputs keeps compiler products finite before the plan serializes them into JSONB.
func readBoundedCompilerOutputs(bundlePath, digestPath string) ([]byte, []byte, error) {
	bundle, bundleErr := readBoundedCompilerFile(bundlePath, 2<<20)
	digest, digestErr := readBoundedCompilerFile(digestPath, 1<<10)
	// Missing outputs indicate a compiler packaging fault rather than an authoring error.
	if bundleErr != nil || digestErr != nil {
		return nil, nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler produced invalid artifacts"}
	}
	// Size checks also cap the in-transaction immutable artifact write.
	if len(bundle) == 0 {
		return nil, nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler produced invalid artifacts"}
	}
	return bundle, digest, nil
}
