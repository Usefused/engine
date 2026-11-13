package api

const mcpClassifierDisclosure = "Intelligent search sends search intent and authorized operation names and descriptions to AI providers through Fused. No additional API key is required."

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
