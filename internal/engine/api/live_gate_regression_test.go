package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestDirectAPIDownloadNeverContactsRegistry covers both available and unavailable Registry dependencies.
func TestDirectAPIDownloadNeverContactsRegistry(t *testing.T) {
	// Delivery rejection must not depend on Registry availability.
	for _, packages := range []SDKPackageClient{nil, &sdkPackageClientStub{}} {
		err := serveSDKPackage(context.Background(), httptest.NewRecorder(), "control", accesscontrol.Actor{AccountID: uuid.New()}, uuid.New(), &sdkPackageBuildStoreStub{err: store.ErrSDKPackageNotGenerated}, nil, packages)
		var typed workspaceConfigHTTPError
		// A permanent mode rejection must precede dependency availability and cache-miss generation.
		if !errors.As(err, &typed) || typed.status != http.StatusConflict || typed.code != "sdk_package_not_generated" || typed.retryable {
			t.Fatalf("direct API download: %#v", err)
		}
	}
}

// TestImmutableVersionRejectionPreservesNonCommit proves the apply wrapper cannot erase known admission evidence.
func TestImmutableVersionRejectionPreservesNonCommit(t *testing.T) {
	familyID, appID := uuid.New(), uuid.New()
	fixture := &workspaceTestStore{apps: map[uuid.UUID]store.App{appID: {AppID: appID, AppFamilyID: familyID, Version: "1.0.0", SourceHash: "original"}}}
	_, _, err := reserveSDKVersionIdentity(context.Background(), fixture, uuid.New(), familyID, "1.0.0", "changed")
	wrapped := withWorkspaceConfigErrorMetadata(err, "apply_execution", uuid.NewString(), "unknown")
	var typed workspaceConfigHTTPError
	// Race-time rejection must be safe to replan, not an ambiguous committed mutation.
	if !errors.As(wrapped, &typed) || typed.code != "app_version_immutable" || typed.commitState != "not_committed" || typed.phase != "apply_admission" {
		t.Fatalf("immutable rejection: %#v", wrapped)
	}
}
