package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const mcpIntelligentClassifier = "fused-intelligent-classifier"

type ClassifierOperation struct {
	OperationName string `json:"operationName"`
	Description   string `json:"description"`
}
type OperationClassifier interface {
	ClassifyOperations(context.Context, string, []ClassifierOperation) (string, error)
}

var mcpClassifier OperationClassifier

// SetMCPOperationClassifier wires discovery to the existing licensed Registry client at process startup.
func SetMCPOperationClassifier(client OperationClassifier) { mcpClassifier = client }

// ClassifyOperations uses the existing Engine license; neither MCP clients nor Node receive the Jev key.
func (c *HTTPRegistryClient) ClassifyOperations(ctx context.Context, intent string, operations []ClassifierOperation) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"intent": intent, "operations": operations})
	// Bound catalogue transfer independently of the public MCP argument budget.
	if len(payload) > 2<<20 {
		return "", errors.New("classifier catalogue exceeds the request limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/api/engine/fused-intelligent-classifier", bytes.NewReader(payload))
	if err != nil {
		return "", errors.New("classifier unavailable")
	} // Keep Registry transport details private.
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doWithCallerDeadline(req)
	if err != nil {
		return "", errors.New("classifier unavailable")
	} // Never expose license-bearing transport failures.
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("classifier unavailable")
	} // Upstream errors are not model-facing content.
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(body) > 8192 {
		return "", errors.New("classifier response invalid")
	} // Admit only a small operation identity response.
	var result struct {
		OperationName string `json:"operationName"`
		Provider      string `json:"provider"`
	}
	if json.Unmarshal(body, &result) != nil || result.Provider != mcpIntelligentClassifier {
		return "", errors.New("classifier response invalid")
	} // Unknown contracts cannot silently change selection semantics.
	return result.OperationName, nil
}

// runMCPDocumentation validates locally before an opted-in intent can leave Engine.
func runMCPDocumentation(ctx context.Context, fixture *Fixture, args map[string]any) (map[string]any, error) {
	local, err := runMCPMetadata(ctx, fixture, args)
	// Invalid requests, exact detail, browsing, and local versions never contact Registry.
	if err != nil || local["isError"] == true || !usesMCPClassifier(fixture, args) {
		return local, err
	}
	operations := mcpClassifierCatalogue(fixture)
	selected := []string{}
	// An empty authorized catalogue has no candidate and requires no inference.
	if len(operations) > 0 {
		name, err := classifyMCPIntent(ctx, args["query"].(string), operations)
		if err != nil {
			return mcpClassifierFailure(), nil
		} // Fail explicitly rather than silently changing the selected search provider.
		if name != "" {
			selected = append(selected, name)
		} // An empty name is the classifier's explicit no-match result.
	}
	copy := *fixture
	copy.ClassifierOperationNames = &selected
	return runMCPMetadata(ctx, &copy, args)
}

// usesMCPClassifier gives exact and section lookup precedence over intent classification.
func usesMCPClassifier(fixture *Fixture, args map[string]any) bool {
	if fixture == nil || !fixture.Server.FusedIntelligentClassifier {
		return false
	} // Remote disclosure requires immutable opt-in.
	query, _ := args["query"].(string)
	operation, _ := args["operationId"].(string)
	return strings.TrimSpace(query) != "" && operation == "" && args["section"] == nil && args["schemaPath"] == nil
}

// classifyMCPIntent rechecks the returned name against the exact token-authorized catalogue.
func classifyMCPIntent(ctx context.Context, intent string, operations []ClassifierOperation) (string, error) {
	if mcpClassifier == nil || len(intent) > 4096 || len(operations) > 2048 {
		return "", errors.New("classifier unavailable")
	} // Bound paid work before dispatch.
	name, err := mcpClassifier.ClassifyOperations(ctx, intent, operations)
	if err != nil || name == "" {
		return name, err
	} // Preserve errors and explicit no-match without fabricating a candidate.
	for _, op := range operations {
		if op.OperationName == name {
			return name, nil
		} // Only exact submitted identities can enter model context.
	}
	return "", errors.New("classifier returned an unauthorized operation")
}

// mcpClassifierCatalogue shares only public names and bounded prose, never schemas or credentials.
func mcpClassifierCatalogue(fixture *Fixture) []ClassifierOperation {
	operations := []ClassifierOperation{}
	for _, op := range fixture.Operations {
		operations = append(operations, ClassifierOperation{op.OperationID, classifierDescription(op.Description)})
	}
	if fixture.UnifiedOperations != nil { // Public Unified descriptors participate in the same exact namespace.
		for _, op := range fixture.UnifiedOperations.Operations {
			operations = append(operations, ClassifierOperation{op.Name, classifierDescription("Unified operation: " + op.Description)})
		}
	}
	return operations
}

// classifierDescription bounds disclosure while preserving valid UTF-8.
func classifierDescription(value string) string {
	if len(value) <= 512 {
		return value
	} // Short descriptions need no transformation.
	value = value[:512]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	} // Never split a multibyte character in the transport.
	return value
}

// mcpClassifierFailure is stable model-facing guidance without raw query or upstream diagnostics.
func mcpClassifierFailure() map[string]any {
	return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "Jev intelligent search is unavailable or the catalogue exceeds 2048 operations. Retry later, narrow the MCP selection, or use an exact operationId. No operation was selected."}}}
}

// mcpSearchHandler gives compatibility transports the same licensed search path without exposing it to scripts.
func mcpSearchHandler(w http.ResponseWriter, r *http.Request) {
	sessionID, _ := extractBearerToken(r)
	sess, ok := lookupMCPSession(sessionID)
	if !ok || sess.fixture == nil {
		http.Error(w, "MCP session unavailable", http.StatusUnauthorized)
		return
	} // Only a live child bridge can request discovery.
	ctx, cancel := mcpSessionRequestContext(r.Context(), sess)
	defer cancel()
	identity, err := validateMCPToken(ctx, sess.appID, sess.token)
	if err != nil || identity.TokenID != sess.tokenID {
		http.Error(w, "MCP session unavailable", http.StatusUnauthorized)
		return
	} // Revalidate revocation before disclosing any catalogue.
	var args map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	if decoder.Decode(&args) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid search arguments", http.StatusBadRequest)
		return
	} // Exactly one bounded object enters trusted metadata code.
	result, err := runMCPDocumentation(ctx, sess.fixture, args)
	if err != nil {
		http.Error(w, "MCP documentation unavailable", http.StatusBadGateway)
		return
	} // Runtime internals remain private.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
