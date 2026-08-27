package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestGenerateAppTokenPersistenceFailureRemainsServerError rejects classification
// by error wording and keeps unknown database failures behind the safe boundary.
func TestGenerateAppTokenPersistenceFailureRemainsServerError(t *testing.T) {
	repository := &appTokenHandlerStore{createErr: errors.New("duplicate token: postgres://private fsk_secret")}
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+uuid.NewString(), strings.NewReader(`{"name":"agent"}`))
	request = controlTestRequest(request, uuid.New())
	response := httptest.NewRecorder()
	GenerateAppTokenHandler(repository).ServeHTTP(response, request)
	var envelope workspaceConfigErrorResponse
	// Unknown persistence failures retain a stable structured 500 without raw detail.
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusInternalServerError || envelope.Error.Code != "app_token_create_failed" || envelope.Error.Retryable {
		t.Fatalf("unexpected persistence error projection: %d %s", response.Code, response.Body.String())
	}
	// Unknown repository outcomes require state inspection and never borrow a target resource as an operation identity.
	if envelope.Error.Phase != "app_token_generation" || envelope.Error.CommitState != "unknown" || envelope.Error.OperationID != "" || !strings.Contains(envelope.Error.Remediation, "Inspect") {
		t.Fatalf("unexpected mutation metadata: %#v", envelope.Error)
	}
	// Store text and credential-shaped values must never cross the control boundary.
	if strings.Contains(response.Body.String(), "postgres://") || strings.Contains(response.Body.String(), "fsk_secret") {
		t.Fatalf("persistence error leaked private detail: %s", response.Body.String())
	}
}

// TestGenerateAppTokenNameConflictIsActionable exercises the shared lifecycle
// and HTTP boundary without exposing wrapped persistence details or plaintext.
func TestGenerateAppTokenNameConflictIsActionable(t *testing.T) {
	exporter := setupTestTracer(t)
	repository := &appTokenHandlerStore{createErr: fmt.Errorf("postgres://private fsk_secret: %w", store.ErrAppTokenNameConflict)}
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+uuid.NewString(), strings.NewReader(`{"name":"agent"}`))
	request = controlTestRequest(request, uuid.New())
	response := httptest.NewRecorder()
	GenerateAppTokenHandler(repository).ServeHTTP(response, request)
	var envelope workspaceConfigErrorResponse
	// The actual JSON envelope must retain its stable conflict contract.
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	// A conflict is actionable and non-retryable until the caller changes the request.
	if response.Code != http.StatusConflict || envelope.Error.Code != "app_token_name_conflict" || envelope.Error.Retryable {
		t.Fatalf("response = %d %#v", response.Code, envelope.Error)
	}
	// A typed conflict proves storage rejected the write before commit.
	if envelope.Error.Phase != "app_token_generation" || envelope.Error.CommitState != "not_committed" || envelope.Error.OperationID != "" {
		t.Fatalf("unexpected mutation metadata: %#v", envelope.Error)
	}
	// Recovery must require an explicit choice and never suggest automatic rotation.
	if !strings.Contains(envelope.Error.Remediation, "different token name") || !strings.Contains(envelope.Error.Remediation, "explicitly revoke") {
		t.Fatalf("remediation = %q", envelope.Error.Remediation)
	}
	// Wrapped SQL context and generated credential values must remain private.
	for _, forbidden := range []string{"postgres://", "fsk_secret", "fused-app-", repository.createdIssue.TokenHash} {
		if strings.Contains(response.Body.String(), forbidden) { // Inspect the complete serialized response.
			t.Fatalf("response disclosed %q", forbidden)
		}
	}
	// Reuse the existing mutation span instead of adding a second reporting path.
	if !hasSpanWithAttributes(exporter.GetSpans(), "engine.api.app_tokens.generate", map[string]string{"outcome": "conflict"}) {
		t.Fatal("missing conflict outcome")
	}
}
