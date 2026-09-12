package authrouting

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOAuthTokenPlacementRejectsInvalidHeaderValues prevents transport errors from exposing malformed credentials.
func TestOAuthTokenPlacementRejectsInvalidHeaderValues(t *testing.T) {
	placement := &OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "raw"}
	for _, control := range []byte{0, 1, 10, 13, 31, 127} {
		req := httptest.NewRequest("GET", "https://provider.example", nil)
		token := "private-token" + string(control)
		err := placement.Apply(req, token, "Bearer")
		// Errors must be bounded and leave the request without a credential header.
		if err == nil || strings.Contains(err.Error(), "private-token") || len(req.Header) != 0 {
			t.Fatalf("unsafe rejection for control byte %d", control)
		}
	}
}

// TestOAuthTokenPlacementRejectsInvalidContracts checks header ownership and explicit OAuth2 scope at the shared boundary.
func TestOAuthTokenPlacementRejectsInvalidContracts(t *testing.T) {
	for _, header := range []string{"", "Host", "Cookie", "Connection", "Proxy-Authorization", "X-Fused-Auth", "X-Forwarded-Host", "Sec-Fetch-Site", "Bad Header", "X-Token\r\nInjected"} {
		placement := &OAuthTokenPlacement{Location: "header", Name: header, Format: "raw"}
		// Header-name failures cannot become provider routing changes.
		if err := placement.Validate("oauth2"); err == nil {
			t.Errorf("accepted header %q", header)
		}
	}
	placement := &OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "raw"}
	// Other auth strategies cannot opt into OAuth token semantics through stray metadata.
	if err := placement.Validate("apiKey"); err == nil {
		t.Fatal("accepted placement on API key scheme")
	}
}
