package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestMCPRefreshSessionOwnership admits reconnection only within the same OAuth rotation family.
func TestMCPRefreshSessionOwnership(t *testing.T) {
	previous := globalTokenValidator
	// Restore shared test wiring even if an assertion fails.
	t.Cleanup(func() { globalTokenValidator = previous })
	appID, grant := uuid.New(), uuid.New()
	session := &mcpSession{appID: appID.String(), tokenID: uuid.New(), oauthGrantID: grant}
	validator := &mcpModernTokenValidator{token: "foat_refreshed", identity: auth.RuntimeIdentity{AppID: appID, TokenID: uuid.New(), OAuthGrantID: grant, Kind: store.AppKindMCP}}
	globalTokenValidator = validator
	// A valid rotation must allow its own client to reinitialize.
	if !rotatedMCPSessionCredential(t.Context(), session, validator.token) {
		t.Fatal("same grant cannot reconnect")
	}
	validator.identity.OAuthGrantID = uuid.New()
	// A session must not be transferable across grants or manual credentials.
	if rotatedMCPSessionCredential(t.Context(), session, validator.token) {
		t.Fatal("another grant claimed session")
	}
	session.oauthGrantID = uuid.Nil
	// A session must not be transferable across grants or manual credentials.
	if rotatedMCPSessionCredential(t.Context(), session, validator.token) {
		t.Fatal("manual token gained cross-token authority")
	}
}
