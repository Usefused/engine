package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDeploymentRouteOwnsOnlyItsExactPOSTAuthentication prevents broad prefix exclusions while reaching the dedicated grant validator.
func TestDeploymentRouteOwnsOnlyItsExactPOSTAuthentication(t *testing.T) {
	for _, test := range []struct {
		method, path      string
		reachesDeployment bool
	}{
		{http.MethodPost, "/oauth/runtime/deploy", true},
		{http.MethodGet, "/oauth/runtime/deploy", false},
		{http.MethodPost, "/oauth/runtime/deploy/", false},
		{http.MethodPost, "/oauth/runtime/deploy/extra", false},
		{http.MethodPost, "/oauth/runtime/deploy-other", false},
		{http.MethodPost, "/oauth/connected-apps", false},
	} {
		reached := false
		// A nil management authenticator proves the specialized bearer is not processed as a broad control credential.
		handler := controlActorMiddleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			// The dedicated handler must receive the exact bearer so it can enforce its own family ceiling.
			if r.Header.Get("Authorization") != "Bearer fot_deployment" {
				t.Fatal("deployment bearer changed")
			}
			w.WriteHeader(http.StatusTeapot)
		}))
		request := httptest.NewRequest(test.method, test.path, nil)
		request.Header.Set("Authorization", "Bearer fot_deployment")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		// Only the one protocol route can reach its independent authenticator.
		if reached != test.reachesDeployment {
			t.Fatalf("%s %s reached=%v", test.method, test.path, reached)
		}
		expected := http.StatusUnauthorized
		// The sentinel response distinguishes handler-owned rejection from global management rejection.
		if test.reachesDeployment {
			expected = http.StatusTeapot
		}
		if response.Code != expected {
			t.Fatalf("%s %s status=%d", test.method, test.path, response.Code)
		}
	}
}
