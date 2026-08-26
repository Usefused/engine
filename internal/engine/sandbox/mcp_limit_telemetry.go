package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// mcpLimitObservation carries only fixed policy facts safe for low-cardinality telemetry.
type mcpLimitObservation struct {
	AppID     string
	Transport string
	Kind      string
	Unit      string
	Code      string
	Maximum   int64
}

// recordMCPLimitRejection emits one bounded audit span without rejected content or identity secrets.
func recordMCPLimitRejection(ctx context.Context, observation mcpLimitObservation) {
	_, span := otel.Tracer("engine").Start(ctx, "engine.sandbox.mcp.limit")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.id", observation.AppID),
		attribute.String("mcp.transport", observation.Transport),
		attribute.String("mcp.limit.kind", observation.Kind),
		attribute.String("mcp.limit.unit", observation.Unit),
		attribute.Int64("mcp.limit.maximum", observation.Maximum),
		attribute.String("outcome", "rejected"),
	)
	span.SetStatus(codes.Error, observation.Code)
}

// recordMCPCallLimit classifies only fixed bridge limits and ignores arbitrary provider errors.
func recordMCPCallLimit(ctx context.Context, sess *mcpSession, code string) {
	observation := mcpLimitObservation{AppID: sess.appID, Transport: "runtime_bridge", Unit: "bytes", Code: code}
	// Each public code maps to a compiled ceiling; unknown provider text must not enter telemetry.
	switch code {
	case "mcp_call_payload_too_large":
		observation.Kind, observation.Maximum = "request_payload", maxMCPMessageBodyBytes
	case "mcp_call_result_too_large":
		observation.Kind, observation.Maximum = "physical_result", maxMCPPhysicalResultBytes
	default:
		return
	}
	recordMCPLimitRejection(ctx, observation)
}

// recordMCPMessageLimit distinguishes an external payload ceiling from unrelated body read failures.
func recordMCPMessageLimit(ctx context.Context, appID, transport string, err error) {
	var limitError *http.MaxBytesError
	// Only the bounded reader's typed failure proves that this request exceeded policy.
	if !errors.As(err, &limitError) {
		return
	}
	recordMCPLimitRejection(ctx, mcpLimitObservation{
		AppID: appID, Transport: boundedMCPSearchTransport(transport),
		Kind: "request_payload", Unit: "bytes", Maximum: maxMCPMessageBodyBytes,
		Code: "mcp_message_payload_too_large",
	})
}

// recordMCPRuntimeOutputLimit distinguishes storage admission from model-facing output ceilings in OTEL.
func recordMCPRuntimeOutputLimit(ctx context.Context, sess *mcpSession, response string) {
	code := mcpRuntimeOutputLimitCode(response)
	observation := mcpLimitObservation{
		AppID: sess.appID, Transport: boundedMCPSearchTransport(sess.transport), Unit: "bytes", Code: code,
	}
	// The transport must not turn arbitrary script errors or provider text into telemetry dimensions.
	switch code {
	case "MCP_DOCUMENTATION_OUTPUT_LIMIT_EXCEEDED":
		observation.Kind, observation.Maximum = "documentation_output", 64<<10
	case "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED":
		observation.Kind, observation.Maximum = "execute_result", maxMCPPhysicalResultBytes
	case "MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED":
		observation.Kind, observation.Maximum = "execute_error", mcpEffectiveOutputLimit(response, 8<<10)
	case "MCP_EXECUTE_VISIBLE_OUTPUT_LIMIT_EXCEEDED":
		observation.Kind, observation.Maximum = "execute_visible", mcpEffectiveOutputLimit(response, 64<<10)
	default:
		return
	}
	recordMCPLimitRejection(ctx, observation)
}

// mcpRuntimeOutputLimitCode inspects only a failed single-text tool envelope without retaining its data.
func mcpRuntimeOutputLimitCode(response string) string {
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	// Successful results and invalid envelopes are not evidence of a runtime-output rejection.
	if json.Unmarshal([]byte(response), &envelope) != nil || !envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		return ""
	}
	content := envelope.Result.Content[0]
	// Runtime-owned limit failures use one small JSON text object; larger script errors are ignored.
	if content.Type != "text" || len(content.Text) > 1024 {
		return ""
	}
	var failure struct {
		Code string `json:"code"`
	}
	// Error text need not be JSON, and parser details are never observable here.
	if json.Unmarshal([]byte(content.Text), &failure) != nil {
		return ""
	}
	return failure.Code
}

// mcpSessionStartFailure exposes typed admission failures without leaking startup or schema content.
func mcpSessionStartFailure(err error) (int, string) {
	var admissionError *MCPSchemaAdmissionError
	// Only reviewed schema codes are useful recovery evidence for an otherwise opaque start failure.
	if errors.As(err, &admissionError) {
		return http.StatusUnprocessableEntity, admissionError.Code
	}
	return http.StatusInternalServerError, "failed to establish MCP session"
}

// recordMCPSchemaLimit maps typed schema failures to fixed policy dimensions without schema text.
func recordMCPSchemaLimit(ctx context.Context, appID string, err error) {
	observation, ok := mcpSchemaLimitObservation(appID, err)
	// Invalid-but-not-limited schema failures remain on the existing session-start outcome.
	if !ok {
		return
	}
	recordMCPLimitRejection(ctx, observation)
}

// mcpSchemaLimitObservation translates only known admission ceilings into bounded OTEL facts.
func mcpSchemaLimitObservation(appID string, err error) (mcpLimitObservation, bool) {
	base := mcpLimitObservation{AppID: appID, Transport: "shared_runtime"}
	// Stable typed errors keep authored schema fragments out of both codes and attributes.
	switch {
	case errors.Is(err, ErrMCPSchemaEncodedBytesLimit):
		base.Kind, base.Unit, base.Code, base.Maximum = "schema_bytes", "bytes", mcpSchemaEncodedBytesLimitCode, maxMCPSchemaEncodedBytes
	case errors.Is(err, ErrMCPSchemaDepthLimit):
		base.Kind, base.Unit, base.Code, base.Maximum = "schema_depth", "levels", mcpSchemaDepthLimitCode, maxMCPSchemaDepth
	case errors.Is(err, ErrMCPSchemaNodeLimit):
		base.Kind, base.Unit, base.Code, base.Maximum = "schema_nodes", "nodes", mcpSchemaNodeLimitCode, maxMCPSchemaNodes
	case errors.Is(err, ErrMCPSchemaAggregateNodesLimit):
		base.Kind, base.Unit, base.Code, base.Maximum = "catalogue_schema_nodes", "nodes", mcpSchemaAggregateNodesLimitCode, maxMCPAggregateSchemaNodes
	default:
		return mcpLimitObservation{}, false
	}
	return base, true
}
