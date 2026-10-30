package sandbox

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type profileConstraintMCPParentFixture struct {
	mcpBridgeFixture
	failure error
}

// ExecuteAttachedBridge models a Bridge result without starting a local device operation.
func (f *profileConstraintMCPParentFixture) ExecuteAttachedBridge(context.Context, auth.RuntimeIdentity, string, json.RawMessage) (json.RawMessage, error) {
	return nil, f.failure
}

// TestMCPProfileConstraintPublicError distinguishes explicit policy denials from offline and uncertain outcomes.
func TestMCPProfileConstraintPublicError(t *testing.T) {
	fixture := &profileConstraintMCPParentFixture{}
	SetMCPCapabilityAdapter(fixture)
	defer SetMCPCapabilityAdapter(nil)
	id := uuid.New()
	admission := &mcpModernAdmission{target: &store.MCPRouteTarget{AppID: id}, identity: auth.RuntimeIdentity{AppID: id, Kind: store.AppKindMCP}, server: FixtureServerMetadata{Name: "mac", Version: "1.0.0"}}
	request := mcpJSONRPCRequest{ID: json.RawMessage(`1`), Params: json.RawMessage(`{"name":"execute","arguments":{"operation":"bridge:volume","input":20}}`)}
	for _, tc := range []struct {
		outcome, code string
		policy        bool
	}{
		{"not_started", "profile_constraint_violation", true},
		{"not_started", "unavailable", false},
		{"not_started", "invalid_input", false},
		{"unknown", "profile_constraint_violation", false},
	} {
		fixture.failure = &bridgehost.InvocationError{Outcome: tc.outcome, Code: tc.code}
		response := httptest.NewRecorder()
		require.True(t, handleBridgeMCPCall(t.Context(), response, httptest.NewRequest("POST", "/mcp", nil), request, admission))
		require.Equal(t, 400, response.Code)
		// Only a trusted not_started policy result earns the policy-specific public explanation.
		if tc.policy {
			require.Contains(t, response.Body.String(), bridgehost.ProfileConstraintMessage)
			require.Contains(t, response.Body.String(), bridgehost.ProfileConstraintCode)
			require.NotContains(t, response.Body.String(), "unavailable")
		} else {
			require.NotContains(t, response.Body.String(), bridgehost.ProfileConstraintMessage)
			require.Contains(t, response.Body.String(), "unavailable")
		}
	}
}
