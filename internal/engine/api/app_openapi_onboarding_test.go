package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestOpenAPIIncludesConnectionOnboarding keeps spec-only consumers informed
// about user connections without changing the actual operation schema.
func TestOpenAPIIncludesConnectionOnboarding(t *testing.T) {
	fixture, _ := newAppOpenAPIFixture(t)
	document, err := buildAppOpenAPIDocument(context.Background(), fixture, fixture.app, fixture.family, "")
	// The ordinary contract builder must still validate and produce a complete document.
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Info struct{ Description string } `json:"info"`
	}
	// Decode the wire document so the test covers the actual export surface.
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"selector.end_user_ref", "workspace service connect", "fused-cli api openapi " + fixture.app.AppID.String(), "Authorization Bearer"} {
		// Authentication setup must remain discoverable without downloading an SDK package.
		if !strings.Contains(decoded.Info.Description, expected) {
			t.Errorf("OpenAPI description missing %q", expected)
		}
	}
}
