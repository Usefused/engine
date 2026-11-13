package api

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type appTryRequirement struct {
	ServiceID   uuid.UUID `json:"service_id"`
	Service     string    `json:"service"`
	BucketID    uuid.UUID `json:"bucket_id"`
	BucketName  string    `json:"bucket_name"`
	AuthName    string    `json:"auth_name"`
	AuthType    string    `json:"auth_type"`
	Credentials string    `json:"credentials"`
	Connection  string    `json:"connection"`
}
type appTryReadiness struct {
	Requirements   []appTryRequirement  `json:"requirements"`
	RuntimeBuckets []appReadinessBucket `json:"runtime_buckets"`
	Note           string               `json:"note,omitempty"`
}

// AppTryReadinessHandler inspects exact deployed scope without running providers or reading credential values.
func AppTryReadinessHandler(s store.Store, contracts appOpenAPIContractStore) http.HandlerFunc {
	// Every refresh rechecks the authenticated actor and immutable app identity.
	return func(w http.ResponseWriter, r *http.Request) {
		// Connection metadata must never enter shared caches, including failed inspections.
		w.Header().Set("Cache-Control", "no-store")
		actor, app, err := lifecycleActorAndApp(r.Context(), s, r)
		// Tenant failures must precede all contract and credential metadata reads.
		if err != nil {
			writeSDKConfigError(w, err)
			return
		}
		result, err := inspectAppTry(r.Context(), s, contracts, actor, app, r.URL.Query().Get("operation"), r.URL.Query().Get("bucket_id"), r.URL.Query().Get("end_user_ref"))
		// Readiness outages remain visible instead of becoming an empty successful result.
		if err != nil {
			writeSDKConfigError(w, err)
			return
		}
		writeJSON(w, result)
	}
}

// inspectAppTry resolves immutable requirements before inspecting mutable credentials in their real buckets.
func inspectAppTry(ctx context.Context, s store.Store, contracts appOpenAPIContractStore, actor accesscontrol.Actor, app *store.App, operation, selectedBucket, userRef string) (*appTryReadiness, error) {
	family, err := loadAppOpenAPIFamily(ctx, s, actor.AccountID, app)
	// Only supported runnable versions may expose executable connection requirements.
	if err != nil {
		return nil, err
	}
	kind := string(family.Kind)
	// REST and package apps share storage identity but have distinct read permissions.
	if family.Kind == store.AppKindSDK {
		kind = string(family.DeliveryMode)
	}
	permission := accesscontrol.AppPermission(kind, accesscontrol.PermissionAppRead)
	// The handler repeats app authorization so direct mounts cannot bypass the control router.
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}}); err != nil {
		return nil, err
	}
	// Reject unbounded selectors before contract or connection queries.
	if invalidTrySelection(operation, userRef) {
		return nil, workspaceConfigHTTPError{status: 400, message: "invalid readiness selection"}
	}
	selections, names, err := appTrySelections(ctx, contracts, app, family, operation)
	// Unselected or ambiguous operations cannot probe another service's credentials.
	if err != nil {
		return nil, err
	}
	buckets, runtime, err := appTryBuckets(ctx, s, app, selectedBucket)
	// Unapproved routing cannot reveal any credential metadata.
	if err != nil {
		return nil, err
	}
	result := &appTryReadiness{Requirements: []appTryRequirement{}, RuntimeBuckets: []appReadinessBucket{}}
	for _, bucket := range runtime {
		result.RuntimeBuckets = append(result.RuntimeBuckets, appReadinessBucket{ID: bucket.ID.String(), Name: bucket.Name})
	}
	result.Requirements, err = appTryMaterial(ctx, s, actor, buckets, selections, names, userRef)
	// Map-backed grouping must not reorder the readiness panel on every refresh.
	sort.Slice(result.Requirements, func(i, j int) bool {
		return result.Requirements[i].Service+result.Requirements[i].AuthName < result.Requirements[j].Service+result.Requirements[j].AuthName
	})
	// Authored code may choose different users and child apps; static inspection cannot predict those decisions.
	if family.Kind == store.AppKindUnifiedApp {
		result.Note = "Connections shown cover declared provider services. The app chooses connected users from its code and input; child apps keep their own buckets."
	}
	return result, err
}

