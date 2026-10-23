package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine/mcpskill"
)

const (
	// mcpSkillPolishProvider identifies a Registry response produced by the skill polish route.
	mcpSkillPolishProvider = "fused-mcp-skill"
	// mcpSkillPolishRequestLimit bounds the evidence sent to the Registry.
	mcpSkillPolishRequestLimit = 64 << 10
	// mcpSkillPolishResponseLimit bounds the prose accepted back.
	mcpSkillPolishResponseLimit = 16 << 10
)

var _ mcpskill.Polisher = (*HTTPRegistryClient)(nil)

// PolishMCPSkill asks the Registry LLM to rewrite skill prose using the Engine's existing license.
// Only public version metadata is sent; transport and upstream errors are never echoed.
func (c *HTTPRegistryClient) PolishMCPSkill(ctx context.Context, request mcpskill.PolishRequest) (mcpskill.Prose, error) {
	payload, _ := json.Marshal(request)
	// Oversized evidence is refused locally instead of being truncated mid-JSON.
	if len(payload) > mcpSkillPolishRequestLimit {
		return mcpskill.Prose{}, errors.New("skill polish evidence exceeds the request limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/api/engine/mcp-skill-polish", bytes.NewReader(payload))
	// Request construction failures keep Registry transport details private.
	if err != nil {
		return mcpskill.Prose{}, errors.New("skill polish unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doWithCallerDeadline(req)
	// License-bearing transport failures are never exposed.
	if err != nil {
		return mcpskill.Prose{}, errors.New("skill polish unavailable")
	}
	defer response.Body.Close()
	// Non-success responses, including an unconfigured Registry LLM, mean template-only.
	if response.StatusCode != http.StatusOK {
		return mcpskill.Prose{}, errors.New("skill polish unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, mcpSkillPolishResponseLimit+1))
	// Only a small prose object is admitted.
	if err != nil || len(body) > mcpSkillPolishResponseLimit {
		return mcpskill.Prose{}, errors.New("skill polish response invalid")
	}
	var result struct {
		mcpskill.Prose
		Provider string `json:"provider"`
	}
	// The response must come from the skill route, not an unrelated Registry handler.
	if json.Unmarshal(body, &result) != nil || result.Provider != mcpSkillPolishProvider {
		return mcpskill.Prose{}, errors.New("skill polish response invalid")
	}
	return result.Prose, nil
}
