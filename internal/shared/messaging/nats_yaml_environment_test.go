package messaging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Usefused/engine/internal/shared/config"
	server "github.com/nats-io/nats-server/v2/server"
)

// TestYAMLBootstrapsExistingNATSConsumer verifies YAML startup publication works with the unchanged environment-only client.
func TestYAMLBootstrapsExistingNATSConsumer(t *testing.T) {
	clearNATSEnv(t)
	t.Setenv("FUSED_NATS_RATE_LIMIT_REPLICAS", "")
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "")
	t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", "")
	broker := startAuthenticatedNATSServer(t, &server.Options{Authorization: "test-token"})
	t.Setenv("NATS_TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "engine.yaml")
	// A mounted YAML profile supplies only the endpoint and replication; the token remains inherited.
	if err := os.WriteFile(path, []byte("nats:\n  url: "+broker.ClientURL()+"\n  rate_limit_replicas: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	// Use the real loader and startup bridge, not a hand-built environment fixture.
	if err != nil {
		t.Fatal(err)
	}
	// The original NATS client must need no knowledge of YAML or new typed config APIs.
	if err := cfg.ExportEnvironment(); err != nil {
		t.Fatal(err)
	}
	client, err := ConnectNATS()
	// Authentication proves inherited secrets remain effective alongside YAML deployment settings.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	// Provision real coordination state to verify the settings reach JetStream.
	if _, err := client.InitProviderRateLimitBucket(); err != nil {
		t.Fatal(err)
	}
}
