package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/mcpskill"
)

// TestPolishMCPSkillAcceptsOnlyTaggedProse verifies the Registry route, provider tag and error masking.
func TestPolishMCPSkillAcceptsOnlyTaggedProse(t *testing.T) {
	// Local httptest servers are plain HTTP, which the client only permits in development.
	t.Setenv("FUSED_ENV", "development")
	for name, item := range map[string]struct {
		status  int
		body    string
		wantErr bool
	}{
		"tagged":        {http.StatusOK, `{"purpose":"Resolve tickets.","provider":"fused-mcp-skill"}`, false},
		"wrong tag":     {http.StatusOK, `{"purpose":"Resolve tickets.","provider":"other"}`, true},
		"unconfigured":  {http.StatusServiceUnavailable, `skill polish is not configured`, true},
		"not json body": {http.StatusOK, `<html>`, true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The Engine must call the dedicated licensed route only.
			if r.URL.Path != "/api/engine/mcp-skill-polish" || r.Method != http.MethodPost {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte(item.body))
		}))
		prose, err := NewHTTPRegistryClient(server.URL, "license").PolishMCPSkill(context.Background(), mcpskill.PolishRequest{Name: "support"})
		server.Close()
		// Only tagged success responses produce prose; everything else is a masked failure.
		if (err != nil) != item.wantErr || (!item.wantErr && prose.Purpose != "Resolve tickets.") {
			t.Fatalf("%s: prose = %#v, err = %v", name, prose, err)
		}
	}
}
