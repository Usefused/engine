package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

// TestResolveWebhookAttachmentLabel_ReturnsAttachmentFromConfigState is the
// core isolation-fix assertion: the label comes entirely from the
// connecting SDK/MCP's own applied config (via fused_apps.config_key),
// never from anything the client reports itself -- see the function's doc
// comment and plans/plan-webhook-kind.md's subject-filter section.
func TestResolveWebhookAttachmentLabel_ReturnsAttachmentFromConfigState(t *testing.T) {
	appID := uuid.New()
	configKey := "sdk:jira-sdk:1.0.0"
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AppID: appID, ConfigKey: configKey},
		},
	}
	configStore := &mockConfigStore{state: &store.ConfigState{
		ConfigKey:    configKey,
		DesiredState: []byte(`{"apiVersion":"fused/v1","kind":"sdk","name":"jira-sdk","webhook_attachment":"team-x-webhooks","services":{}}`),
	}}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("resolveWebhookAttachmentLabel: %v", err)
	}
	if label != "team-x-webhooks" {
		t.Fatalf("expected label %q, got %q", "team-x-webhooks", label)
	}
}

func TestResolveWebhookAttachmentLabel_NoScopeReturnsEmptyNotError(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{mockScopes: map[uuid.UUID]*store.AppRuntime{}}
	configStore := &mockConfigStore{}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error for an unknown sdk id, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label for an unknown sdk id, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_ScopeWithNoConfigKeyReturnsEmpty(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID}},
	}
	configStore := &mockConfigStore{}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error for a scope with no config_key, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label for a scope with no config_key, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_NoConfigStateReturnsEmpty(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID, ConfigKey: "sdk:reader:1.0.0"}},
	}
	// mockConfigStore.state is nil with a nil error -- GetConfigState's real
	// "not found" shape (config_repository.go's scanConfigState), not an
	// error condition.
	configStore := &mockConfigStore{}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error when the config state is missing, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label when the config state is missing, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_NoAttachmentReturnsEmpty(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID, ConfigKey: "sdk:reader:1.0.0"}},
	}
	configStore := &mockConfigStore{state: &store.ConfigState{
		DesiredState: []byte(`{"apiVersion":"fused/v1","kind":"sdk","name":"reader","services":{}}`),
	}}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error for a config with no webhook_attachment, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label for a config with no webhook_attachment, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_ConfigStateLookupErrorPropagates(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID, ConfigKey: "sdk:reader:1.0.0"}},
	}
	configStore := &mockConfigStore{err: errors.New("db unavailable")}

	if _, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID); err == nil {
		t.Fatal("expected a config state lookup failure to propagate")
	}
}

// subjectSafeLabel is duplicated verbatim in sandbox/webhook.go (see that
// package's test for the equivalent case) -- both must apply the exact same
// substitution or a label containing "." would match on the publish side
// but not the consumer-filter side, silently dropping every delivery.
func TestSubjectSafeLabel_ReplacesDots(t *testing.T) {
	if got, want := subjectSafeLabel("team.x"), "team-x"; got != want {
		t.Fatalf("subjectSafeLabel(%q) = %q, want %q", "team.x", got, want)
	}
}
