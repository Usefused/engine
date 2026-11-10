package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestWebhookDiagnosticControlBoundary proves the actual route namespace authenticates and authorizes before disclosure.
func TestWebhookDiagnosticControlBoundary(t *testing.T) {
	service, registration, receipt := uuid.New(), uuid.New(), uuid.New()
	path := "/workspace/services/" + service.String() + "/webhooks/" + registration.String() + "/receipts/" + receipt.String() + "/diagnostics"
	for _, tc := range []struct {
		name                      string
		authenticated, diagnostic bool
		status                    int
	}{{"anonymous", false, false, 401}, {"ordinary receipt", true, false, 403}, {"private diagnostic", true, true, 200}} {
		// A fresh principal and audit sink prevent cached authorization from affecting another case.
		t.Run(tc.name, func(t *testing.T) {
			principal := controlTestPrincipal()
			principal.EffectiveGrants = []accesscontrol.Grant{
				{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service}},
				{Permission: accesscontrol.PermissionAuditRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID}},
			}
			// The private grant must be explicitly present, independently of receipt visibility.
			if tc.diagnostic {
				principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{Permission: accesscontrol.PermissionWebhookDiagnosticsRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service}})
			}
			loader := &controlPrincipalLoaderStub{principal: principal}
			authenticator, err := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
			require.NoError(t, err)
			recorder := &controlAuditRecorderStub{}
			calls := 0
			// This synthetic body detects accidental copying by the canonical control audit boundary.
			target := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.Write([]byte(`{"body":"PRIVATE_SENTINEL"}`)) })
			handler := controlActorMiddlewareWithAudit(authenticator, recorder)(controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(target))
			request := httptest.NewRequest(http.MethodGet, path, nil)
			// Anonymous reads cannot be upgraded by knowing a private URL.
			if tc.authenticated {
				request.Header.Set("X-API-Key", "fsk_synthetic")
			}
			require.Equal(t, requestClassControl, classifyEngineRequest(request))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			// Denied actors must never reach the body handler, not merely receive a filtered response.
			if tc.status == 200 {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
			encoded, _ := json.Marshal(recorder.events)
			require.NotContains(t, string(encoded), "PRIVATE_SENTINEL")
		})
	}
}
