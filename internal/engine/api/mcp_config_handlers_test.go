package api

import (
	"strings"
	"testing"
)

// TestValidateArtifactConfigDocument_MCPRejectsWebhookSelection locks in the
// MCP webhook-rejection rule at both entry points that can select webhooks on
// a service: the explicit Webhooks allowlist and the newer WebhooksSelectAll
// flag. WebhooksSelectAll was added alongside webhook_attachment without
// updating this check, so `webhooks_select_all: true` on an MCP service
// silently bypassed the "MCP cannot select webhooks" rule that the CLI's own
// validateArtifactServices already enforced -- this test guards against that
// regressing again.
func TestValidateArtifactConfigDocument_MCPRejectsWebhookSelection(t *testing.T) {
	baseDoc := func(svc sdkConfigServiceDoc) sdkConfigDocument {
		return sdkConfigDocument{
			APIVersion: "fused/v1",
			Kind:       "mcp",
			Name:       "jira-mcp",
			Version:    "1.0.0",
			Bucket:     "default",
			Services:   map[string]sdkConfigServiceDoc{"jira": svc},
		}
	}

	t.Run("explicit webhooks list rejected", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}, Webhooks: []string{"issue.created"}})
		err := validateArtifactConfigDocument(doc, "mcp")
		if err == nil || !strings.Contains(err.Error(), "cannot select webhooks") {
			t.Fatalf("expected 'cannot select webhooks' error, got %v", err)
		}
	})

	t.Run("webhooks_select_all rejected", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}, WebhooksSelectAll: true})
		err := validateArtifactConfigDocument(doc, "mcp")
		if err == nil || !strings.Contains(err.Error(), "cannot select webhooks") {
			t.Fatalf("expected 'cannot select webhooks' error for webhooks_select_all, got %v", err)
		}
	})

	t.Run("plain operations selection is fine", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}})
		if err := validateArtifactConfigDocument(doc, "mcp"); err != nil {
			t.Fatalf("expected no error for an operations-only MCP service, got %v", err)
		}
	})
}
