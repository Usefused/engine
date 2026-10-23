package sandbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/mcpskill"
	"github.com/Usefused/engine/internal/engine/store"
)

// mcpSkillResource advertises the server's own Agent Skill beside the built-in workflow guide.
// The entry is static so resources/list never needs a database read.
func mcpSkillResource() map[string]any {
	return map[string]any{
		"name": "server-skill", "title": "Agent skill for this MCP server", "uri": mcpskill.ResourceURI,
		"description": "SKILL.md describing this server's purpose, the operations your token can call, and how to call them.",
		"mimeType":    "text/markdown",
	}
}

// mcpSkillRequest answers resources/read for the exact skill URI after the transport's normal token admission.
func mcpSkillRequest(ctx context.Context, request mcpJSONRPCRequest, identity auth.RuntimeIdentity) (any, bool, error) {
	// Only resources/read can address the skill.
	if request.Method != "resources/read" {
		return nil, false, nil
	}
	var params struct {
		URI string `json:"uri"`
	}
	// Invalid params remain the shared native handler's responsibility unless the skill URI is established.
	if json.Unmarshal(request.Params, &params) != nil || params.URI != mcpskill.ResourceURI {
		return nil, false, nil
	}
	content, err := renderMCPSkillResource(ctx, identity)
	// A missing app identity or storage failure is reported, never replaced by another version's skill.
	if err != nil {
		return nil, true, err
	}
	return map[string]any{"contents": []any{map[string]any{"uri": mcpskill.ResourceURI, "mimeType": "text/markdown", "text": content}}}, true, nil
}

// renderMCPSkillResource renders the token-scoped skill for the caller's exact MCP version.
func renderMCPSkillResource(ctx context.Context, identity auth.RuntimeIdentity) (string, error) {
	loader, ok := globalObjectCache.(mcpServerMetadataLoader)
	// Server identity must come from the same immutable version the token was admitted to.
	if !ok {
		return "", errors.New("MCP server metadata is unavailable")
	}
	server, err := loader.GetMCPServerMetadata(ctx, identity.AppID.String())
	// The skill cannot name or describe a server whose metadata cannot be read.
	if err != nil {
		return "", errors.New("MCP server metadata is unavailable")
	}
	snapshot, err := loadMCPSkillSnapshot(ctx, identity)
	// A corrupt or unreadable snapshot fails closed rather than advertising an unknown scope.
	if err != nil {
		return "", err
	}
	_, content := mcpskill.Render(mcpskill.Input{
		Name: server.Name, Version: server.Version, Description: server.Description,
		Snapshot: snapshot, Allow: identity.AllowsOperation,
	})
	return content, nil
}

// loadMCPSkillSnapshot reads the plan-pinned snapshot; nil means the version predates skills.
func loadMCPSkillSnapshot(ctx context.Context, identity auth.RuntimeIdentity) (*mcpskill.Snapshot, error) {
	cache, ok := globalObjectCache.(*LocalObjectCache)
	// Other cache adapters (tests, offline fixtures) have no plan storage; they serve the template skill.
	if !ok || cache.db == nil {
		return nil, nil
	}
	repository, ok := cache.db.(store.MCPSkillSnapshotStore)
	// A store without the capability serves the template skill instead of failing the read.
	if !ok {
		return nil, nil
	}
	raw, err := repository.GetMCPSkillSnapshot(ctx, identity.AppID)
	// Storage failures are distinct from the legacy no-snapshot case.
	if err != nil {
		return nil, errors.New("MCP skill is unavailable")
	}
	// Versions planned before skills existed have no pinned inventory.
	if raw == nil {
		return nil, nil
	}
	var snapshot mcpskill.Snapshot
	// Only the known schema is trusted to describe operation scope.
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.SchemaVersion != mcpskill.SchemaVersion {
		return nil, errors.New("MCP skill is unavailable")
	}
	return &snapshot, nil
}
