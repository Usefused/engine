package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/mcpskill"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// mcpSkillTestStore serves one app runtime and one pinned skill snapshot.
type mcpSkillTestStore struct {
	store.Store
	runtime  *store.AppRuntime
	snapshot json.RawMessage
	err      error
}

// GetAppRuntime returns the fixture runtime for server metadata.
func (s *mcpSkillTestStore) GetAppRuntime(context.Context, uuid.UUID) (*store.AppRuntime, error) {
	return s.runtime, nil
}

// GetMCPSkillSnapshot returns the configured snapshot or storage error.
func (s *mcpSkillTestStore) GetMCPSkillSnapshot(context.Context, uuid.UUID) (json.RawMessage, error) {
	return s.snapshot, s.err
}

// useMCPSkillTestCache installs a LocalObjectCache backed by the skill test store.
func useMCPSkillTestCache(t *testing.T, db *mcpSkillTestStore) {
	t.Helper()
	previous := globalObjectCache
	globalObjectCache = &LocalObjectCache{db: db}
	t.Cleanup(func() { globalObjectCache = previous })
}

// readMCPSkillText performs one native resources/read for the skill URI.
func readMCPSkillText(t *testing.T, identity auth.RuntimeIdentity) (string, error) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"uri": mcpskill.ResourceURI})
	result, handled, err := importedNativeRequest(context.Background(), nil, mcpJSONRPCRequest{Method: "resources/read", Params: params}, identity, map[string][]any{}, nil)
	// The skill URI is always owned by the Engine, never forwarded to a provider.
	if !handled {
		t.Fatal("skill read was not handled")
	}
	// Errors are returned to the caller so failure paths can be asserted.
	if err != nil {
		return "", err
	}
	contents := result.(map[string]any)["contents"].([]any)
	return contents[0].(map[string]any)["text"].(string), nil
}

// TestMCPSkillResourceModernAdmission proves the skill is listed and readable only after normal token admission.
func TestMCPSkillResourceModernAdmission(t *testing.T) {
	fixture := installMCPModernFixture(t)
	t.Setenv("PATH", t.TempDir())
	for i, item := range []struct {
		method string
		params map[string]any
		want   string
	}{
		{"resources/list", nil, mcpskill.ResourceURI},
		{"resources/read", map[string]any{"uri": mcpskill.ResourceURI}, "name: transport-test-mcp"},
	} {
		response := httptest.NewRecorder()
		fixture.router.ServeHTTP(response, newMCPModernRequest(t, fixture, i+300, item.method, item.params))
		raw, _ := json.Marshal(decodeMCPModernResult(t, response))
		// Listing advertises the skill and reading returns the server's own SKILL.md.
		if !strings.Contains(string(raw), item.want) {
			t.Fatalf("%s result missing %q: %s", item.method, item.want, raw)
		}
		denied := newMCPModernRequest(t, fixture, i+400, item.method, item.params)
		denied.Header.Set("Authorization", "Bearer denied")
		response = httptest.NewRecorder()
		fixture.router.ServeHTTP(response, denied)
		// The skill reveals operation scope, so it must never bypass token admission.
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized %s returned %d", item.method, response.Code)
		}
	}
}

// TestMCPSkillResourceRendersPinnedSnapshotForToken proves the plan snapshot is used and filtered per token.
func TestMCPSkillResourceRendersPinnedSnapshotForToken(t *testing.T) {
	snapshot := mcpskill.BuildSnapshot([]mcpskill.Operation{
		{OperationID: "tickets.create", ServiceKey: "s1"},
		{OperationID: "tickets.delete", ServiceKey: "s1"},
	}, []mcpskill.Service{{Key: "s1", Label: "helpdesk", Version: "v1"}})
	snapshot.Prose = &mcpskill.Prose{Purpose: "Resolve customer support tickets."}
	raw, _ := json.Marshal(snapshot)
	useMCPSkillTestCache(t, &mcpSkillTestStore{runtime: &store.AppRuntime{Name: "support", Version: "1.0.0", Description: "Manage tickets."}, snapshot: raw})
	restricted := auth.RuntimeIdentity{AppID: uuid.New(), TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"tickets.create"}}}
	text, err := readMCPSkillText(t, restricted)
	// The pinned prose and only the token's operations are served.
	if err != nil || !strings.Contains(text, "Resolve customer support tickets.") || !strings.Contains(text, "- `tickets.create`") || strings.Contains(text, "tickets.delete") {
		t.Fatalf("skill = %q, err = %v", text, err)
	}
}

// TestMCPSkillResourceLegacyAndFailure covers versions without a snapshot and unreadable storage.
func TestMCPSkillResourceLegacyAndFailure(t *testing.T) {
	db := &mcpSkillTestStore{runtime: &store.AppRuntime{Name: "legacy", Version: "1", Description: "Old server."}}
	useMCPSkillTestCache(t, db)
	identity := auth.RuntimeIdentity{AppID: uuid.New(), TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	text, err := readMCPSkillText(t, identity)
	// A version planned before skills existed still gets a valid template skill.
	if err != nil || !strings.Contains(text, "name: legacy-mcp") || !strings.Contains(text, "Browse the operations available to you") {
		t.Fatalf("legacy skill = %q, err = %v", text, err)
	}
	db.err = errors.New("database down")
	// Storage failure fails closed instead of advertising an unknown scope.
	if _, err := readMCPSkillText(t, identity); err == nil {
		t.Fatal("storage failure returned a skill")
	}
	db.err, db.snapshot = nil, json.RawMessage(`{"schema_version":99}`)
	// An unknown snapshot schema is never interpreted.
	if _, err := readMCPSkillText(t, identity); err == nil {
		t.Fatal("unknown snapshot schema returned a skill")
	}
}
