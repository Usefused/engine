package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// validateRuntimeBuckets makes a finite allowlist the explicit opt-in; omission preserves static routing.
func validateRuntimeBuckets(doc sdkConfigDocument) error {
	// Omission preserves static routing for every adapter.
	if len(doc.RuntimeBuckets) == 0 {
		return nil
	}
	// Other adapters cannot enforce this capability, and scopes must remain bounded.
	if doc.Kind != "unified_app" || len(doc.RuntimeBuckets) > 32 {
		return fmt.Errorf("runtime_buckets requires a Unified App and at most 32 bucket names")
	}
	seen := make(map[string]bool)
	for _, name := range doc.RuntimeBuckets {
		// Wildcards, aliases and duplicate names cannot broaden the reviewed scope.
		if name == "" || name != strings.TrimSpace(name) || name == "*" || seen[name] {
			return fmt.Errorf("runtime_buckets requires distinct, nonempty bucket names without wildcards")
		}
		seen[name] = true
	}
	return nil
}

// resolveRuntimeBuckets uses one bounded lookup and pins IDs rather than mutable bucket names.
func resolveRuntimeBuckets(ctx context.Context, repository store.Store, doc sdkConfigDocument) ([]store.Bucket, error) {
	// Reuse admission at both plan and delayed apply boundaries.
	if err := validateRuntimeBuckets(doc); err != nil {
		return nil, err
	}
	// Disabled selection needs no bucket lookup or additional authority.
	if len(doc.RuntimeBuckets) == 0 {
		return nil, nil
	}
	buckets, err := repository.GetBucketsByNames(ctx, doc.RuntimeBuckets)
	// Partial resolution is not an acceptable subset of the authored policy.
	if err != nil {
		return nil, err
	}
	// A missing name invalidates the whole reviewed scope rather than narrowing it silently.
	if len(buckets) != len(doc.RuntimeBuckets) {
		return nil, fmt.Errorf("a permitted runtime bucket is unavailable")
	}
	// Stable ordering keeps equivalent authored scopes deterministic in plan payloads.
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	return buckets, nil
}

// runtimeBucketRefs records the reviewed identities in the immutable applied plan.
func runtimeBucketRefs(buckets []store.Bucket) []appResolvedBucketRef {
	refs := make([]appResolvedBucketRef, 0, len(buckets))
	for _, bucket := range buckets {
		refs = append(refs, appResolvedBucketRef{BucketID: bucket.ID, BucketName: bucket.Name})
	}
	return refs
}

// verifyRuntimeBucketPlan rejects deletion/recreation and any mismatch between authored and resolved scope.
func verifyRuntimeBucketPlan(ctx context.Context, repository store.Store, doc sdkConfigDocument, refs []appResolvedBucketRef) error {
	buckets, err := resolveRuntimeBuckets(ctx, repository, doc)
	// Failed lookups cannot revive a stale authorization decision.
	if err != nil {
		return err
	}
	// An added or removed identity requires a fresh authorization plan.
	if len(buckets) != len(refs) {
		return fmt.Errorf("runtime bucket scope changed; create a new plan")
	}
	expected := make(map[string]appResolvedBucketRef, len(refs))
	for _, ref := range refs {
		expected[ref.BucketName] = ref
	}
	for _, bucket := range buckets {
		// An equal name never authorizes a replacement bucket with different identity.
		if expected[bucket.Name].BucketID != bucket.ID {
			return fmt.Errorf("runtime bucket identity changed; create a new plan")
		}
	}
	return nil
}

// authorizeRuntimeBucket checks live authority before every accepted run, including reruns and replays.
func (s *EngineGRPCServer) authorizeRuntimeBucket(ctx context.Context, spec capabilityRunSpec) *restExecutionError {
	// Existing apps and automatic triggers retain their static configuration unless explicitly selected.
	if spec.bucketID == nil {
		return nil
	}
	repository, ok := s.store.(store.UnifiedAppRuntimeBucketStore)
	// Unsupported storage fails closed without starting a worker or returning a pending handle.
	if !ok {
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "runtime bucket authorization is unavailable")
	}
	allowed, err := repository.AuthorizeUnifiedAppRuntimeBucket(ctx, spec.identity.AccountID, spec.identity.AppID, spec.identity.TokenID, *spec.bucketID)
	// Storage failures cannot be interpreted as permission to use configured credentials.
	if err != nil {
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "runtime bucket authorization is unavailable")
	}
	// Absence, cross-account identity, missing opt-in and revoked authority share a value-free denial.
	if !allowed {
		return newRESTExecutionError(http.StatusForbidden, "bucket_selection_denied", "runtime bucket selection is not permitted")
	}
	return nil
}

// validateUnifiedAppRoutingJSON distinguishes omitted controls from explicit null or empty selections.
func validateUnifiedAppRoutingJSON(raw []byte) *restExecutionError {
	var fields map[string]json.RawMessage
	// Canonical parsing normally guarantees this shape; direct callers must also fail closed.
	if err := json.Unmarshal(raw, &fields); err != nil {
		return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "execution controls must be a JSON object")
	}
	for name, value := range fields {
		// JSON's case-insensitive struct matching must not bypass checks on routing controls.
		if !strings.EqualFold(name, "bucket_id") && !strings.EqualFold(name, "expected_app_id") {
			continue
		}
		var id uuid.UUID
		// Explicit absence must never silently fall back to a different credential bucket or version.
		if err := json.Unmarshal(value, &id); err != nil || id == uuid.Nil {
			return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "bucket_id and expected_app_id must be nonzero UUIDs when supplied")
		}
	}
	return nil
}

// validateUnifiedAppExpectedVersion fences optional schema-bound clients against concurrent traffic promotion.
func validateUnifiedAppExpectedVersion(expected *uuid.UUID, actual uuid.UUID) *restExecutionError {
	// Stable URL callers may follow promotions unless they explicitly pin their reviewed version.
	if expected != nil && *expected != actual {
		return newRESTExecutionError(http.StatusConflict, "app_version_not_current", "the requested Unified App version is no longer receiving traffic")
	}
	return nil
}

// isAuthoredUnifiedAppExecution separates the hosted command from raw operations sharing its route.
func isAuthoredUnifiedAppExecution(scope *store.AppRuntime, request restExecutionRequest) bool {
	return scope.Kind == store.AppKindUnifiedApp && request.Operation == "execute"
}

// hasUnifiedAppRoutingControls prevents unsupported operations from silently ignoring invocation authority.
func hasUnifiedAppRoutingControls(request restExecutionRequest) bool {
	return request.BucketID != nil || request.ExpectedAppID != nil
}
