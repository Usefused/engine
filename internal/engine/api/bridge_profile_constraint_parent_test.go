package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/stretchr/testify/require"
)

type profileConstraintParentHost struct{ replayHostFixture }

// Fetch models the explicit Bridge response before any app-launch effect.
func (*profileConstraintParentHost) Fetch(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, &bridgehost.InvocationError{Outcome: "not_started", Code: "profile_constraint_violation"}
}

// TestProfileConstraintPublicProjection exercises JavaScript execution and the stored public receipt.
func TestProfileConstraintPublicProjection(t *testing.T) {
	bundle := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>JSON.parse(await __fusedHost.fetch(JSON.stringify({bridge:"mac",input:{app:"Notes"}}))) };`
	host := newReplayTestRecorder(t, &profileConstraintParentHost{})
	_, runErr := executionappvm.RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), host)
	require.Error(t, runErr)
	status, code := capabilityCompletion(runErr, nil, false)
	record := &store.ExecutionResult{Status: status, ErrorCode: code, ErrorMessage: capabilityPublicError(code, runErr)}
	body, err := json.Marshal(projectCapabilityExecution(record, ""))
	require.NoError(t, err)
	require.Contains(t, string(body), bridgehost.ProfileConstraintMessage)
	require.NotContains(t, string(body), "bridge unavailable")
	require.NotContains(t, string(body), "Notes")
	// Replay must reproduce the same public denial without issuing another Bridge call.
	history, err := host.History()
	require.NoError(t, err)
	replay, err := NewReplayCapabilityHost(history)
	require.NoError(t, err)
	_, replayErr := executionappvm.RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), replay)
	require.Equal(t, record.ErrorMessage, capabilityPublicError(code, replayErr))
	// The REST path uses the same category without mislabeling a real disconnection.
	response := httptest.NewRecorder()
	writeRESTExecutionError(response, bridgeRESTExecutionError(&bridgehost.InvocationError{Outcome: "not_started", Code: "profile_constraint_violation"}))
	require.Equal(t, 400, response.Code)
	require.Contains(t, response.Body.String(), bridgehost.ProfileConstraintCode)
	require.Contains(t, response.Body.String(), bridgehost.ProfileConstraintMessage)
	response = httptest.NewRecorder()
	writeRESTExecutionError(response, bridgeRESTExecutionError(&bridgehost.InvocationError{Outcome: "unknown", Code: "unavailable"}))
	require.Contains(t, response.Body.String(), "bridge_unavailable")
	require.NotContains(t, response.Body.String(), bridgehost.ProfileConstraintCode)
}
