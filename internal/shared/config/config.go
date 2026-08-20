package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type WorkerPoolConfig struct {
	Size int `yaml:"size"`
}

type SandboxRateLimitConfig struct {
	SSEConnectionsPerMinute int `yaml:"sse_connections_per_minute"`
	SSEBurst                int `yaml:"sse_burst"`
	MessagesPerMinute       int `yaml:"messages_per_minute"`
	MessagesBurst           int `yaml:"messages_burst"`
}

type SandboxConfig struct {
	ToolCallTimeoutSeconds int                    `yaml:"tool_call_timeout_seconds"`
	SessionMaxAgeSeconds   int                    `yaml:"session_max_age_seconds"`
	RateLimit              SandboxRateLimitConfig `yaml:"rate_limit"`
}

type CreditConfig struct {
	SDKGeneration          float64        `yaml:"sdk_generation"`
	MCPGeneration          float64        `yaml:"mcp_generation"`
	MCPSandboxRequest      float64        `yaml:"mcp_sandbox_request"`
	OpenAPIGeneration      float64        `yaml:"openapi_generation"`
	LLMPer1kTokens         float64        `yaml:"llm_per_1k_tokens"`
	AddEndpoint            float64        `yaml:"add_endpoint"`
	DriftMonitoring        float64        `yaml:"drift_monitoring"`
	WebhookIngestionCharge float64        `yaml:"webhook_ingestion_charge"`
	InitialCreditBalance   float64        `yaml:"initial_credit_balance"`
	Bundles                []CreditBundle `yaml:"bundles"`
}

type CreditBundle struct {
	ID       string  `yaml:"id" json:"id"`
	Name     string  `yaml:"name" json:"name"`
	Credits  float64 `yaml:"credits" json:"credits"`
	PriceUSD float64 `yaml:"price_usd" json:"price_usd"`
}

const DefaultInitialCreditBalance = 10_000_000.0

var GlobalEncryptionKey []byte

type EngineConfig struct {
	RegistryEndpoint       string `yaml:"registry_endpoint"`
	PublicURL              string `yaml:"public_url"`
	PublicGRPCURL          string `yaml:"public_grpc_url"`
	AccountID              string `yaml:"account_id"`
	LicenseKey             string `yaml:"license_key"`
	LicenseKeySource       string `yaml:"-"`
	ExecutionRetentionDays int    `yaml:"execution_retention_days"`
	ExecutionCleanupBatch  int    `yaml:"execution_cleanup_batch"`
	// ConnectedAuthRefreshWorkers is the exact bounded OAuth refresh pool size
	// selected by the Engine operator.
	ConnectedAuthRefreshWorkers int `yaml:"connected_auth_refresh_workers"`
}

const (
	// DefaultConnectedAuthRefreshWorkers keeps provider token traffic conservative
	// while still allowing independent connections to make progress in parallel.
	DefaultConnectedAuthRefreshWorkers = 4
	// MaxConnectedAuthRefreshWorkers bounds provider and database pressure caused
	// by an accidental or hostile deployment configuration.
	MaxConnectedAuthRefreshWorkers = 64
)

type EngineLicenseSources struct {
	Flag        string
	DotEnv      string
	Environment string
}

type loadOptions struct {
	engineLicense *EngineLicenseSources
}

type LoadOption func(*loadOptions)

func WithEngineLicenseSources(sources EngineLicenseSources) LoadOption {
	return func(options *loadOptions) {
		options.engineLicense = &sources
	}
}

type ObservabilityConfig struct {
	OTELTarget string `yaml:"otel_target"`
}

type Config struct {
	EncryptionKey string `yaml:"encryption_key"`
	Database      struct {
		URL string `yaml:"url"`
	} `yaml:"database"`
	WorkerPool      WorkerPoolConfig `yaml:"worker_pool"`
	DriftWorkerPool WorkerPoolConfig `yaml:"drift_worker_pool"`
	// HomepageURL remains shared configuration for Registry-oriented packages;
	// Engine's embedded UI is same-origin and needs no configurable UI origin.
	HomepageURL   string              `yaml:"homepage_url"`
	Credits       CreditConfig        `yaml:"credits"`
	Sandbox       SandboxConfig       `yaml:"sandbox"`
	Engine        EngineConfig        `yaml:"engine"`
	Observability ObservabilityConfig `yaml:"observability"`
}

