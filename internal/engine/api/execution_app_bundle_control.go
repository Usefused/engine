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

const maxUnifiedAppBundleRequestBytes = 3 << 20
const maxUnifiedAppSelectedOperations = 64

type unifiedAppBundleAttachRequest struct {
	SourceHash string          `json:"source_hash"`
	BundleJS   string          `json:"bundle_js"`
	Manifest   json.RawMessage `json:"manifest"`
}

type unifiedAppBundleAdmissionStore interface {
	store.Store
	store.UnifiedAppBundleStore
	store.ServiceContractEndpointSelectionBatchStore
}

type unifiedAppSelectionKey struct {
	serviceID uuid.UUID
	versionID uuid.UUID
}

type unifiedAppExpectedEndpoint struct {
	id   uuid.UUID
	name string
}

// UnifiedAppBundleHandler is a transitional write-once attach path for an applied Unified App version.
func UnifiedAppBundleHandler(s store.Store) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.unified_app.bundle_attach")
		defer span.End()
		actor, app, err := lifecycleActorAndApp(ctx, s, request)
		// Actor and exact workspace ownership are prerequisites for reading authored code.
		if err != nil {
			writeUnifiedAppBundleError(writer, span, err)
			return
		}
		if err := authorizeUnifiedAppBundle(ctx, actor, app); err != nil {
			writeUnifiedAppBundleError(writer, span, err)
			return
		}
		repository, ok := s.(unifiedAppBundleAdmissionStore)
		// The route cannot attach code without both immutable storage and a set-based scope lookup.
		if !ok {
			writeUnifiedAppBundleError(writer, span, workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "unified app deployment is unavailable"})
			return
		}
		bundle, err := prepareUnifiedAppBundle(ctx, writer, request, repository, app)
		// Validation must finish before the immutable artifact receives a database row.
		if err != nil {
			writeUnifiedAppBundleError(writer, span, err)
			return
		}
		if err := repository.CreateUnifiedAppBundle(ctx, bundle); err != nil {
			writeUnifiedAppBundleError(writer, span, err)
			return
		}
		span.SetAttributes(attribute.String("app.id", app.AppID.String()), attribute.String("app.family_id", app.AppFamilyID.String()), attribute.String("outcome", "attached"))
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"attached","app_id":"` + app.AppID.String() + `"}`))
	}
}

// authorizeUnifiedAppBundle applies the same family app.manage boundary as other app mutations.
func authorizeUnifiedAppBundle(ctx context.Context, actor accesscontrol.Actor, app *store.App) error {
	// An absent exact app cannot yield a family grant.
	if app == nil {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "app not found"}
	}
	return (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAppManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID},
	})
}

// prepareUnifiedAppBundle checks version identity, manifest, and selected operations before storage.
func prepareUnifiedAppBundle(ctx context.Context, writer http.ResponseWriter, request *http.Request, repository unifiedAppBundleAdmissionStore, app *store.App) (store.UnifiedAppBundle, error) {
	family, err := repository.GetAppFamily(ctx, app.AppFamilyID)
	// Only an active Unified App version can acquire its planned hosted bundle.
	if err != nil || family == nil || family.Kind != store.AppKindUnifiedApp || app.Status != store.AppStatusActive {
		return store.UnifiedAppBundle{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "active Unified App version required"}
	}
	decoded, err := decodeUnifiedAppBundleAttach(writer, request)
	if err != nil {
		return store.UnifiedAppBundle{}, err
	}
	if err := validateUnifiedAppBundleIdentityAndBytes(app, decoded); err != nil {
		return store.UnifiedAppBundle{}, err
	}
	// The submitted descriptor must be exactly what the compiled bundle publishes when evaluated without host effects.
	if err := verifyUnifiedAppBundleManifest(ctx, decoded.BundleJS, decoded.Manifest); err != nil {
		return store.UnifiedAppBundle{}, err
	}
	manifest, err := parseUnifiedAppManifest(decoded.Manifest)
	// The one authored execute contract must be admitted before attaching code.
	if err != nil {
		return store.UnifiedAppBundle{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid unified app manifest"}
	}
	if err := validateUnifiedAppManifestScope(ctx, repository, app, manifest); err != nil {
		return store.UnifiedAppBundle{}, err
	}
	return store.UnifiedAppBundle{AppID: app.AppID, SourceHash: decoded.SourceHash, BundleJS: decoded.BundleJS, Manifest: decoded.Manifest}, nil
}

// validateUnifiedAppBundleIdentityAndBytes pins submitted source and compiled bytes to the applied app version.
func validateUnifiedAppBundleIdentityAndBytes(app *store.App, decoded unifiedAppBundleAttachRequest) error {
	// The applied app's source identity is authoritative; the request cannot choose a new version.
	if app.SourceHash == "" || decoded.SourceHash != app.SourceHash {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "bundle source hash does not match app version"}
	}
	// The route should report malformed code before the immutable store applies its matching database limit.
	if len(decoded.BundleJS) == 0 || len(decoded.BundleJS) > 2<<20 {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bundle script must be between 1 byte and 2 MiB"}
	}
	// An old version without a planned digest, or bytes that differ from its plan, cannot gain hosted behavior.
	if !store.IsCanonicalUnifiedAppBundleDigest(app.BundleDigest) || store.UnifiedAppBundleDigest([]byte(decoded.BundleJS)) != app.BundleDigest {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "bundle digest does not match planned app version"}
	}
	return nil
}

// verifyUnifiedAppBundleManifest binds the admitted declaration to the actual script without provider or DB access.
func verifyUnifiedAppBundleManifest(ctx context.Context, script string, submitted json.RawMessage) error {
	inspected, err := sandbox.InspectCapabilityBundle(ctx, []byte(script))
	// A script that cannot publish its own declaration must not acquire execution authority.
	if err != nil {
		return unifiedAppBundleInspectionError(err)
	}
	actual, actualErr := canonicaljson.Canonicalize(inspected)
	claimed, claimedErr := canonicaljson.Canonicalize(submitted)
	// Canonical equality rejects a forged manifest while allowing harmless JSON property order differences.
	if actualErr != nil || claimedErr != nil || !bytes.Equal(actual, claimed) {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bundle manifest does not match compiled declarations"}
	}
	return nil
}

// unifiedAppBundleInspectionError separates unavailable worker isolation from invalid authored code.
func unifiedAppBundleInspectionError(err error) error {
	// A missing OS isolation boundary is an Engine deployment fault, so retrying other bundle bytes cannot help.
	if errors.Is(err, sandbox.ErrCapabilityWorkerUnavailable) {
		return workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "unified app worker is unavailable"}
	}
	return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bundle declaration is invalid"}
}

// decodeUnifiedAppBundleAttach admits one bounded JSON document with no unknown deployment controls.
func decodeUnifiedAppBundleAttach(writer http.ResponseWriter, request *http.Request) (unifiedAppBundleAttachRequest, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	// A fixed JSON media type prevents alternate parsers from changing bundle bytes.
	if err != nil || mediaType != "application/json" {
		return unifiedAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusUnsupportedMediaType, message: "Content-Type must be application/json"}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxUnifiedAppBundleRequestBytes))
	// A bounded body is required before JSON decoding allocates the compiled script.
	if err != nil || len(raw) == 0 {
		return unifiedAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid or oversized bundle request"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decoded unifiedAppBundleAttachRequest
	// Unknown controls and malformed JSON cannot become deployment authority.
	if err := decoder.Decode(&decoded); err != nil {
		return unifiedAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid bundle request"}
	}
	var trailing any
	// A second document must not change the authored manifest after admission.
	if err := decoder.Decode(&trailing); err != io.EOF {
		return unifiedAppBundleAttachRequest{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid bundle request"}
	}
	return decoded, nil
}

// validateUnifiedAppManifestScope admits every declared physical call in one snapshot query.
func validateUnifiedAppManifestScope(ctx context.Context, repository store.ServiceContractEndpointSelectionBatchStore, app *store.App, manifest *unifiedAppManifest) error {
	selections, err := models.DecodeAppSelections(app.ScopeSchemaVersion, app.Selections)
	// Incomplete immutable app scope cannot authorize newly attached code.
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "app operation scope is unavailable"}
	}
	requests, expected, err := unifiedAppEndpointRequests(manifest, selections)
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
	return verifyUnifiedAppEndpointMatches(matches, expected)
}

// unifiedAppEndpointRequests converts manifest pins into one set-based selection query.
func unifiedAppEndpointRequests(manifest *unifiedAppManifest, selections []models.SDKSelection) ([]store.ServiceContractEndpointSelection, map[int]unifiedAppExpectedEndpoint, error) {
	requests := make([]store.ServiceContractEndpointSelection, 0)
	expected := make(map[int]unifiedAppExpectedEndpoint)
	selected := make(map[unifiedAppSelectionKey]models.SDKSelection, len(selections))
	for _, selection := range selections {
		key := unifiedAppSelectionKey{serviceID: selection.ServiceID, versionID: selection.ServiceVersionID}
		// Duplicate exact service versions would make operation authority ambiguous.
		if _, exists := selected[key]; exists {
			return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app operation scope is ambiguous"}
		}
		selected[key] = selection
	}
	// Search only accepts paths approved when this immutable bundle is attached.
	if err := validateUnifiedAppSearchable(manifest.Searchable); err != nil {
		return nil, nil, err
	}
	for _, operation := range manifest.SelectedOperations {
		// A fixed total prevents a manifest from causing unbounded contract work.
		if len(requests) >= maxUnifiedAppSelectedOperations {
			return nil, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "too many selected operations"}
		}
		selection, found := selected[unifiedAppSelectionKey{serviceID: operation.ServiceID, versionID: operation.ServiceVersionID}]
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
		expected[index] = unifiedAppExpectedEndpoint{id: operation.EndpointID, name: operation.Operation}
	}
	return requests, expected, nil
}

// verifyUnifiedAppEndpointMatches checks that SQL returned one exact selected endpoint for each pin.
func verifyUnifiedAppEndpointMatches(matches []store.ServiceContractEndpointMatch, expected map[int]unifiedAppExpectedEndpoint) error {
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

// validateUnifiedAppSearchable bounds immutable data filters to scalar object paths.
func validateUnifiedAppSearchable(paths []string) error {
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

// writeUnifiedAppBundleError records a bounded control outcome without logging bundle bytes.
func writeUnifiedAppBundleError(writer http.ResponseWriter, span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, "bundle attachment failed")
	span.SetAttributes(attribute.String("outcome", "failed"))
	// Immutable conflicts are distinct from malformed submitted artifacts.
	if errors.Is(err, store.ErrUnifiedAppBundleImmutable) {
		writeSDKConfigError(writer, workspaceConfigHTTPError{status: http.StatusConflict, message: "app bundle is immutable; create a new version"})
		return
	}
	// A version removed or changed between admission and write cannot acquire a bundle.
	if errors.Is(err, store.ErrUnifiedAppBundleNotFound) {
		writeSDKConfigError(writer, workspaceConfigHTTPError{status: http.StatusConflict, message: "app version is no longer available for bundle attachment"})
		return
	}
	writeSDKConfigError(writer, err)
}
