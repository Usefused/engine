package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	mcpSearchToolName          = "search_docs"
	maxMCPSearchTelemetryCount = 100_000
)

// mcpSearchObservation retains only Engine-owned span state and a bounded start time.
type mcpSearchObservation struct {
	span    trace.Span
	started time.Time
}

// startMCPSearchObservation starts one canonical agent search span without retaining tool values.
func startMCPSearchObservation(ctx context.Context, request mcpJSONRPCRequest, sess *mcpSession) *mcpSearchObservation {
	mode, ok := mcpSearchMode(request.Params)
	// Other tools never enter the documentation-search telemetry path.
	if !ok {
		return nil
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.sandbox.mcp.search_docs")
	physicalCount, unifiedCount := mcpFixtureKindCounts(sess.fixture)
	// Every mode except the catalogue list may return callable contract material.
	span.SetAttributes(
		attribute.String("actor.type", "agent"),
		attribute.String("mcp.transport", boundedMCPSearchTransport(sess.transport)),
		attribute.String("mcp.search.mode", mode),
		attribute.Int("mcp.search.physical_count", physicalCount),
		attribute.Int("mcp.search.unified_count", unifiedCount),
		attribute.Bool("mcp.search.include_detail", mode != "list"),
	)
	return &mcpSearchObservation{span: span, started: time.Now()}
}

// mcpSearchMode recognizes only the closed public discovery modes and discards every argument value.
func mcpSearchMode(raw json.RawMessage) (string, bool) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Query       string `json:"query"`
			OperationID string `json:"operationId"`
			Section     string `json:"section"`
			SchemaPath  string `json:"schemaPath"`
		} `json:"arguments"`
	}
	// An unreadable params object is not safely identifiable as search_docs.
	if json.Unmarshal(raw, &call) != nil || call.Name != mcpSearchToolName {
		return "", false
	}
	// Either lazy field routes through section mode, including invalid requests
	// that the runtime will reject for omitting its operation or section pair.
	if strings.TrimSpace(call.Arguments.Section) != "" || strings.TrimSpace(call.Arguments.SchemaPath) != "" {
		return "section", true
	}
	// Exact operation detail takes precedence over a simultaneous fuzzy query.
	if strings.TrimSpace(call.Arguments.OperationID) != "" {
		return "operation", true
	}
	// Empty query text has the runtime's list semantics.
	if strings.TrimSpace(call.Arguments.Query) != "" {
		return "query", true
	}
	return "list", true
}

// mcpFixtureKindCounts projects only bounded catalogue cardinalities already held by Engine.
func mcpFixtureKindCounts(fixture *Fixture) (int, int) {
	// A missing fixture is reported as an empty catalogue rather than exposing runtime state.
	if fixture == nil {
		return 0, 0
	}
	unified := 0
	// An absent descriptor is the canonical empty Unified catalogue.
	if fixture.UnifiedOperations != nil {
		unified = len(fixture.UnifiedOperations.Operations)
	}
	return min(len(fixture.Operations), maxMCPSearchTelemetryCount), min(unified, maxMCPSearchTelemetryCount)
}

// boundedMCPSearchTransport collapses unexpected session values to one stable enum.
func boundedMCPSearchTransport(transport string) string {
	// Only Engine-owned transport constants are safe low-cardinality dimensions.
	switch transport {
	case "sse", mcpStreamableTransport:
		return transport
	default:
		return "unknown"
	}
}

// finishMCPSearchObservation records response metadata without parsing documentation content.
func finishMCPSearchObservation(observation *mcpSearchObservation, response, boundaryError string) {
	outcome, errorCode := mcpSearchOutcome(response, boundaryError)
	// Search duration is bounded by its call budget, independently of the session's age.
	duration := min(time.Since(observation.started).Milliseconds(), int64(cfg.Sandbox.ToolCallTimeoutSeconds)*1000)
	responseBytes := min(len(response), maxMCPPhysicalResultBytes)
	observation.span.SetAttributes(
		attribute.Int("mcp.search.response_bytes", responseBytes),
		attribute.Int64("mcp.search.duration_ms", duration),
		attribute.String("outcome", outcome),
	)
	// Only failed searches carry a stable closed error code.
	if errorCode != "" {
		observation.span.SetAttributes(attribute.String("error.code", errorCode))
		observation.span.SetStatus(codes.Error, errorCode)
	}
	observation.span.End()
}

// finishMCPSearchSession drains in-flight observations before the child runtime disappears.
func finishMCPSearchSession(sess *mcpSession) {
	sess.pendingMu.Lock()
	observations := make([]*mcpSearchObservation, 0, len(sess.searchTelemetry))
	// Copy-and-clear guarantees later timeout races cannot end the same span twice.
	for callID, observation := range sess.searchTelemetry {
		observations = append(observations, observation)
		delete(sess.searchTelemetry, callID)
	}
	sess.pendingMu.Unlock()
	// Span completion runs outside the session lock so exporters cannot delay request bookkeeping.
	for _, observation := range observations {
		finishMCPSearchObservation(observation, "", "session_ended")
	}
}

// mcpSearchOutcome classifies only the JSON-RPC envelope and never opens tool content.
func mcpSearchOutcome(response, boundaryError string) (string, string) {
	// Engine-owned transport failures take precedence over any absent child response.
	if boundaryError != "" {
		return "failed", boundedMCPSearchError(boundaryError)
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	// Invalid child output is a protocol failure and raw parser text is discarded.
	if json.Unmarshal([]byte(response), &envelope) != nil {
		return "failed", "protocol_error"
	}
	// A JSON-RPC error object is observable without inspecting its message or data.
	if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
		return "failed", "jsonrpc_error"
	}
	// MCP tool handlers use isError for bounded application-level failures.
	if envelope.Result.IsError {
		return "failed", "tool_error"
	}
	return "succeeded", ""
}

// boundedMCPSearchError admits only Engine-owned failure codes into telemetry.
func boundedMCPSearchError(code string) string {
	// Callers cannot expand the telemetry vocabulary through arbitrary errors.
	switch code {
	case "dispatch_failed", "tool_call_timeout", "runtime_unavailable", "session_ended":
		return code
	default:
		return "runtime_unavailable"
	}
}
