package api

import (
	"github.com/Usefused/engine/internal/engine/store"
	"strings"
	"testing"
)

// TestMCPClassifierIsImmutableAndDisclosed proves remote-processing consent is part of version identity and plan review.
func TestMCPClassifierIsImmutableAndDisclosed(t *testing.T) {
	doc := sdkConfigDocument{APIVersion: "fused/v1", Kind: "mcp", Name: "mail", Version: "1.0.0", Description: "Read email.", Services: map[string]sdkConfigServiceDoc{}}
	prior, err := canonicalAppState(doc)
	if err != nil {
		t.Fatal(err)
	} // Establish the exact previously applied declaration.
	doc.FusedIntelligentClassifier = true
	_, err = validateMCPDesiredState(doc, &store.ConfigState{DesiredState: prior})
	if err == nil || !strings.Contains(err.Error(), "app_version_immutable") {
		t.Fatalf("consent mutation error=%v", err)
	} // Existing versions cannot acquire remote processing.
	if err := validateMCPServerDescription(doc, "mcp"); err != nil {
		t.Fatal(err)
	} // The canonical provider must be admitted on MCP.
	summary := mcpSearchPlanSummary(true, nil, doc.FusedIntelligentClassifier)
	if !strings.Contains(summary["disclaimer"].(string), "Jev") {
		t.Fatal("missing disclosure")
	} // Both UI and CLI receive the same plan disclosure.
	doc.FusedIntelligentClassifier = true
	if validateMCPServerDescription(doc, "sdk") == nil {
		t.Fatal("SDK classifier accepted")
	} // SDK documents cannot opt into MCP-only remote processing.
	if _, ok := mcpSearchPlanSummary(true, nil, false)["disclaimer"]; ok {
		t.Fatal("local search has remote disclosure")
	} // Local versions have no third-party data transfer.
}

// TestMCPClassifierWireRequiresBoolean rejects quoted values before plan creation.
func TestMCPClassifierWireRequiresBoolean(t *testing.T) {
	for _, value := range []string{"true", "false", `"true"`} {
		var doc sdkConfigDocument
		err := decodeAppConfigJSON([]byte(`{"fused-intelligent-classifier":`+value+`}`), &doc)
		// Only a JSON boolean may express the owner's remote-processing choice.
		if (err != nil) != (value == `"true"`) {
			t.Fatalf("value=%s error=%v", value, err)
		}
	}
}
