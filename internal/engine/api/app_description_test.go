package api

import (
	"testing"

	"github.com/google/uuid"
)

// TestAppPublicationPreservesDescription covers authored and absent metadata through the plan payload.
func TestAppPublicationPreservesDescription(t *testing.T) {
	for _, tc := range []struct{ kind, description, want string }{
		{"unified_app", "", ""},
		{"sdk", "", ""},
		{"unified_app", "  Read Mac status and open approved apps.  ", "Read Mac status and open approved apps."},
	} {
		doc := sdkConfigDocument{Kind: tc.kind, Name: "mac-control", Version: "1.0.0", Description: tc.description}
		request := sdkGenerateRequest(doc, nil, nil, "https://engine.example.com")
		payload := resolvedSDKPayload(request, uuid.New(), uuid.New(), false, nil)
		// Canonical metadata must survive publication without introducing generated copy.
		if payload.Description != tc.want {
			t.Fatalf("%s description = %q", tc.kind, payload.Description)
		}
	}
}
