package sandbox

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"testing"
)

// TestOAuthTokenPlacementSnapshotAndMapping proves persisted JSON and dispatcher mapping retain the runtime policy.
func TestOAuthTokenPlacementSnapshotAndMapping(t *testing.T) {
	var configs fusedobject.AuthConfigs
	// Snapshot decoding is the same typed path used for immutable Registry metadata.
	if err := json.Unmarshal([]byte(`[{"name":"oauth","type":"oauth2","oauth_token_placement":{"location":"header","name":"X-Provider-Token","format":"raw"}}]`), &configs); err != nil {
		t.Fatal(err)
	}
	// Runtime admission rejects malformed routing even when an envelope claims support.
	if err := validateAuthRuntimeContract(configs[0]); err != nil {
		t.Fatal(err)
	}
	mapped := mapAuthConfigs(configs)
	// Provider dispatch must consume the stored header instead of a CLI or SDK reinterpretation.
	if len(mapped) != 1 || mapped[0].OAuthTokenPlacement.Name != "X-Provider-Token" {
		t.Fatalf("mapped = %#v", mapped)
	}
	configs[0].OAuthTokenPlacement.Location = "query"
	// Unsupported delivery must fail closed rather than leak a credential into a URL.
	if validateAuthRuntimeContract(configs[0]) == nil {
		t.Fatal("accepted unsupported query placement")
	}
}
