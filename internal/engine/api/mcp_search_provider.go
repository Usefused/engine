package api

const mcpClassifierDisclosure = "Intelligent search uses Jev through Fused Registry. Search intent and authorized operation names and descriptions are sent to Jev. Fused manages the Jev API key; no additional key is required."

// mcpSearchPlanSummary makes remote processing visible wherever UI or CLI reviews the immutable plan.
func mcpSearchPlanSummary(create bool, services any, enabled bool) map[string]any {
	summary := map[string]any{"create_mcp": create, "services": services}
	// Only explicitly opted-in versions disclose remote intent classification.
	if enabled {
		summary["fused-intelligent-classifier"] = true
		summary["disclaimer"] = mcpClassifierDisclosure
	}
	return summary
}
