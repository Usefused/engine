package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestValidateAppConfigDocument_MCPRejectsWebhookSelection locks in the
// MCP webhook-rejection rule at both entry points that can select webhooks on
// a service: the explicit Webhooks allowlist and the newer WebhooksSelectAll
// flag. WebhooksSelectAll was added alongside webhook_attachment without
// updating this check, so `webhooks_select_all: true` on an MCP service
// silently bypassed the "MCP cannot select webhooks" rule that CLI validation
// already enforces -- this test guards against that
// regressing again.
func TestValidateAppConfigDocument_MCPRejectsWebhookSelection(t *testing.T) {
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
		err := validateAppConfigDocument(doc, "mcp")
		if err == nil || !strings.Contains(err.Error(), "cannot select webhooks") {
			t.Fatalf("expected 'cannot select webhooks' error, got %v", err)
		}
	})

	t.Run("webhooks_select_all rejected", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}, WebhooksSelectAll: true})
		err := validateAppConfigDocument(doc, "mcp")
		if err == nil || !strings.Contains(err.Error(), "cannot select webhooks") {
			t.Fatalf("expected 'cannot select webhooks' error for webhooks_select_all, got %v", err)
		}
	})

	t.Run("plain operations selection is fine", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}})
		if err := validateAppConfigDocument(doc, "mcp"); err != nil {
			t.Fatalf("expected no error for an operations-only MCP service, got %v", err)
		}
	})
}

func TestAppConfigVersionLengthBound(t *testing.T) {
	tooLong := "1.0.0-" + strings.Repeat("a", maxAppVersionLength)

	sdkDoc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "sdk", Name: "support", Version: tooLong,
		Language: "typescript", Bucket: "default", Services: map[string]sdkConfigServiceDoc{},
	}
	if err := validateSDKIdentity(sdkDoc); err == nil {
		t.Fatal("expected an overlong SDK version to be rejected")
	}

	mcpDoc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "mcp", Name: "support", Version: tooLong,
		Bucket: "default", Services: map[string]sdkConfigServiceDoc{},
	}
	if err := validateAppConfigDocument(mcpDoc, "mcp"); err == nil {
		t.Fatal("expected an overlong MCP version to be rejected")
	}
}

// TestEnforceMCPFamilyLimit guards the MaxMCPFamilies entitlement check added
// to executeMCPConfigApply.  It mirrors the SDK family limit pattern in
// sdk_config_handlers.go but uses a separate limit field so SDK and MCP
// counts are gated independently.
func TestEnforceMCPFamilyLimit(t *testing.T) {
	accountID := uuid.New()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
		MaxMCPFamilies: models.IntPtr(-1), // unlimited
	})
	defer entitlement.LiveEntitlement.Reset()

	t.Run("existing family allowed regardless of limit", func(t *testing.T) {
		s := &workspaceTestStore{
			accountID: accountID,
			appFamilies: map[string]store.AppFamily{
				accountID.String() + "\x00" + "mcp" + "\x00" + "jira": {
					AccountID:     accountID,
					Kind:          "mcp",
					CanonicalName: "jira",
				},
			},
		}
		// set a very low limit
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(0),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "jira")
		if err != nil {
			t.Fatalf("existing family should be allowed even at zero limit: %v", err)
		}
	})

	t.Run("new family allowed when under limit", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(2),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "slack")
		if err != nil {
			t.Fatalf("new family under limit should be allowed: %v", err)
		}
	})

	t.Run("new family blocked at limit", func(t *testing.T) {
		s := &workspaceTestStore{
			accountID: accountID,
			appFamilies: map[string]store.AppFamily{
				accountID.String() + "\x00" + "mcp" + "\x00" + "github": {
					AccountID:     accountID,
					Kind:          "mcp",
					CanonicalName: "github",
				},
				accountID.String() + "\x00" + "mcp" + "\x00" + "gitlab": {
					AccountID:     accountID,
					Kind:          "mcp",
					CanonicalName: "gitlab",
				},
			},
		}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(2),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "bitbucket")
		if err == nil {
			t.Fatal("expected block when at limit")
		}
		werr, ok := err.(workspaceConfigHTTPError)
		if !ok || werr.status != http.StatusForbidden {
			t.Fatalf("expected 403, got %#v", err)
		}
	})

	t.Run("zero limit blocks all new families", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(0),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "pagerduty")
		if err == nil {
			t.Fatal("expected block with zero limit")
		}
		werr, ok := err.(workspaceConfigHTTPError)
		if !ok || werr.status != http.StatusForbidden {
			t.Fatalf("expected 403, got %#v", err)
		}
	})

	t.Run("unlimited allows new families", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(-1),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "asana")
		if err != nil {
			t.Fatalf("unlimited should allow: %v", err)
		}
	})

	t.Run("nil limit is treated as unlimited", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: nil,
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "notion")
		if err != nil {
			t.Fatalf("nil limit (normalized to unlimited) should allow: %v", err)
		}
	})

	t.Run("invalid name returns 400", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "")
		if err == nil {
			t.Fatal("expected error for empty name")
		}
		werr, ok := err.(workspaceConfigHTTPError)
		if !ok || werr.status != http.StatusBadRequest {
			t.Fatalf("expected 400, got %#v", err)
		}
	})

	// DB count-error paths are covered by integration tests against real
	// Store implementations; workspaceTestStore does not expose injected
	// count errors.
}
