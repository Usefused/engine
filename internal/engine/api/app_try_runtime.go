package api

import (
	"context"
	"net/http"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// admitRESTBucket authorizes alternate credentials before dispatch or idempotent response replay.
func (s *EngineGRPCServer) admitRESTBucket(ctx context.Context, scope *store.AppRuntime, identity auth.RuntimeIdentity, request restExecutionRequest) *restExecutionError {
	// Ordinary clients preserve configured routing with no additional store query.
	if !hasUnifiedAppRoutingControls(request) {
		return nil
	}
	// Hosted children and local aliases keep their own credential boundaries; version fences belong to authored execute.
	if scope.Kind != store.AppKindSDK || request.ExpectedAppID != nil || request.BucketID == nil || !isTryPhysicalOperation(request.Operation) {
		return newRESTExecutionError(http.StatusBadRequest, "bucket_selection_unsupported", "bucket selection requires a permitted REST API operation or Unified App execute")
	}
	return s.authorizeRuntimeBucket(ctx, capabilityRunSpec{identity: identity, bucketID: request.BucketID})
}

// addAPIRuntimeBucketSchema advertises the same finite API scope enforced by the shared live authorization query.
func addAPIRuntimeBucketSchema(ctx context.Context, bundles store.UnifiedAppBundleStore, app *store.App, family *store.AppFamily, export *appOpenAPIExport) error {
	// Generated SDKs and hosted MCP retain their configured static routing.
	if family.Kind != store.AppKindSDK || family.DeliveryMode != store.AppDeliveryModeAPI {
		return nil
	}
	repository, ok := bundles.(store.UnifiedAppRuntimeBucketScope)
	// Missing scope support must not publish an incomplete runtime contract.
	if !ok {
		return unavailableAppOpenAPISchemaError()
	}
	buckets, err := repository.ReadUnifiedAppRuntimeBuckets(ctx, app.AccountID, app.AppID)
	// An unavailable allowlist cannot be represented as an empty opt-in.
	if err != nil {
		return err
	}
	// No opt-in means no selector; the UI still offers configured routing.
	if len(buckets) == 0 {
		return nil
	}
	ids, labels := []string{}, map[string]string{}
	for _, bucket := range buckets {
		ids = append(ids, bucket.ID.String())
		labels[bucket.ID.String()] = bucket.Name
	}
	for _, operation := range export.operations {
		// Nested app and Bridge aliases cannot inherit a caller's provider bucket.
		if !isTryPhysicalOperation(operation.name) {
			continue
		}
		properties := operation.requestSchema["properties"].(map[string]any)
		properties["bucket_id"] = map[string]any{"type": "string", "format": "uuid", "enum": ids, "x-fused-bucket-names": labels}
	}
	return nil
}

// admitRESTTraffic fences authored apps against promotion while preserving exact REST API version routing.
func (s *EngineGRPCServer) admitRESTTraffic(ctx context.Context, scope *store.AppRuntime, appID uuid.UUID) *restExecutionError {
	// Only Unified Apps follow a mutable family traffic pointer.
	if scope.Kind == store.AppKindUnifiedApp {
		return s.admitUnifiedAppTraffic(ctx, appID)
	}
	return nil
}