// appTrySelections narrows REST readiness to the selected operation and keeps authored app dependencies intact.
func appTrySelections(ctx context.Context, contracts appOpenAPIContractStore, app *store.App, family *store.AppFamily, operation string) ([]models.SDKSelection, map[uuid.UUID]string, error) {
	selections, err := decodeAppOpenAPISelections(app.Selections)
	// Invalid immutable selections must fail before any mutable metadata query.
	if err != nil {
		return nil, nil, err
	}
	filter := operation
	// Authored execution may use every declared physical dependency.
	if family.Kind == store.AppKindUnifiedApp {
		// Hosted execution has one authored entry point, not an arbitrary provider-operation alias.
		if operation != "execute" {
			return nil, nil, workspaceConfigHTTPError{status: 400, message: "select the app execute operation"}
		}
		filter = ""
	}
	// Physical contracts are required even when there is no live Registry connection.
	if contracts == nil {
		return nil, nil, unavailableAppOpenAPISchemaError()
	}
	matches, err := loadAppOpenAPIEndpointMatches(ctx, contracts, selections, filter)
	// Missing immutable contracts must remain an inspection failure.
	if err != nil {
		return nil, nil, err
	}
	// Ambiguous names must never combine requirements from unrelated operations.
	if filter != "" && len(matches) != 1 {
		return nil, nil, workspaceConfigHTTPError{status: 400, message: "select one physical REST operation"}
	}
	return appTrySelectionAuth(ctx, contracts, selections, matches)
}

// appTrySelectionAuth shares the planner's auth resolver using one immutable metadata batch.
func appTrySelectionAuth(ctx context.Context, contracts appOpenAPIContractStore, selections []models.SDKSelection, matches []store.ServiceContractEndpointMatch) ([]models.SDKSelection, map[uuid.UUID]string, error) {
	grouped := make(map[int][]sandbox.OperationSecuritySummary)
	for _, match := range matches {
		grouped[match.SelectionIndex] = append(grouped[match.SelectionIndex], sandbox.OperationSecuritySummary{Name: match.Endpoint.Name, SecurityRequirements: match.Endpoint.SecurityRequirements})
	}
	refs := make([]store.ServiceContractMetadataRef, 0, len(grouped))
	for index := range grouped {
		selection := selections[index]
		refs = append(refs, store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID})
	}
	names := make(map[uuid.UUID]string)
	// Apps with only child/local capabilities have no direct provider material to inspect.
	if len(refs) == 0 {
		return []models.SDKSelection{}, names, nil
	}
	repository, ok := contracts.(store.ServiceContractMetadataBatchStore)
	// Readiness requires the same authoritative contract snapshot as execution.
	if !ok {
		return nil, nil, unavailableAppOpenAPISchemaError()
	}
	metadata, err := repository.ListServiceContractMetadata(ctx, refs)
	// Store failure cannot downgrade an authenticated operation to anonymous.
	if err != nil {
		return nil, nil, unavailableAppOpenAPISchemaError()
	}
	result := make([]models.SDKSelection, 0, len(grouped))
	for index, operations := range grouped {
		selection := selections[index]
		ref := store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
		meta := metadata[ref]
		// Exact-version metadata prevents silently selecting a newer auth scheme.
		if !validTryServiceMetadata(meta, ref) {
			return nil, nil, unavailableAppOpenAPISchemaError()
		}
		// Invalid or ambiguous auth policy cannot produce a partial readiness result.
		if err := resolveSelectionAuthPolicy(&selection, sandbox.ServiceVersionExecutionAuthContract{AuthConfigs: meta.AuthConfigs, Operations: operations}, &sdkAuthResolutionTelemetry{}); err != nil {
			return nil, nil, err
		}
		names[selection.ServiceID] = meta.Name
		result = append(result, selection)
	}
	return result, names, nil
}

