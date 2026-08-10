package messaging

import (
	"os"
	"strings"
	"testing"
	"time"

	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
)

func TestLoadNATSConnectionConfigRejectsExternalSettingsInEmbeddedMode(t *testing.T) {
	clearNATSEnv(t)
	t.Setenv("NATS_TOKEN", "must-not-appear")
	_, err := loadNATSConnectionConfig()
	if err == nil || !strings.Contains(err.Error(), "NATS_URL is required") {
		t.Fatalf("expected external configuration error, got %v", err)
	}
	if strings.Contains(err.Error(), "must-not-appear") {
		t.Fatal("configuration error exposed the NATS token")
	}
}

func TestLoadNATSConnectionConfigRejectsCredentialsInURL(t *testing.T) {
	clearNATSEnv(t)
	t.Setenv("NATS_URL", "nats://secret-user:secret-password@nats.example.test:4222")
	_, err := loadNATSConnectionConfig()
	if err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Fatalf("expected URL credential error, got %v", err)
	}
	for _, secret := range []string{"secret-user", "secret-password"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("configuration error exposed %q", secret)
		}
	}
}

func TestLoadNATSConnectionConfigValidatesAuthenticationSelection(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       map[string]string
		wantError string
	}{
		{name: "password requires username", env: map[string]string{"NATS_PASSWORD": "secret"}, wantError: "configured together"},
		{name: "username requires password", env: map[string]string{"NATS_USERNAME": "engine"}, wantError: "configured together"},
		{name: "methods are exclusive", env: map[string]string{"NATS_TOKEN": "secret", "NATS_USERNAME": "engine", "NATS_PASSWORD": "secret"}, wantError: "exactly one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearNATSEnv(t)
			t.Setenv("NATS_URL", "nats://127.0.0.1:4222")
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			_, err := loadNATSConnectionConfig()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected %q error, got %v", test.wantError, err)
			}
		})
	}
}

func TestLoadNATSConnectionConfigValidatesTLSFiles(t *testing.T) {
	for _, missing := range []string{"certificate", "key"} {
		t.Run(missing, func(t *testing.T) {
			clearNATSEnv(t)
			t.Setenv("NATS_URL", "tls://nats.example.test:4222")
			if missing == "certificate" {
				t.Setenv("NATS_TLS_KEY_FILE", "/secret/client.key")
			} else {
				t.Setenv("NATS_TLS_CERT_FILE", "/secret/client.crt")
			}
			_, err := loadNATSConnectionConfig()
			if err == nil || !strings.Contains(err.Error(), "configured together") {
				t.Fatalf("expected TLS pair error, got %v", err)
			}
		})
	}
}

func TestLoadNATSConnectionConfigSelectsCredentialsFile(t *testing.T) {
	clearNATSEnv(t)
	t.Setenv("NATS_URL", "tls://nats.example.test:4222")
	t.Setenv("NATS_CREDS_FILE", "/run/secrets/engine.creds")
	config, err := loadNATSConnectionConfig()
	if err != nil {
		t.Fatalf("load credentials-file config: %v", err)
	}
	if config.authMethod != natsAuthCreds || !config.tlsEnabled || len(config.clientOptions) != 2 {
		t.Fatalf("unexpected credentials-file config: auth=%q tls=%t options=%d", config.authMethod, config.tlsEnabled, len(config.clientOptions))
	}
}

func TestConnectNATSAuthenticatesWithToken(t *testing.T) {
	clearNATSEnv(t)
	natsServer := startAuthenticatedNATSServer(t, &server.Options{Authorization: "server-token"})
	t.Setenv("NATS_URL", natsServer.ClientURL())
	t.Setenv("NATS_TOKEN", "server-token")

	client, err := ConnectNATS()
	if err != nil {
		t.Fatalf("connect with token: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.InitStream("AUTH_TOKEN_TEST", []string{"auth.token"}); err != nil {
		t.Fatalf("authenticated JetStream access: %v", err)
	}
}

func TestConnectNATSAuthenticatesWithUsernameAndPassword(t *testing.T) {
	clearNATSEnv(t)
	natsServer := startAuthenticatedNATSServer(t, &server.Options{Username: "engine", Password: "server-password"})
	t.Setenv("NATS_URL", natsServer.ClientURL())
	t.Setenv("NATS_USERNAME", "engine")
	t.Setenv("NATS_PASSWORD", "server-password")

	client, err := ConnectNATS()
	if err != nil {
		t.Fatalf("connect with username/password: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.InitStream("AUTH_USER_TEST", []string{"auth.user"}); err != nil {
		t.Fatalf("authenticated JetStream access: %v", err)
	}
}

func TestConnectNATSAuthenticatesWithNKeySeedFile(t *testing.T) {
	clearNATSEnv(t)
	keyPair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create NKey: %v", err)
	}
	publicKey, err := keyPair.PublicKey()
	if err != nil {
		t.Fatalf("read NKey public key: %v", err)
	}
	seed, err := keyPair.Seed()
	if err != nil {
		t.Fatalf("read NKey seed: %v", err)
	}
	seedFile := t.TempDir() + "/engine.nk"
	if err := os.WriteFile(seedFile, seed, 0o600); err != nil {
		t.Fatalf("write NKey seed fixture: %v", err)
	}

	natsServer := startAuthenticatedNATSServer(t, &server.Options{Nkeys: []*server.NkeyUser{{Nkey: publicKey}}})
	t.Setenv("NATS_URL", natsServer.ClientURL())
	t.Setenv("NATS_NKEY_SEED_FILE", seedFile)
	client, err := ConnectNATS()
	if err != nil {
		t.Fatalf("connect with NKey: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.InitStream("AUTH_NKEY_TEST", []string{"auth.nkey"}); err != nil {
		t.Fatalf("NKey-authenticated JetStream access: %v", err)
	}
}

func TestConnectNATSRejectsWrongTokenWithoutExposingIt(t *testing.T) {
	clearNATSEnv(t)
	natsServer := startAuthenticatedNATSServer(t, &server.Options{Authorization: "accepted-token"})
	varz, varzErr := natsServer.Varz(nil)
	if varzErr != nil || !varz.AuthRequired {
		t.Fatalf("test NATS server did not enable authentication: varz=%+v err=%v", varz, varzErr)
	}
	t.Setenv("NATS_URL", natsServer.ClientURL())
	t.Setenv("NATS_TOKEN", "rejected-secret-token")

	client, err := ConnectNATS()
	if client != nil {
		client.Close()
	}
	if err == nil {
		t.Fatal("expected authentication to fail")
	}
	if strings.Contains(err.Error(), "rejected-secret-token") {
		t.Fatal("connection error exposed the rejected token")
	}
}

func startAuthenticatedNATSServer(t *testing.T, options *server.Options) *server.Server {
	t.Helper()
	options.Host = "127.0.0.1"
	options.Port = -1
	options.JetStream = true
	options.StoreDir = t.TempDir()
	natsServer, err := server.NewServer(options)
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatalf("NATS server did not become ready (running=%t, addr=%v, url=%q)", natsServer.Running(), natsServer.Addr(), natsServer.ClientURL())
	}
	t.Cleanup(natsServer.Shutdown)
	return natsServer
}

func clearNATSEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"NATS_URL", "NATS_CREDS_FILE", "NATS_NKEY_SEED_FILE", "NATS_TOKEN", "NATS_USERNAME", "NATS_PASSWORD",
		"NATS_TLS_CA_FILE", "NATS_TLS_CERT_FILE", "NATS_TLS_KEY_FILE", "NATS_TLS_SERVER_NAME",
	} {
		t.Setenv(name, "")
	}
}
