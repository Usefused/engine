package connectresource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// TestDiscoveryOAuthTokenPlacementUsesConsentedScheme proves discovery cannot borrow another service scheme's credential destination.
func TestDiscoveryOAuthTokenPlacementUsesConsentedScheme(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{AuthConfigs: fusedobject.AuthConfigs{
		{Name: "first", Type: "oauth2"},
		{Name: "selected", Type: "oauth2", OAuthTokenPlacement: &authrouting.OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "raw"}},
	}}
	placement, err := discoveryTokenPlacement(metadata, []string{"selected"})
	// Only the exact consented scheme authorizes custom delivery.
	if err != nil || placement == nil || placement.Name != "X-Provider-Token" {
		t.Fatalf("placement = %#v err=%v", placement, err)
	}
	placement, err = discoveryTokenPlacement(metadata, []string{"first"})
	// A separate default scheme retains its own Bearer semantics.
	if err != nil || placement != nil {
		t.Fatalf("default placement = %#v err=%v", placement, err)
	}
	// Missing identities cannot silently choose either scheme when custom policy exists.
	if _, err := discoveryTokenPlacement(metadata, []string{"missing"}); err == nil {
		t.Fatal("accepted missing scheme")
	}
}

// TestDiscoverUsesCustomOAuthHeader exercises callback-time resource discovery against a real local provider.
func TestDiscoverUsesCustomOAuthHeader(t *testing.T) {
	// The fake provider requires the token exactly where the selected auth contract declared it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject accidental default-header duplication as well as missing custom delivery.
		if r.Header.Get("X-Provider-Token") != "fresh-token" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect discovery credential headers")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"seller-one"}]`))
	}))
	defer server.Close()
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: server.URL,
		AuthConfigs: fusedobject.AuthConfigs{{Name: "seller", Type: "oauth2", OAuthTokenPlacement: &authrouting.OAuthTokenPlacement{
			Location: "header", Name: "X-Provider-Token", Format: "raw",
		}}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceDiscovery: &fusedobject.ResourceDiscoveryConfig{IDPath: "$[*].id"}},
	}
	resources, err := Discover(context.Background(), metadata, &fusedobject.Endpoint{Method: http.MethodGet, Path: "/sellers"}, "fresh-token", "Bearer", "seller")
	// Only the successfully authenticated response can become a discovered connection resource.
	if err != nil || len(resources) != 1 || resources[0].ProviderID != "seller-one" {
		t.Fatalf("discovery failed: resources=%v err=%v", resources, err)
	}
}