// appTryBuckets preserves service overrides unless an explicit deployed runtime bucket was selected.
func appTryBuckets(ctx context.Context, s store.Store, app *store.App, selected string) (appBucketSet, []store.Bucket, error) {
	repository, ok := s.(store.UnifiedAppRuntimeBucketScope)
	// This endpoint needs authoritative deployed scope, never a workspace-wide bucket picker.
	if !ok {
		return appBucketSet{}, nil, unavailableAppOpenAPISchemaError()
	}
	runtime, err := repository.ReadUnifiedAppRuntimeBuckets(ctx, app.AccountID, app.AppID)
	// Scope must come from the deployed version even when configured routing is requested.
	if err != nil {
		return appBucketSet{}, nil, err
	}
	// An explicit override replaces all direct provider bindings for this inspection only.
	if selected != "" {
		for _, bucket := range runtime {
			// Exact identity prevents deleted/recreated buckets inheriting an old deployment grant.
			if bucket.ID.String() == selected {
				return appBucketSet{Default: bucket}, runtime, nil
			}
		}
		return appBucketSet{}, nil, workspaceConfigHTTPError{status: 403, message: "bucket selection is not permitted"}
	}
	buckets, err := appTryConfiguredBuckets(ctx, s, app)
	return buckets, runtime, err
}

// appTryCanRead keeps metadata permissions independent from the ability to execute with an app token.
func appTryCanRead(ctx context.Context, actor accesscontrol.Actor, bucket uuid.UUID, permission accesscontrol.Permission) bool {
	return (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucket}}) == nil
}

// appTryMaterial batches material by bucket and connection health across all exact service/auth tuples.
func appTryMaterial(ctx context.Context, s store.Store, actor accesscontrol.Actor, buckets appBucketSet, selections []models.SDKSelection, names map[uuid.UUID]string, userRef string) ([]appTryRequirement, error) {
	grouped := make(map[uuid.UUID][]models.SDKSelection)
	for _, selection := range selections {
		bucket := buckets.forService(selection.ServiceID)
		grouped[bucket.ID] = append(grouped[bucket.ID], selection)
	}
	result := []appTryRequirement{}
	requests := []store.AppTryConnectionRequest{}
	indices := []int{}
	for bucketID, group := range grouped {
		items, err := appTryBucketMaterial(ctx, s, actor, bucketID, group, buckets, names)
		// Failures must not produce a partial green readiness list.
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			// Only authorized OAuth requirements enter the connection metadata batch.
			if item.Connection == "user_required" {
				requests = append(requests, store.AppTryConnectionRequest{BucketID: bucketID, ServiceID: item.ServiceID, AuthName: item.AuthName, AuthType: item.AuthType, EndUserRef: userRef})
				indices = append(indices, len(result))
			}
			result = append(result, item)
		}
	}
	return appTryConnectionStates(ctx, s, result, requests, indices)
}

// appTryBucketMaterial exposes only readiness flags and setup identities, never secret keys or values.
func appTryBucketMaterial(ctx context.Context, s store.Store, actor accesscontrol.Actor, bucketID uuid.UUID, selections []models.SDKSelection, buckets appBucketSet, names map[uuid.UUID]string) ([]appTryRequirement, error) {
	canRead := appTryCanRead(ctx, actor, bucketID, accesscontrol.PermissionCredentialsMetadataRead)
	ready, keys := map[string]bool{}, map[string]bool{}
	// App read authority alone must not query credential presence.
	if canRead {
		var err error
		ready, keys, err = loadAppBucketMaterial(ctx, s, bucketID, selections)
		// An outage differs from a permission-limited unknown state.
		if err != nil {
			return nil, err
		}
	}
	result := []appTryRequirement{}
	for _, selection := range selections {
		for _, required := range selection.RequiredAuth {
			item := appTryRequirement{ServiceID: selection.ServiceID, Service: names[selection.ServiceID], BucketID: bucketID, BucketName: buckets.forService(selection.ServiceID).Name, AuthName: required.AuthName, AuthType: required.AuthType, Credentials: "unknown", Connection: "not_required"}
			item.Credentials = appTryCredentialState(selection, required, canRead, names, ready, keys)
			// OAuth application setup and end-user consent are separate readiness dimensions.
			if required.AuthType == "oauth" || required.AuthType == "oidc" {
				item.Connection = "unknown"
				// Consent metadata has an independent permission from application credentials.
				if appTryCanRead(ctx, actor, bucketID, accesscontrol.PermissionConnectionRead) {
					item.Connection = "user_required"
				}
			}
			result = append(result, item)
		}
	}
	return result, nil
}

