package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// AgentConfig controls the optional workspace agent independently of app execution workers.
type AgentConfig struct {
	Enabled        bool               `yaml:"enabled"`
	CacheDir       string             `yaml:"cache_dir"`
	RuntimeArchive string             `yaml:"runtime_archive"`
	Gateway        AgentGatewayConfig `yaml:"gateway"`
}

// AgentGatewayConfig defaults to Registry; custom providers require their own explicit credential variable.
type AgentGatewayConfig struct {
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// ResolveAgentConfig keeps environment-only installations compatible with engine.yaml configuration.
func ResolveAgentConfig(value AgentConfig) (AgentConfig, error) {
	// An explicit environment enablement overrides the YAML default without enabling on malformed values.
	if raw, ok := os.LookupEnv("FUSED_AGENT_ENABLED"); ok {
		enabled, err := strconv.ParseBool(raw)
		// Incomplete configuration or runtime state must not grant an unverified agent connection.
		if err != nil {
			return value, fmt.Errorf("FUSED_AGENT_ENABLED must be a boolean")
		}
		value.Enabled = enabled
	}
	for name, target := range map[string]*string{"FUSED_AGENT_CACHE_DIR": &value.CacheDir, "FUSED_AGENT_RUNTIME_ARCHIVE": &value.RuntimeArchive, "FUSED_AGENT_GATEWAY_URL": &value.Gateway.BaseURL, "FUSED_AGENT_MODEL": &value.Gateway.Model, "FUSED_AGENT_API_KEY_ENV": &value.Gateway.APIKeyEnv} {
		// Missing variables preserve operator-authored YAML rather than zeroing it.
		if raw, ok := os.LookupEnv(name); ok {
			*target = strings.TrimSpace(raw)
		}
	}
	// Validate configured endpoints even while disabled so a later enablement cannot silently redirect credentials.
	for _, endpoint := range []string{value.Gateway.BaseURL} {
		// Incomplete configuration or runtime state must not grant an unverified agent connection.
		if endpoint != "" {
			// Incomplete configuration or runtime state must not grant an unverified agent connection.
			if err := ValidateAgentURL(endpoint); err != nil {
				return value, err
			}
		}
	}
	return value, nil
}

// ValidateAgentURL permits local development while retaining TLS for remote model and runtime credentials.
func ValidateAgentURL(raw string) error {
	u, err := url.Parse(raw)
	// Userinfo and query fields cannot smuggle credentials into logs or alter the configured gateway.
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("agent endpoint must be an HTTP(S) URL without userinfo, query or fragment")
	}
	// HTTP is restricted to loopback, never arbitrary internal-network credential transport.
	if u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil
	}
	return fmt.Errorf("agent endpoint requires HTTPS except on loopback")
}
