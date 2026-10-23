package api

import (
	"context"
	"encoding/json"

	"github.com/Usefused/engine/internal/engine/store"
)

// workflowAuthoredServicePins recovers names lost when applied config normalizes service keys to display labels.
func workflowAuthoredServicePins(ctx context.Context, s store.Store, app *store.App, pins []workflowServicePin) ([]workflowServicePin, error) {
	// Apps without an authored bundle have no compiled runtime bindings to recover.
	if app.BundleDigest == "" {
		return pins, nil
	}
	bundles, ok := s.(store.UnifiedAppBundleStore)
	// A missing bundle reader cannot safely guess the names referenced by private source.
	if !ok {
		return nil, workflowSourceIdentityError()
	}
	bundle, err := bundles.GetUnifiedAppBundle(ctx, app.AppID)
	// Exact source and content identity are required before exporting compiled aliases.
	if err != nil || bundle == nil || bundle.AppID != app.AppID || bundle.SourceHash != app.SourceHash || store.UnifiedAppBundleDigest([]byte(bundle.BundleJS)) != app.BundleDigest {
		return nil, workflowSourceIdentityError()
	}
	var manifest unifiedAppManifest
	// Corrupt stored declarations must not silently fall back to display names.
	if json.Unmarshal(bundle.Manifest, &manifest) != nil {
		return nil, workflowSourceIdentityError()
	}
	return matchWorkflowAuthoredPins(pins, manifest.SelectedOperations)
}

// matchWorkflowAuthoredPins associates a source name only with the same immutable service and version.
func matchWorkflowAuthoredPins(pins []workflowServicePin, operations []unifiedAppManifestOperation) ([]workflowServicePin, error) {
	result := append([]workflowServicePin(nil), pins...)
	for index, pin := range result {
		for _, operation := range operations {
			// Bindings for sibling services cannot rename this dependency.
			if operation.ServiceID != pin.ServiceID || operation.ServiceVersionID != pin.ServiceVersionID {
				continue
			}
			// A normalized display label cannot represent several distinct authored aliases safely.
			if operation.Service == "" || (result[index].AuthoredKey != "" && result[index].AuthoredKey != operation.Service) {
				return nil, workflowSourceIdentityError()
			}
			result[index].AuthoredKey = operation.Service
		}
	}
	return result, nil
}
