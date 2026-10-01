package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DatabaseConfig keeps pool sizing independent of provider-specific DSN parameters.
type DatabaseConfig struct {
	URL             string        `yaml:"url"`
	MaxConns        int32         `yaml:"max_conns"`
	MaxConnIdleTime time.Duration `yaml:"max_conn_idle_time"`
}

// ServerConfig describes local listeners; public advertised URLs remain in engine.
type ServerConfig struct {
	HTTPPort    string `yaml:"http_port"`
	GRPCHost    string `yaml:"grpc_host"`
	GRPCPort    string `yaml:"grpc_port"`
	WebhookPort string `yaml:"webhook_port"`
}

// NATSConfig exposes deployment settings while bearer credentials stay in environment or files.
type NATSConfig struct {
	URL               string        `yaml:"url"`
	StoreDir          string        `yaml:"store_dir"`
	RateLimitReplicas int           `yaml:"rate_limit_replicas"`
	CredentialsFile   string        `yaml:"credentials_file"`
	NKeySeedFile      string        `yaml:"nkey_seed_file"`
	TLS               NATSTLSConfig `yaml:"tls"`
}

type NATSTLSConfig struct {
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
}

// DefaultDatabaseConfig preserves the existing lazy, bounded connection pool policy.
func DefaultDatabaseConfig() DatabaseConfig {
	return DatabaseConfig{MaxConns: 10, MaxConnIdleTime: 30 * time.Minute}
}

// ResolveDatabaseConfig validates effective pool settings before startup publishes YAML environment defaults.
func ResolveDatabaseConfig(policy DatabaseConfig) (DatabaseConfig, error) {
	// Nonempty environment values retain the legacy override semantics.
	if raw := strings.TrimSpace(os.Getenv("FUSED_DATABASE_MAX_CONNS")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 32)
		// Reject malformed capacity rather than silently selecting another pool size.
		if err != nil {
			return policy, fmt.Errorf("FUSED_DATABASE_MAX_CONNS must be a positive integer")
		}
		policy.MaxConns = int32(value)
	}
	// Duration strings retain the existing Go duration syntax.
	if raw := strings.TrimSpace(os.Getenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME")); raw != "" {
		value, err := time.ParseDuration(raw)
		// Invalid environment input must fail before opening database connections.
		if err != nil {
			return policy, fmt.Errorf("FUSED_DATABASE_MAX_CONN_IDLE_TIME must be a positive duration")
		}
		policy.MaxConnIdleTime = value
	}
	return policy, policy.Validate()
}

// Validate prevents YAML and environment input from disabling or overflowing pool policy.
func (policy DatabaseConfig) Validate() error {
	// Both settings must remain positive to retain bounded, lazy database resource usage.
	if policy.MaxConns < 1 || policy.MaxConnIdleTime <= 0 {
		return fmt.Errorf("database.max_conns and database.max_conn_idle_time must be positive")
	}
	return nil
}

// ResolveServerConfig admits new environment equivalents without changing explicit flag precedence.
func ResolveServerConfig(server ServerConfig) ServerConfig {
	for name, destination := range map[string]*string{
		"FUSED_ENGINE_HTTP_PORT":    &server.HTTPPort,
		"FUSED_ENGINE_GRPC_HOST":    &server.GRPCHost,
		"FUSED_ENGINE_GRPC_PORT":    &server.GRPCPort,
		"FUSED_ENGINE_WEBHOOK_PORT": &server.WebhookPort,
	} {
		// Unset or empty variables preserve YAML and historical listener defaults.
		if value := os.Getenv(name); value != "" {
			*destination = value
		}
	}
	return server
}

// Validate checks the final listener selection after explicit CLI flags are applied.
func (server ServerConfig) Validate() error {
	for name, port := range map[string]string{"http_port": server.HTTPPort, "grpc_port": server.GRPCPort, "webhook_port": server.WebhookPort} {
		// An absent dedicated webhook listener intentionally shares the main HTTP server.
		if name == "webhook_port" && port == "" {
			continue
		}
		value, err := strconv.Atoi(port)
		// Port zero remains supported for ephemeral local listeners.
		if err != nil || value < 0 || value > 65535 {
			return fmt.Errorf("server.%s must be a port between 0 and 65535", name)
		}
	}
	return nil
}
