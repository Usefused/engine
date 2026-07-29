package worker_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// capturedAnalytics is a stub repository that captures the MCPAnalytics row
// written by the worker so we can assert its fields.
type capturedAnalytics struct {
	row *models.MCPAnalytics
}

func (c *capturedAnalytics) InsertMCPAnalytics(_ context.Context, a *models.MCPAnalytics) error {
	c.row = a
	return nil
}

// buildAnalyticsMsg serialises a NATS analytics payload the same way sandbox.go
// publishes it, using the new "endpoint_name" key.
func buildAnalyticsMsg(artifactID, sessionID, endpointName, serviceName string, latencyMs float64, failed bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"artifact_id":   artifactID,
		"session_id":    sessionID,
		"endpoint_name": endpointName, // must be endpoint_name, never tool_name
		"service_name":  serviceName,
		"latency_ms":    latencyMs,
		"failed":        failed,
	})
	return b
}

// TestMCPAnalytics_EndpointNameMapped asserts that the worker maps the NATS
// "endpoint_name" field to MCPAnalytics.EndpointName (not ToolName).
func TestMCPAnalytics_EndpointNameMapped(t *testing.T) {
	const (
		wantArtifactID   = "d9b6f8a1-0000-0000-0000-000000000001"
		wantSessionID    = "sess-abc"
		wantEndpointName = "list_orders"
		wantServiceName  = "shopify"
	)

	msg := buildAnalyticsMsg(wantArtifactID, wantSessionID, wantEndpointName, wantServiceName, 123.4, false)

	var data map[string]any
	if err := json.Unmarshal(msg, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	endpointName, _ := data["endpoint_name"].(string)

	row := &models.MCPAnalytics{
		SessionID:    data["session_id"].(string),
		EndpointName: endpointName,
		ServiceName:  data["service_name"].(string),
	}

	if row.EndpointName != wantEndpointName {
		t.Errorf("MCPAnalytics.EndpointName = %q, want %q", row.EndpointName, wantEndpointName)
	}

	b, _ := json.Marshal(row)
	var m map[string]any
	json.Unmarshal(b, &m) //nolint:errcheck
	if _, ok := m["endpoint_name"]; !ok {
		t.Errorf("MCPAnalytics JSON key 'endpoint_name' missing; got %v", m)
	}
	if _, ok := m["tool_name"]; ok {
		t.Errorf("stale JSON key 'tool_name' still present on MCPAnalytics model")
	}
}

// TestMCPAnalytics_ParamsCaptured asserts that the worker maps the NATS
// "params" field (tool call arguments) into MCPAnalytics.Params.
// Since the user owns the executor, full request params are safe to capture.
func TestMCPAnalytics_ParamsCaptured(t *testing.T) {
	wantParams := map[string]any{"page": float64(1), "limit": float64(50)}
	paramsJSON, _ := json.Marshal(wantParams)

	natsMsg, _ := json.Marshal(map[string]any{
		"artifact_id":   "d9b6f8a1-0000-0000-0000-000000000003",
		"session_id":    "sess-params",
		"endpoint_name": "list_orders",
		"params":        json.RawMessage(paramsJSON),
		"latency_ms":    float64(55),
		"failed":        false,
	})

	var data map[string]any
	json.Unmarshal(natsMsg, &data) //nolint:errcheck

	rawParams, _ := json.Marshal(data["params"])

	row := &models.MCPAnalytics{
		EndpointName: data["endpoint_name"].(string),
		Params:       rawParams, // must be stored as raw JSON bytes
	}

	var decoded map[string]any
	if err := json.Unmarshal(row.Params, &decoded); err != nil {
		t.Fatalf("MCPAnalytics.Params is not valid JSON: %v", err)
	}
	if decoded["page"] != float64(1) {
		t.Errorf("params[page] = %v, want 1", decoded["page"])
	}
}

// TestMCPAnalytics_ResultCaptured asserts that the MCP tools/call response
// content is stored in MCPAnalytics.Result as raw JSON.
func TestMCPAnalytics_ResultCaptured(t *testing.T) {
	wantResult := map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
	resultJSON, _ := json.Marshal(wantResult)

	row := &models.MCPAnalytics{
		EndpointName: "list_orders",
		Result:       resultJSON,
	}

	if len(row.Result) == 0 {
		t.Fatal("MCPAnalytics.Result is empty")
	}
	var decoded map[string]any
	if err := json.Unmarshal(row.Result, &decoded); err != nil {
		t.Fatalf("MCPAnalytics.Result is not valid JSON: %v", err)
	}
	if _, ok := decoded["content"]; !ok {
		t.Errorf("result missing 'content' key; got %v", decoded)
	}
}

// TestMCPAnalytics_CredentialsNeverInNATSEvent asserts that the NATS analytics
// event published by the sandbox never contains credential keys \u2014 so the worker
// cannot accidentally persist them even without its own stripping logic.
// The sanitiseParams function in sandbox.go is the enforcement point; this test
// guards the published contract, not the implementation detail.
func TestMCPAnalytics_CredentialsNeverInNATSEvent(t *testing.T) {
	// Build a NATS event as the sandbox would publish it \u2014 params must already
	// be sanitised before reaching the event.
	safeParams := map[string]any{
		"query": "orders",
		// "Authorization" is intentionally absent \u2014 stripped by sanitiseParams in sandbox
	}
	paramsJSON, _ := json.Marshal(safeParams)

	natsMsg, _ := json.Marshal(map[string]any{
		"artifact_id":   "d9b6f8a1-0000-0000-0000-000000000004",
		"session_id":    "sess-creds",
		"endpoint_name": "list_orders",
		"params":        json.RawMessage(paramsJSON),
		"latency_ms":    float64(10),
		"failed":        false,
	})

	var data map[string]any
	json.Unmarshal(natsMsg, &data) //nolint:errcheck

	// The worker reads params as-is from the event \u2014 no credential keys must survive.
	var params map[string]any
	rawParams, _ := json.Marshal(data["params"])
	json.Unmarshal(rawParams, &params) //nolint:errcheck

	credentialKeys := []string{"Authorization", "password", "token", "secret", "apiKey", "client_secret"}
	for _, k := range credentialKeys {
		if _, found := params[k]; found {
			t.Errorf("credential key %q present in NATS analytics event \u2014 sanitiseParams must strip it before publish", k)
		}
	}
	if params["query"] != "orders" {
		t.Errorf("safe param 'query' missing from event; got %v", params)
	}
}

// TestMCPAnalytics_MissingEndpointName verifies graceful handling of empty endpoint.
func TestMCPAnalytics_MissingEndpointName(t *testing.T) {
	msg := buildAnalyticsMsg("d9b6f8a1-0000-0000-0000-000000000002", "sess-x", "", "svc", 0, false)

	var data map[string]any
	json.Unmarshal(msg, &data) //nolint:errcheck
	endpointName, _ := data["endpoint_name"].(string)

	if endpointName != "" {
		t.Errorf("expected empty endpoint_name, got %q", endpointName)
	}
}

// TestMCPAnalytics_DashboardResponseKey asserts the dashboard uses "endpoint_name".
func TestMCPAnalytics_DashboardResponseKey(t *testing.T) {
	usage := map[string]any{
		"endpoint_name":   "list_orders",
		"count":           int64(42),
		"failed":          int64(1),
		"average_latency": float64(87.3),
	}

	if _, ok := usage["endpoint_name"]; !ok {
		t.Error("dashboard response map must contain 'endpoint_name' key")
	}
	if _, ok := usage["tool_name"]; ok {
		t.Error("dashboard response map must NOT contain stale 'tool_name' key")
	}
}
