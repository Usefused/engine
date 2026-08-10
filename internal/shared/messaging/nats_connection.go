package messaging

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/nats-io/nats.go"
)

const (
	natsAuthNone     = "none"
	natsAuthToken    = "token"
	natsAuthUserPass = "user_password"
	natsAuthCreds    = "credentials_file"
	natsAuthNKey     = "nkey_seed_file"
)

type natsConnectionConfig struct {
	url           string
	external      bool
	authMethod    string
	tlsEnabled    bool
	clientOptions []nats.Option
}

func loadNATSConnectionConfig() (natsConnectionConfig, error) {
	rawURL := strings.TrimSpace(os.Getenv("NATS_URL"))
	config := natsConnectionConfig{url: rawURL, external: rawURL != "", authMethod: natsAuthNone}
	if !config.external {
		config.url = nats.DefaultURL
		if hasExternalNATSSettings() {
			return natsConnectionConfig{}, errors.New("NATS_URL is required when external NATS authentication or TLS is configured")
		}
		return config, nil
	}
	if hasURLCredentials(rawURL) {
		return natsConnectionConfig{}, errors.New("NATS_URL must not contain credentials; use dedicated NATS authentication environment variables")
	}
	authMethod, authOptions, err := natsAuthenticationOptions()
	if err != nil {
		return natsConnectionConfig{}, err
	}
	tlsEnabled, tlsOptions, err := natsTLSOptions(rawURL)
	if err != nil {
		return natsConnectionConfig{}, err
	}
	config.authMethod = authMethod
	config.tlsEnabled = tlsEnabled
	config.clientOptions = append(authOptions, tlsOptions...)
	return config, nil
}

func hasExternalNATSSettings() bool {
	for _, name := range []string{
		"NATS_CREDS_FILE", "NATS_NKEY_SEED_FILE", "NATS_TOKEN", "NATS_USERNAME", "NATS_PASSWORD",
		"NATS_TLS_CA_FILE", "NATS_TLS_CERT_FILE", "NATS_TLS_KEY_FILE", "NATS_TLS_SERVER_NAME",
	} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

func hasURLCredentials(raw string) bool {
	for _, item := range strings.Split(raw, ",") {
		parsed, err := url.Parse(strings.TrimSpace(item))
		if err == nil && parsed.User != nil {
			return true
		}
	}
	return false
}

func natsAuthenticationOptions() (string, []nats.Option, error) {
	config := natsAuthenticationConfig{
		credsFile: strings.TrimSpace(os.Getenv("NATS_CREDS_FILE")),
		nkeyFile:  strings.TrimSpace(os.Getenv("NATS_NKEY_SEED_FILE")),
		token:     os.Getenv("NATS_TOKEN"),
		username:  os.Getenv("NATS_USERNAME"),
		password:  os.Getenv("NATS_PASSWORD"),
	}
	if err := config.validate(); err != nil {
		return "", nil, err
	}
	return config.options()
}

type natsAuthenticationConfig struct {
	credsFile string
	nkeyFile  string
	token     string
	username  string
	password  string
}

func (c natsAuthenticationConfig) validate() error {
	userPasswordSet := c.username != "" || c.password != ""
	if userPasswordSet && (c.username == "" || c.password == "") {
		return errors.New("NATS_USERNAME and NATS_PASSWORD must be configured together")
	}
	configured := boolCount(c.credsFile != "", c.nkeyFile != "", c.token != "", userPasswordSet)
	if configured > 1 {
		return errors.New("configure exactly one NATS authentication method")
	}
	return nil
}

func (c natsAuthenticationConfig) options() (string, []nats.Option, error) {
	switch {
	case c.credsFile != "":
		return natsAuthCreds, []nats.Option{nats.UserCredentials(c.credsFile)}, nil
	case c.nkeyFile != "":
		option, err := nats.NkeyOptionFromSeed(c.nkeyFile)
		if err != nil {
			return "", nil, fmt.Errorf("load NATS NKey seed file: %w", err)
		}
		return natsAuthNKey, []nats.Option{option}, nil
	case c.token != "":
		return natsAuthToken, []nats.Option{nats.Token(c.token)}, nil
	case c.username != "":
		return natsAuthUserPass, []nats.Option{nats.UserInfo(c.username, c.password)}, nil
	default:
		return natsAuthNone, nil, nil
	}
}

func natsTLSOptions(rawURL string) (bool, []nats.Option, error) {
	caFile := strings.TrimSpace(os.Getenv("NATS_TLS_CA_FILE"))
	certFile := strings.TrimSpace(os.Getenv("NATS_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("NATS_TLS_KEY_FILE"))
	serverName := strings.TrimSpace(os.Getenv("NATS_TLS_SERVER_NAME"))
	if (certFile == "") != (keyFile == "") {
		return false, nil, errors.New("NATS_TLS_CERT_FILE and NATS_TLS_KEY_FILE must be configured together")
	}
	enabled := caFile != "" || certFile != "" || serverName != "" || natsURLUsesTLS(rawURL)
	if !enabled {
		return false, nil, nil
	}
	// An explicit minimum avoids inheriting legacy TLS defaults when the
	// external cluster is operated outside the Engine deployment.
	options := []nats.Option{nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName})}
	if caFile != "" {
		options = append(options, nats.RootCAs(caFile))
	}
	if certFile != "" {
		options = append(options, nats.ClientCert(certFile, keyFile))
	}
	return true, options, nil
}

func natsURLUsesTLS(raw string) bool {
	for _, item := range strings.Split(raw, ",") {
		parsed, err := url.Parse(strings.TrimSpace(item))
		if err == nil && (parsed.Scheme == "tls" || parsed.Scheme == "wss") {
			return true
		}
	}
	return false
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}
