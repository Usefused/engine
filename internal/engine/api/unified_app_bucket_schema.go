package api

import (
	"context"
	"github.com/Usefused/engine/internal/engine/store"
)

// unifiedAppRequestProperties exposes only explicitly permitted IDs in the version's public request schema.
func unifiedAppRequestProperties(ctx context.Context, bundles store.UnifiedAppBundleStore, app *store.App, input map[string]any) (map[string]any, error) {
	properties := map[string]any{"operation": map[string]any{"type": "string", "const": "execute"}, "input": input, "expected_app_id": map[string]any{"type": "string", "format": "uuid", "const": app.AppID.String(), "description": "Optional version fence; rejects execution if traffic changed after this contract was read."}}
	repository, ok := bundles.(store.UnifiedAppRuntimeBucketScope)
	// Legacy test adapters without scope storage describe only their static execution contract.
	if !ok {
		return properties, nil
	}
	buckets, err := repository.ReadUnifiedAppRuntimeBuckets(ctx, app.AccountID, app.AppID)
	if err != nil {
		return nil, err
	}
	if len(buckets) == 0 {
		return properties, nil
	}
	ids := make([]string, 0, len(buckets))
	labels := make(map[string]string, len(buckets))
	for _, bucket := range buckets {
		ids = append(ids, bucket.ID.String())
		labels[bucket.ID.String()] = bucket.Name
	}
	properties["bucket_id"] = map[string]any{"type": "string", "format": "uuid", "enum": ids, "x-fused-bucket-names": labels, "description": "Optional runtime bucket for every provider call in this execution. Omit to retain configured default and service overrides. Live authorization is checked on every run."}
	return properties, nil
}