// isTryPhysicalOperation excludes attachment/local aliases from provider bucket overrides.
func isTryPhysicalOperation(operation string) bool {
	return !strings.Contains(operation, ":")
}

// invalidTrySelection bounds untrusted selectors before immutable contract lookup.
func invalidTrySelection(operation, userRef string) bool {
	return operation == "" || len(operation) > maxRESTOperationBytes || len(userRef) > maxRESTSelectorBytes
}

// validTryServiceMetadata binds authentication to the exact immutable service version.
func validTryServiceMetadata(meta *fusedobject.ServiceMetadata, ref store.ServiceContractMetadataRef) bool {
	return meta != nil && meta.ID == ref.ServiceID && meta.ServiceVersionID == ref.ServiceVersionID
}

// appTryConfiguredBuckets loads family defaults and overrides in bounded batches without mutating them.
func appTryConfiguredBuckets(ctx context.Context, s store.Store, app *store.App) (appBucketSet, error) {
	scope, err := s.GetAppRuntime(ctx, app.AppID)
	// A missing runtime cannot safely choose any default bucket.
	if err != nil || scope == nil {
		return appBucketSet{}, unavailableAppOpenAPISchemaError()
	}
	bindings, err := s.ListAppFamilyServiceBuckets(ctx, app.AppFamilyID)
	// Failed override lookup must not fall back to the family default.
	if err != nil {
		return appBucketSet{}, err
	}
	buckets, err := s.ListBucketsForAppFamily(ctx, app.AppFamilyID)
	// Names and identities must come from the same family bindings.
	if err != nil {
		return appBucketSet{}, err
	}
	byID := make(map[uuid.UUID]store.Bucket)
	for _, bucket := range buckets {
		byID[bucket.ID] = bucket
	}
	result := appBucketSet{Default: byID[scope.BucketID], Overrides: make(map[uuid.UUID]store.Bucket)}
	for service, binding := range bindings {
		bucket, exists := byID[binding.BucketID]
		// A missing override must not inspect the zero bucket or silently use the default.
		if !exists {
			return appBucketSet{}, unavailableAppOpenAPISchemaError()
		}
		result.Overrides[service] = bucket
	}
	// Missing bindings cannot fall through to another bucket's credentials.
	if result.Default.ID == uuid.Nil {
		return appBucketSet{}, unavailableAppOpenAPISchemaError()
	}
	return result, nil
}

// appTryConnectionStates preserves batch ordering and treats any incomplete metadata result as unavailable.
func appTryConnectionStates(ctx context.Context, s store.Store, result []appTryRequirement, requests []store.AppTryConnectionRequest, indices []int) ([]appTryRequirement, error) {
	// No authorized OAuth requirements means there is no connection query to make.
	if len(requests) == 0 {
		return result, nil
	}
	repository, ok := s.(store.AppTryConnectionStore)
	// Unsupported stores report unavailable rather than fabricating consent health.
	if !ok {
		return nil, unavailableAppOpenAPISchemaError()
	}
	states, err := repository.ReadAppTryConnections(ctx, requests)
	// Every requested tuple needs one result to keep positional mapping trustworthy.
	if err != nil || len(states) != len(indices) {
		return nil, unavailableAppOpenAPISchemaError()
	}
	for index, state := range states {
		result[indices[index]].Connection = state
	}
	return result, nil
}

// appTryCredentialState checks every member of an atomic credential requirement without revealing values.
func appTryCredentialState(selection models.SDKSelection, required models.SDKRequiredAuth, canRead bool, names map[uuid.UUID]string, ready, keys map[string]bool) string {
	// Managed setup replaces only its selected OAuth family, never an additional AND requirement.
	if selection.ManagedAuth && required.AuthName == selection.AuthName && required.AuthType == selection.AuthType {
		return "managed"
	}
	// Execution authority does not grant secret metadata visibility.
	if !canRead {
		return "unknown"
	}
	exact := selection
	exact.RequiredAuth = []models.SDKRequiredAuth{required}
	// Complete pairs are mandatory; a partially configured credential cannot be called ready.
	if len(missingAppBucketMaterial(exact, names, ready, keys)) > 0 {
		return "missing"
	}
	return "ready"
}