// Load reads the YAML configuration file and parses it.
// If the file does not exist, it returns a Config struct with default values.
func Load(path string, options ...LoadOption) (*Config, error) {
	cfg := defaultConfig()
	if err := loadYAML(path, cfg); err != nil {
		return nil, err
	}
	settings := loadOptions{}
	for _, option := range options {
		option(&settings)
	}
	if err := applyEnvironment(cfg); err != nil {
		return nil, err
	}
	resolveEngineLicense(cfg, settings)
	finalizeEncryptionKey(cfg)
	if cfg.WorkerPool.Size <= 0 {
		cfg.WorkerPool.Size = 1
	}
	if err := validateConnectedAuthRefreshWorkers(cfg.Engine.ConnectedAuthRefreshWorkers); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultConfig supplies non-secret development defaults while leaving
// deployment identity and credentials to explicit Engine configuration.
func defaultConfig() *Config {
	return &Config{
		EncryptionKey: "fused-default-encrypt-key-32b",
		WorkerPool: WorkerPoolConfig{
			Size: 5, // Default worker pool size
		},
		DriftWorkerPool: WorkerPoolConfig{
			Size: 3, // Default drift worker pool size
		},
		HomepageURL: "http://localhost:3000", // Default homepage URL (matches homepage/Dockerfile's PORT=3000)
		Sandbox: SandboxConfig{
			ToolCallTimeoutSeconds: 45,
			SessionMaxAgeSeconds:   300,
			RateLimit: SandboxRateLimitConfig{
				SSEConnectionsPerMinute: 5,
				SSEBurst:                2,
				MessagesPerMinute:       60,
				MessagesBurst:           10,
			},
		},
		Credits: CreditConfig{
			SDKGeneration:          1.0,
			MCPGeneration:          1.0,
			MCPSandboxRequest:      0.1,
			OpenAPIGeneration:      0.5,
			LLMPer1kTokens:         0.0001,
			AddEndpoint:            0.2,
			DriftMonitoring:        0.5,
			WebhookIngestionCharge: 1.0,
			InitialCreditBalance:   DefaultInitialCreditBalance,
			Bundles: []CreditBundle{
				{ID: "starter", Name: "Starter", Credits: 200, PriceUSD: 9},
				{ID: "pro", Name: "Pro", Credits: 1000, PriceUSD: 30},
				{ID: "enterprise", Name: "Enterprise", Credits: 100000, PriceUSD: 119},
			},
		},
		Engine: EngineConfig{
			RegistryEndpoint:            "https://registry.usefused.com/graphql",
			ExecutionRetentionDays:      30,
			ExecutionCleanupBatch:       1000,
			ConnectedAuthRefreshWorkers: DefaultConnectedAuthRefreshWorkers,
		},
	}
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, cfg)
}

// applyEnvironment accepts only Engine-owned process overrides and rejects
// malformed values before the Engine can start with unexpected concurrency.
func applyEnvironment(cfg *Config) error {
	if envKey := os.Getenv("FUSED_ENCRYPTION_KEY"); envKey != "" {
		cfg.EncryptionKey = envKey
	}
	if envRegistryEndpoint := os.Getenv("FUSED_REGISTRY_ENDPOINT"); envRegistryEndpoint != "" {
		cfg.Engine.RegistryEndpoint = envRegistryEndpoint
	}
	if publicURL := os.Getenv("FUSED_ENGINE_PUBLIC_URL"); publicURL != "" {
		cfg.Engine.PublicURL = publicURL
	}
	if publicGRPCURL := os.Getenv("FUSED_ENGINE_PUBLIC_GRPC_URL"); publicGRPCURL != "" {
		cfg.Engine.PublicGRPCURL = publicGRPCURL
	}
	if rawWorkers := os.Getenv("FUSED_ENGINE_CONNECTED_AUTH_REFRESH_WORKERS"); rawWorkers != "" {
		workers, err := strconv.Atoi(rawWorkers)
		if err != nil {
			// Why: an operator-provided override must not quietly become a
			// different concurrency level after a typo.
			return fmt.Errorf("parse FUSED_ENGINE_CONNECTED_AUTH_REFRESH_WORKERS: %w", err)
		}
		cfg.Engine.ConnectedAuthRefreshWorkers = workers
	}
	return nil
}

// validateConnectedAuthRefreshWorkers enforces the supported provider-refresh
// concurrency range for both YAML and environment configuration.
func validateConnectedAuthRefreshWorkers(workers int) error {
	if workers < 1 || workers > MaxConnectedAuthRefreshWorkers {
		// Why: zero disables a required reliability path, while an excessive
		// value can overload provider token endpoints and the Engine database.
		return fmt.Errorf("engine.connected_auth_refresh_workers must be between 1 and %d, got %d", MaxConnectedAuthRefreshWorkers, workers)
	}
	return nil
}

func resolveEngineLicense(cfg *Config, options loadOptions) {
	if options.engineLicense == nil {
		resolveDefaultLicense(cfg)
		return
	}
	sources := *options.engineLicense
	// Startup passes the sources separately because collapsing .env and process
	// env would make a checked-in deployment choice depend on the parent shell.
	switch {
	case sources.Flag != "":
		cfg.Engine.LicenseKey, cfg.Engine.LicenseKeySource = sources.Flag, "flag"
	case sources.DotEnv != "":
		cfg.Engine.LicenseKey, cfg.Engine.LicenseKeySource = sources.DotEnv, "dotenv"
	case cfg.Engine.LicenseKey != "":
		cfg.Engine.LicenseKeySource = "yaml"
	case sources.Environment != "":
		cfg.Engine.LicenseKey, cfg.Engine.LicenseKeySource = sources.Environment, "environment"
	default:
		cfg.Engine.LicenseKeySource = "missing"
	}
}

func resolveDefaultLicense(cfg *Config) {
	if envLicenseKey := os.Getenv("FUSED_LICENSE_KEY"); envLicenseKey != "" {
		cfg.Engine.LicenseKey, cfg.Engine.LicenseKeySource = envLicenseKey, "environment"
		return
	}
	if cfg.Engine.LicenseKey != "" {
		cfg.Engine.LicenseKeySource = "yaml"
		return
	}
	cfg.Engine.LicenseKeySource = "missing"
}

func finalizeEncryptionKey(cfg *Config) {
	const defaultKey = "fused-default-encrypt-key-32b"
	if cfg.EncryptionKey == "" {
		cfg.EncryptionKey = defaultKey
	}
	if cfg.EncryptionKey == defaultKey || cfg.EncryptionKey == "fused-default-encrypt-key-32b" {
		fmt.Fprintln(os.Stderr, "WARNING: using the default encryption key — set FUSED_ENCRYPTION_KEY in production")
	}
	if len(cfg.EncryptionKey) >= 32 {
		GlobalEncryptionKey = []byte(cfg.EncryptionKey[:32])
	} else {
		b := make([]byte, 32)
		copy(b, defaultKey)
		copy(b, cfg.EncryptionKey)
		GlobalEncryptionKey = b
	}
}
