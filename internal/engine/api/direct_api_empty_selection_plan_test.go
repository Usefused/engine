package api

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestDirectAPIOpenAPIPlanRejectsUnresolvedScope ensures a compiler digest cannot bypass the physical schema check.
func TestDirectAPIOpenAPIPlanRejectsUnresolvedScope(t *testing.T) {
	generate := false
	doc := sdkConfigDocument{Generate: &generate, BundleDigest: store.ExecutionAppBundleDigest([]byte("greeting"))}
	// A digest is provenance for authored code; it does not make missing provider scope valid.
	if err := validateDirectAPIOpenAPIPlan(context.Background(), nil, doc, uuid.Nil, nil, sdkUnifiedCompilation{}); err == nil {
		t.Fatal("API plan without schema storage was accepted with a digest")
	}
	doc.BundleDigest = ""
	// The same projection requirement applies without authored code.
	if err := validateDirectAPIOpenAPIPlan(context.Background(), nil, doc, uuid.Nil, nil, sdkUnifiedCompilation{}); err == nil {
		t.Fatal("raw API without schema storage was accepted")
	}
}
