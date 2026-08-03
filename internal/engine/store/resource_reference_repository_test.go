package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestResourceReferenceValidationAndArtifactParsing(t *testing.T) {
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceBucket, Value: "company"}); err != nil {
		t.Fatalf("validate bucket reference: %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceCredential, Value: "laptop"}); !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("credential without parent error = %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceCredential, Value: "laptop", ParentID: uuid.New()}); err != nil {
		t.Fatalf("validate bounded credential reference: %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support", ArtifactKind: "mcp"}); err != nil {
		t.Fatalf("validate MCP reference: %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support", ArtifactKind: "webhook"}); !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("unsupported artifact namespace error = %v", err)
	}
	name, version := parseArtifactReference(" support-sdk@1.2.0 ")
	if name != "support-sdk" || version != "1.2.0" {
		t.Fatalf("artifact reference = %q/%q", name, version)
	}
}
