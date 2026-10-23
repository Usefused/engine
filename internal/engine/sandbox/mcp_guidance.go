package sandbox

import (
	"encoding/json"
	"errors"

	engineruntime "github.com/Usefused/engine/runtime"
)

// mcpGuidanceContent mirrors the shared static contract used by the bundled Node runtime.
type mcpGuidanceContent struct {
	Instructions       string `json:"instructions"`
	ExecuteDescription string `json:"executeDescription"`
	Guide              struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		URI         string `json:"uri"`
		Description string `json:"description"`
		Text        string `json:"text"`
	} `json:"guide"`
}

var mcpGuidance = loadMCPGuidance()

// loadMCPGuidance rejects an invalid built-in artifact at startup rather than serving conflicting guidance.
func loadMCPGuidance() mcpGuidanceContent {
	var content mcpGuidanceContent
	// All required fields are build-owned; a failure indicates a broken distribution, not user input.
	if err := json.Unmarshal(engineruntime.MCPGuidanceJSON, &content); err != nil || content.Instructions == "" || content.ExecuteDescription == "" || content.Guide.URI == "" || content.Guide.Name == "" || content.Guide.Text == "" {
		panic("invalid bundled MCP guidance")
	}
	return content
}

// mcpGuidanceResource describes a finite, read-only namespace that cannot expose app credentials or data.
func mcpGuidanceResource() map[string]any {
	guide := mcpGuidance.Guide
	return map[string]any{"name": guide.Name, "title": guide.Title, "uri": guide.URI, "description": guide.Description, "mimeType": "text/markdown"}
}

// mcpGuidancePrompt advertises the same guide as an explicitly selected MCP prompt.
func mcpGuidancePrompt() map[string]any {
	guide := mcpGuidance.Guide
	return map[string]any{"name": guide.Name, "title": guide.Title, "description": guide.Description}
}

// mcpGuidanceRequest answers only exact built-in identities after the transport's normal authorization.
func mcpGuidanceRequest(request mcpJSONRPCRequest) (any, bool, error) {
	var params struct {
		Name      string         `json:"name"`
		URI       string         `json:"uri"`
		Arguments map[string]any `json:"arguments"`
	}
	// Invalid params remain the shared native handler's responsibility unless a built-in identity is established.
	if json.Unmarshal(request.Params, &params) != nil {
		return nil, false, nil
	}
	guide := mcpGuidance.Guide
	// Exact URI matching prevents this public guide from becoming a file or provider-resource proxy.
	if request.Method == "resources/read" && params.URI == guide.URI {
		return map[string]any{"contents": []any{map[string]any{"uri": guide.URI, "mimeType": "text/markdown", "text": guide.Text}}}, true, nil
	}
	// The static prompt accepts no task inputs and therefore cannot interpolate untrusted instructions.
	if request.Method == "prompts/get" && params.Name == guide.Name {
		// An unexpected argument must not be silently interpreted as authored workflow guidance.
		if len(params.Arguments) != 0 {
			return nil, true, errors.New("workflow guide accepts no arguments")
		}
		return map[string]any{"description": guide.Description, "messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": guide.Text}}}}, true, nil
	}
	return nil, false, nil
}
