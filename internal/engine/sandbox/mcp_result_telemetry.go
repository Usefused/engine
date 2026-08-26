package sandbox

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// mcpResultDelivery contains only trusted runtime counters, never result references or content.
type mcpResultDelivery struct {
	Delivery          string `json:"delivery"`
	RetainedReads     int    `json:"retained_reads"`
	UnavailableReads  int    `json:"unavailable_reads"`
	OutputBudgetBytes int    `json:"output_budget_bytes"`
	ExecutionOutcome  string `json:"execution_outcome"`
}

// recordMCPRuntimeResultDelivery audits delivery and invocation termination without another provider receipt.
func recordMCPRuntimeResultDelivery(ctx context.Context, sess *mcpSession, response string) {
	observation, ok := mcpRuntimeResultDelivery(response)
	// Older runtimes and non-execute results have no trusted delivery metadata.
	if !ok {
		return
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.sandbox.mcp.result_delivery")
	defer span.End()
	span.SetAttributes(
		attribute.String("actor.type", "agent"),
		attribute.String("mcp.transport", boundedMCPSearchTransport(sess.transport)),
		attribute.String("mcp.result.delivery", observation.Delivery),
		attribute.Int("mcp.result.retained_reads", observation.RetainedReads),
		attribute.Int("mcp.result.unavailable_reads", observation.UnavailableReads),
	)
	// Older runtime responses omit this metadata; only validated explicit budgets are observable.
	if observation.OutputBudgetBytes != 0 {
		span.SetAttributes(attribute.Int("mcp.result.output_budget_bytes", observation.OutputBudgetBytes))
	}
	// Legacy runtimes omit termination status; never infer timeout from script-controlled error text.
	if observation.ExecutionOutcome != "" {
		span.SetAttributes(attribute.String("mcp.execute.outcome", observation.ExecutionOutcome))
	}
}

// mcpRuntimeResultDelivery reads protocol metadata only; script-returned content cannot spoof these counters.
func mcpRuntimeResultDelivery(response string) (mcpResultDelivery, bool) {
	var envelope struct {
		Result struct {
			Meta struct {
				Execute mcpResultDelivery `json:"com.usefused/execute"`
			} `json:"_meta"`
		} `json:"result"`
	}
	// Invalid protocol JSON must not produce misleading audit events or parser-error details.
	if json.Unmarshal([]byte(response), &envelope) != nil {
		return mcpResultDelivery{}, false
	}
	value := envelope.Result.Meta.Execute
	// Only the three host-owned delivery states are admitted to OTEL cardinality.
	switch value.Delivery {
	case "inline", "stored", "error":
	default:
		return mcpResultDelivery{}, false
	}
	// Counters and policy bytes are bounded independently of any script or result content.
	if !validMCPResultDeliveryCounts(value) || !validMCPExecutionOutcome(value) {
		return mcpResultDelivery{}, false
	}
	return value, true
}

// validMCPExecutionOutcome admits only host-owned states consistent with the delivery result.
func validMCPExecutionOutcome(value mcpResultDelivery) bool {
	// Absent status preserves compatibility with already-running older session processes.
	switch value.ExecutionOutcome {
	case "":
		return true
	case "completed":
		return value.Delivery != "error"
	case "failed", "timed_out", "cancelled":
		return value.Delivery == "error"
	default:
		return false
	}
}

// validMCPResultDeliveryCounts admits only runtime-owned counters and the compiled client-budget range.
func validMCPResultDeliveryCounts(value mcpResultDelivery) bool {
	// Zero budget is absent legacy metadata, not permission to configure zero-byte output.
	validBudget := value.OutputBudgetBytes == 0 || (value.OutputBudgetBytes >= 1024 && value.OutputBudgetBytes <= 64<<10)
	return validBudget && value.RetainedReads >= 0 && value.RetainedReads <= 100000 && value.UnavailableReads >= 0 && value.UnavailableReads <= value.RetainedReads
}

// mcpEffectiveOutputLimit distinguishes a client-tightened ceiling from the compiled maximum in limit audits.
func mcpEffectiveOutputLimit(response string, maximum int64) int64 {
	value, ok := mcpRuntimeResultDelivery(response)
	// Missing or untrusted metadata cannot change the compiled policy projection.
	if !ok || value.OutputBudgetBytes == 0 {
		return maximum
	}
	return min(maximum, int64(value.OutputBudgetBytes))
}
