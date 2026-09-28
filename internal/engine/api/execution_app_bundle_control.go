package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxExecutionAppBundleRequestBytes = 3 << 20
const maxExecutionAppSelectedOperations = 64

type executionAppBundleAttachRequest struct {
	SourceHash string          `json:"source_hash"`
	BundleJS   string          `json:"bundle_js"`
	Manifest   json.RawMessage `json:"manifest"`
}

type executionAppBundleAdmissionStore interface {
	store.Store
	store.ExecutionAppBundleStore
	store.ServiceContractEndpointSelectionBatchStore
}

type executionAppSelectionKey struct {
	serviceID uuid.UUID
	versionID uuid.UUID
}

type executionAppExpectedEndpoint struct {
	id   uuid.UUID
	name string
}

// ExecutionAppBundleHandler is a transitional write-once attach path for an applied Execution App version.
func ExecutionAppBundleHandler(s store.Store) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.execution_app.bundle_attach")
		defer span.End()
		actor, app, err := lifecycleActorAndApp(ctx, s, request)
		// Actor and exact workspace ownership are prerequisites for reading authored code.
		if err != nil {
			writeExecutionAppBundleError(writer, span, err)
			return
		}
		if err := authorizeExecutionAppBundle(ctx, actor, app); err != nil {
			writeExecutionAppBundleError(writer, span, err)
			return
		}
		repository, ok := s.(executionAppBundleAdmissionStore)
		// The route cannot attach code without both immutable storage and a set-based scope lookup.
		if !ok {
			writeExecutionAppBundleError(writer, span, workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "execution app deployment is unavailable"})
			return
		}
		bundle, err := prepareExecutionAppBundle(ctx, writer, request, repository, app)
		// Validation must finish before the immutable artifact receives a database row.
		if err != nil {
			writeExecutionAppBundleError(writer, span, err)
			return
		}
		if err := repository.CreateExecutionAppBundle(ctx, bundle); err != nil {
			writeExecutionAppBundleError(writer, span, err)
			return
		}
		span.SetAttributes(attribute.String("app.id", app.AppID.String()), attribute.String("app.family_id", app.AppFamilyID.String()), attribute.String("outcome", "attached"))
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"attached","app_id":"` + app.AppID.String() + `"}`))
	}
}

// authorizeExecutionAppBundle applies the same family app.manage boundary as other app mutations.
func authorizeExecutionAppBundle(ctx context.Context, actor accesscontrol.Actor, app *store.App) error {
	// An absent exact app cannot yield a family grant.
	if app == nil {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "app not found"}
	}
	return (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAppManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID},
	})
}

// prepareExecutionAppBundle checks version identity, manifest, and selected operations before storage.
func prepareExecutionAppBundle(ctx context.Context, writer http.ResponseWriter, request *http.Request, repository executionAppBundleAdmissionStore, app *store.App) (store.ExecutionAppBundle, error) {
	family, err := repository.GetAppFamily(ctx, app.AppFamilyID)
	// Only an active Execution App version can acquire its planned hosted bundle.
	if err != nil || family == nil || family.Kind != store.AppKindExecution || app.Status != store.AppStatusActive {
		return store.ExecutionAppBundle{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "active Execution App version required"}
	}
	decoded, err := decodeExecutionAppBundleAttach(writer, request)
	if err != nil {
		return store.ExecutionAppBundle{}, err
	}
	if err := validateExecutionAppBundleIdentityAndBytes(app, decoded); err != nil {
		return store.ExecutionAppBundle{}, err
	}
	// The submitted descriptor must be exactly what the compiled bundle publishes when evaluated without host effects.
	if err := verifyExecutionAppBundleManifest(ctx, decoded.BundleJS, decoded.Manifest); err != nil {
		return store.ExecutionAppBundle{}, err
	}
	manifest, err := parseExecutionAppManifest(decoded.Manifest)
	// The one authored execute contract must be admitted before attaching code.
	if err != nil {
		return store.ExecutionAppBundle{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid execution app manifest"}
	}
	if err := validateExecutionAppManifestScope(ctx, repository, app, manifest); err != nil {
		return store.ExecutionAppBundle{}, err
	}
	return store.ExecutionAppBundle{AppID: app.AppID, SourceHash: decoded.SourceHash, BundleJS: decoded.BundleJS, Manifest: decoded.Manifest}, nil
}

// validateExecutionAppBundleIdentityAndBytes pins submitted source and compiled bytes to the applied app version.
func validateExecutionAppBundleIdentityAndBytes(app *store.App, decoded executionAppBundleAttachRequest) error {
	// The applied app's source identity is authoritative; the request cannot choose a new version.
	if app.SourceHash == "" || decoded.SourceHash != app.SourceHash {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "bundle source hash does not match app version"}
	}
	// The route should report malformed code before the immutable store applies its matching database limit.
	if len(decoded.BundleJS) == 0 || len(decoded.BundleJS) > 2<<20 {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bundle script must be between 1 byte and 2 MiB"}
	}
	// An old version without a planned digest, or bytes that differ from its plan, cannot gain hosted behavior.
	if !store.IsCanonicalExecutionAppBundleDigest(app.BundleDigest) || store.ExecutionAppBundleDigest([]byte(decoded.BundleJS)) != app.BundleDigest {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "bundle digest does not match planned app version"}
	}
	return nil
}

// verifyExecutionAppBundleManifest binds the admitted declaration to the actual script without provider or DB access.
func verifyExecutionAppBundleManifest(ctx context.Context, script string, submitted json.RawMessage) error {
	inspected, err := sandbox.InspectCapabilityBundle(ctx, []byte(script))
	// A script that cannot publish its own declaration must not acquire execution authority.
	if err != nil {
		return executionAppBundleInspectionError(err)
	}
	actual, actualErr := canonicaljson.Canonicalize(inspected)
	claimed, claimedErr := canonicaljson.Canonicalize(submitted)
	// Canonical equality rejects a forged manifest while allowing harmless JSON property order differences.
	if actualErr != nil || claimedErr != nil || !bytes.Equal(actual, claimed) {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bundle manifest does not match compiled declarations"}
	}
	return nil
}

// executionAppBundleInspectionError separates unavailable worker isolation from invalid authored code.
func executionAppBundleInspectionError(err error) error {
	// A missing OS isolation boundary is an Engine deployment fault, so retrying other bundle bytes cannot help.
	if errors.Is(err, sandbox.ErrCapabilityWorkerUnavailable) {
		return workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "execution app worker is unavailable"}
	}
	return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bundle declaration is invalid"}
}

// decodeExecutionAppBundleAttach admits one bounded JSON document with no unknown deployment controls.
func decodeExecutionAppBundleAttach(writer http.ResponseWriter, request *http.Request) (executionAppBundleAttachRequest, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	// A fixed JSON media type prevents alternate parsers from changing bundle bytes.
	if err != nil || mediaType != "application/json" {
		return executionAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusUnsupportedMediaType, message: "Content-Type must be application/json"}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxExecutionAppBundleRequestBytes))
	// A bounded body is required before JSON decoding allocates the compiled script.
	if err != nil || len(raw) == 0 {
		return executionAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid or oversized bundle request"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decoded executionAppBundleAttachRequest
	// Unknown controls and malformed JSON cannot become deployment authority.
	if err := decoder.Decode(&decoded); err != nil {
		return executionAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid bundle request"}
	}
	var trailing any
	// A second document must not change the authored manifest after admission.
	if err := decoder.Decode(&trailing); err != io.EOF {
		return executionAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid bundle request"}
	}
	return decoded, nil
}

// validateExecutionAppManifestScope admits every declared physical call in one snapshot query.
func validateExecutionAppManifestScope(ctx context.Context, repository store.ServiceContractEndpointSelectionBatchStore, app *store.App, manifest *executionAppManifest) error {
	selections, err := models.DecodeAppSelections(app.ScopeSchemaVersion, app.Selections)
	// Incomplete immutable app scope cannot authorize newly attached code.
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "app operation scope is unavailable"}
	}
	requests, expected, err := executionAppEndpointRequests(manifest, selections)
	if err != nil {
		return err
	}
	// A bundle with no provider calls has no physical scope to query.
	if len(requests) == 0 {
		return nil
	}
	matches, err := repository.ListServiceContractEndpointsForSelections(ctx, requests, nil)
	// A failed snapshot read cannot silently admit undeclared or removed operations.
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "app operation scope is unavailable"}
	}
	return verifyExecutionAppEndpointMatches(matches, expected)
}

// executionAppEndpointRequests converts manifest pins into one set-based selection query.
func executionAppEndpointRequests(manifest *executionAppManifest, selections []models.SDKSelection) ([]store.ServiceContractEndpointSelection, map[int]executionAppExpectedEndpoint, error) {
	requests := make([]store.ServiceContractEndpointSelection, 0)
	expected := make(map[int]executionAppExpectedEndpoint)
	selected := make(map[executionAppSelectionKey]models.SDKSelection, len(selections))
	for _, selection := range selections {
		key := executionAppSelectionKey{serviceID: selection.ServiceID, versionID: selection.ServiceVersionID}
		// Duplicate exact service versions would make operation authority ambiguous.
		if _, exists := selected[key]; exists {
			return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app operation scope is ambiguous"}
		}
		selected[key] = selection
	}
	// Search only accepts paths approved when this immutable bundle is attached.
	if err := validateExecutionAppSearchable(manifest.Searchable); err != nil {
		return nil, nil, err
	}
	for _, operation := range manifest.SelectedOperations {
		// A fixed total prevents a manifest from causing unbounded contract work.
		if len(requests) >= maxExecutionAppSelectedOperations {
			return nil, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "too many selected operations"}
		}
		selection, found := selected[executionAppSelectionKey{serviceID: operation.ServiceID, versionID: operation.ServiceVersionID}]
		// An unselected service/version cannot become callable through a manifest entry.
		if !found {
			return nil, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "manifest operation is outside app scope"}
		}
		index := len(requests)
		requests = append(requests, store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: operation.ServiceID, ServiceVersionID: operation.ServiceVersionID,
			SelectAll: selection.SelectAll, EndpointIDs: selection.EndpointIDs, OperationNames: selection.OperationNames,
			EndpointNames: []string{operation.Operation},
		})
		expected[index] = executionAppExpectedEndpoint{id: operation.EndpointID, name: operation.Operation}
	}
	return requests, expected, nil
}

// verifyExecutionAppEndpointMatches checks that SQL returned one exact selected endpoint for each pin.
func verifyExecutionAppEndpointMatches(matches []store.ServiceContractEndpointMatch, expected map[int]executionAppExpectedEndpoint) error {
	// Missing rows mean at least one manifest operation was not selected in the app snapshot.
	if len(matches) != len(expected) {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "manifest operation is outside app scope"}
	}
	seen := make(map[int]struct{}, len(matches))
	for _, match := range matches {
		wanted, exists := expected[match.SelectionIndex]
		// A duplicate or mismatched endpoint cannot be repaired by another row.
		if !exists || wanted.id != match.Endpoint.ID || wanted.name != match.Endpoint.Name {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "manifest operation does not match app snapshot"}
		}
		if _, duplicate := seen[match.SelectionIndex]; duplicate {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "manifest operation is duplicated in snapshot"}
		}
		seen[match.SelectionIndex] = struct{}{}
	}
	return nil
}

// validateExecutionAppSearchable bounds immutable data filters to scalar object paths.
func validateExecutionAppSearchable(paths []string) error {
	// Search index policy allows at most 32 authored data paths per app version.
	if len(paths) > 32 {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "too many searchable data paths"}
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		// The same path syntax is enforced again by execution-result search.
		if !store.ValidExecutionDataPath(path) {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid searchable data path"}
		}
		if _, duplicate := seen[path]; duplicate {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "duplicate searchable data path"}
		}
		seen[path] = struct{}{}
	}
	return nil
}

// writeExecutionAppBundleError records a bounded control outcome without logging bundle bytes.
func writeExecutionAppBundleError(writer http.ResponseWriter, span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, "bundle attachment failed")
	span.SetAttributes(attribute.String("outcome", "failed"))
	// Immutable conflicts are distinct from malformed submitted artifacts.
	if errors.Is(err, store.ErrExecutionAppBundleImmutable) {
		writeSDKConfigError(writer, workspaceConfigHTTPError{status: http.StatusConflict, message: "app bundle is immutable; create a new version"})
		return
	}
	// A version removed or changed between admission and write cannot acquire a bundle.
	if errors.Is(err, store.ErrExecutionAppBundleNotFound) {
		writeSDKConfigError(writer, workspaceConfigHTTPError{status: http.StatusConflict, message: "app version is no longer available for bundle attachment"})
		return
	}
	writeSDKConfigError(writer, err)
}
