package accesscontrol

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSanitizeAuditMetadataCopiesSafeValues(t *testing.T) {
	input := map[string]any{"authorization_revision": int64(4), "team_count": 2}

	got, err := SanitizeAuditMetadata(input)
	if err != nil {
		t.Fatal(err)
	}
	input["authorization_revision"] = int64(8)
	if got["authorization_revision"] != int64(4) {
		t.Fatalf("sanitized metadata aliases input: %#v", got)
	}
}

func TestAuditEventValidation(t *testing.T) {
	event := AuditEvent{
		Action:     "workspace.service.add",
		Permission: PermissionServiceManage,
		Resource:   ResourceRef{Type: ResourceWorkspace, ID: uuid.New()},
		Outcome:    AuditSucceeded,
		Metadata:   map[string]any{"service_count": 2},
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	event.Outcome = AuditAttempted
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate attempted outcome: %v", err)
	}
	event.Outcome = AuditSucceeded
	event.Metadata["api_key"] = "must-not-persist"
	if err := event.Validate(); !errors.Is(err, ErrUnsafeAuditMetadata) {
		t.Fatalf("unsafe metadata error = %v", err)
	}
}

func TestSanitizeAuditMetadataAllowsArtifactOwnerIdentity(t *testing.T) {
	ownerID := uuid.NewString()
	metadata, err := SanitizeAuditMetadata(map[string]any{
		"owner_type": "team",
		"owner_id":   ownerID,
		"changed":    true,
	})
	if err != nil {
		t.Fatalf("sanitize artifact owner metadata: %v", err)
	}
	if metadata["owner_type"] != "team" || metadata["owner_id"] != ownerID {
		t.Fatalf("artifact owner metadata = %#v", metadata)
	}
}

func TestAuditEventBoundsMissingRequirements(t *testing.T) {
	resource := ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}
	missing := make([]Requirement, MaxAuditMissingRequirements)
	for i := range missing {
		missing[i] = Requirement{Permission: PermissionWorkspaceRead, Resource: resource}
	}
	event := AuditEvent{Action: "workspace.read", Outcome: AuditDenied, MissingRequirements: missing}
	if err := event.Validate(); err != nil {
		t.Fatalf("64 missing requirements: %v", err)
	}
	event.MissingRequirements = append(event.MissingRequirements, missing[0])
	if err := event.Validate(); err == nil {
		t.Fatal("65 missing requirements must be rejected")
	}
}

func TestSanitizeAuditMetadataRejectsForbiddenKeysAndCredentialValues(t *testing.T) {
	tests := []map[string]any{
		{"api_key": "redacted"},
		{"requestBody": "{}"},
		{"requirements": "Bearer local-key"},
		{"requirements": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature"},
		{"requirements": "fused_local_credential"},
		{"requirements": "fsk_personal_control_credential"},
		{"requirements": "fused_sdk_runtime_credential"},
		{"requirements": map[string]any{"safe_name": "nested"}},
	}
	for _, metadata := range tests {
		if _, err := SanitizeAuditMetadata(metadata); !errors.Is(err, ErrUnsafeAuditMetadata) {
			t.Fatalf("metadata %#v error = %v, want ErrUnsafeAuditMetadata", metadata, err)
		}
	}
}

func TestAuditEventRejectsUnboundedRequestText(t *testing.T) {
	tests := []AuditEvent{
		{Action: "workspace.read", Outcome: AuditAllowed, Path: "/workspace?token=secret"},
		{Action: "workspace.read", Outcome: AuditAllowed, UserAgent: "arbitrary-client/1.0"},
		{Action: "workspace.read\nforged", Outcome: AuditAllowed},
		{Action: "workspace.read", Outcome: AuditAllowed, RequestID: strings.Repeat("x", 129)},
	}
	for _, event := range tests {
		if err := event.Validate(); !errors.Is(err, ErrUnsafeAuditMetadata) {
			t.Fatalf("event %#v error = %v, want ErrUnsafeAuditMetadata", event, err)
		}
	}
}
