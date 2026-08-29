package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// TestCheckSDKFamilyCapacityReportsEntitlementError keeps SDK quota failures
// actionable and distinct from authorization failures in CLI output.
func TestCheckSDKFamilyCapacityReportsEntitlementError(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxSDKFamilies: models.IntPtr(0)})
	t.Cleanup(entitlement.LiveEntitlement.Reset)

	err := checkSDKFamilyCapacity(context.Background(), &workspaceTestStore{accountID: accountID}, trace.SpanFromContext(context.Background()), accountID, "replacement")
	assertSDKFamilyLimitError(t, err)
}

// assertSDKFamilyLimitError verifies both the internal classification and the public wire envelope.
func assertSDKFamilyLimitError(t *testing.T, err error) {
	t.Helper()
	assertSDKFamilyLimitType(t, err)
	assertSDKFamilyLimitResponse(t, err)
}

// assertSDKFamilyLimitType verifies the admission layer returns reviewed entitlement metadata.
func assertSDKFamilyLimitType(t *testing.T, err error) {
	t.Helper()
	// A plan ceiling remains HTTP 403 while its stable code identifies entitlement rather than RBAC.
	if err == nil {
		t.Fatal("expected SDK family limit error")
	}
	httpErr, ok := err.(workspaceConfigHTTPError)
	// The public envelope must include bounded quota detail and a concrete recovery path.
	if !ok || httpErr.status != http.StatusForbidden || httpErr.code != "sdk_family_limit_exceeded" || httpErr.category != "entitlement" || httpErr.message != "This workspace has reached its SDK limit (0 of 0)." || httpErr.remediation != "Deactivate all active or deprecated versions of an unused SDK, or upgrade the workspace plan, then retry." {
		t.Fatalf("unexpected SDK family limit error: %#v", err)
	}
}

// assertSDKFamilyLimitResponse verifies HTTP serialization cannot relabel quota as authorization denial.
func assertSDKFamilyLimitResponse(t *testing.T, err error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	writeSDKConfigError(recorder, err)
	var response workspaceConfigErrorResponse
	// The actual wire serializer must preserve quota identity instead of deriving permission_denied from HTTP 403.
	if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &response); decodeErr != nil {
		t.Fatalf("decode SDK family limit response: %v", decodeErr)
	}
	// CLI consumers depend on this explicit code and remediation to avoid misleading RBAC guidance.
	if response.Error.Code != "sdk_family_limit_exceeded" || response.Error.Category != "entitlement" || response.Error.Message != "This workspace has reached its SDK limit (0 of 0)." || response.Error.Remediation != "Deactivate all active or deprecated versions of an unused SDK, or upgrade the workspace plan, then retry." {
		t.Fatalf("unexpected SDK family limit response: %#v", response.Error)
	}
}
