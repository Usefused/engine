package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestResourceReferenceValidation(t *testing.T) {
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceBucket, Value: "company"}); err != nil {
		t.Fatalf("validate bucket reference: %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceCredential, Value: "laptop"}); !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("credential without parent error = %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceCredential, Value: "laptop", ParentID: uuid.New()}); err != nil {
		t.Fatalf("validate bounded credential reference: %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceApp, Value: "support", AppKind: "mcp"}); err != nil {
		t.Fatalf("validate MCP reference: %v", err)
	}
	if err := validateResourceReferenceQuery(ResourceReferenceQuery{Kind: ReferenceApp, Value: "support", AppKind: "webhook"}); !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("unsupported app namespace error = %v", err)
	}
}
