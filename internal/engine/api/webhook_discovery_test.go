package api

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
)

// TestWebhookDiscoveryURLs distinguishes direct ingress from broker receivers without leaking secret references.
func TestWebhookDiscoveryURLs(t *testing.T) {
	t.Setenv("FUSED_ENGINE_PUBLIC_URL", "https://engine.example/base")
	for _, tc := range []struct {
		name          string
		row           store.WorkspaceWebhook
		wantURL, mode string
	}{
		{"registered", store.WorkspaceWebhook{Slug: "route", CallbackURL: "https://custom.example/webhook/route"}, "https://custom.example/webhook/route", "direct"},
		{"configured", store.WorkspaceWebhook{Slug: "route"}, "https://engine.example/base/webhook/route", "direct"},
		{"managed", store.WorkspaceWebhook{Slug: "route", CallbackURL: "https://must-not-advertise.example", RelayConfig: json.RawMessage(`{"source":{"bucket":"private","connection_id":"private"}}`)}, "", "managed"},
		{"invalid", store.WorkspaceWebhook{Slug: "route", RelayConfig: json.RawMessage(`{`)}, "", "unknown"},
	} {
		// Each subtest exercises the production projection boundary without contacting a provider.
		t.Run(tc.name, func(t *testing.T) {
			tc.row.SecretRef = "${bucket.private.secret.must_not_expose}"
			item := projectGraphQLWorkspaceWebhooks([]store.WorkspaceWebhook{tc.row})[0]
			// Advertised routing must match the trusted registration or configured public host exactly.
			if item["callback_url"] != tc.wantURL || item["delivery_mode"] != tc.mode {
				t.Fatalf("unexpected discovery destination for %s", tc.name)
			}
			encoded, _ := json.Marshal(item)
			// GraphQL service readers receive no credential reference or relay authority.
			if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "must_not_expose") {
				t.Fatal("private registration metadata leaked")
			}
		})
	}
	t.Setenv("FUSED_ENGINE_PUBLIC_URL", "")
	url, mode := webhookDiscoveryDestination(store.WorkspaceWebhook{Slug: "route"})
	// Missing public configuration must not be replaced with a guessed browser or request origin.
	if url != "" || mode != "direct" {
		t.Fatal("invented a public webhook URL")
	}
}

// TestWebhookDiscoveryServicePermission proves direct GraphQL requests cannot bypass URL visibility rules.
func TestWebhookDiscoveryServicePermission(t *testing.T) {
	accountID, workspaceID, serviceID := uuid.New(), uuid.New(), uuid.New()
	fixture := &workspaceTestStore{accountID: accountID, workspaceID: workspaceID, listWorkspaceWebhooksResult: []store.WorkspaceWebhook{{Slug: "private-route", CallbackURL: "https://engine.example/webhook/private-route"}}}
	handler := mcpGraphQLHandler(authorizationTestSchema(t, fixture))
	for _, allowed := range []bool{false, true} {
		grantedService := uuid.New()
		// A read grant for another service does not authorize this registration's receiving URL.
		if allowed {
			grantedService = serviceID
		}
		actor := actorWithResourcePermissions(t, workspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: grantedService}})
		actor.AccountID = accountID
		body, _ := json.Marshal(map[string]any{"query": `query { workspaceWebhooks(service_id:"` + serviceID.String() + `"){ callback_url slug } }`})
		request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
		request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
		response := httptest.NewRecorder()
		handler(response, request)
		// Checking the actual payload covers aliases and direct API clients as well as the visible UI.
		if strings.Contains(response.Body.String(), "private-route") != allowed {
			t.Fatalf("URL permission mismatch (allowed=%v): %s", allowed, response.Body.String())
		}
	}
}

// TestWebhookSecretTargetRequiresCredentialMetadata proves service readers cannot discover bucket names or secret variables.
func TestWebhookSecretTargetRequiresCredentialMetadata(t *testing.T) {
	bucketID, workspaceID := uuid.New(), uuid.New()
	registration := store.WorkspaceWebhook{SecretBucketID: &bucketID, SecretRef: "${bucket.private.secret.signing_key}"}
	for _, allowed := range []bool{false, true} {
		grants := []accesscontrol.Grant{{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}}}
		// Bucket read alone does not disclose credential metadata.
		if allowed {
			grants = append(grants, accesscontrol.Grant{Permission: accesscontrol.PermissionCredentialsMetadataRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}})
		}
		actor := actorWithResourcePermissions(t, workspaceID, grants...)
		target := webhookSigningSecretTarget(accesscontrol.ContextWithActor(context.Background(), actor), registration)
		if !allowed && target != nil {
			t.Fatal("credential location leaked to bucket reader")
		}
		if allowed && (target["bucket_id"] != bucketID.String() || target["key_name"] != "signing_key") {
			t.Fatal("authorized credential link missing")
		}
	}
}
