package config

import (
	"fmt"
	"os"

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

type StorageConfig struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
}

type EngineWorkerCounts struct {
	MCPAnalytics     int `yaml:"mcp_analytics"`
	WebhookConfig    int `yaml:"webhook_config"`
	WebhookAnalytics int `yaml:"webhook_analytics"`
}

type EngineConfig struct {
	RegistryEndpoint string             `yaml:"registry_endpoint"`
	AccountID        string             `yaml:"account_id"`
	LicenseKey       string             `yaml:"license_key"`
	WorkerCounts     EngineWorkerCounts `yaml:"worker_counts"`
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
	UIURL           string           `yaml:"ui_url"`
	// HomepageURL is the public marketing site's origin (homepage/, split out
	// in Sprint 3). The Registry's CORS allowlist needs both this and UIURL --
	// two separate deployments, two separate origins. The Engine never needs
	// this: its UI only ever calls the Engine directly, never the Registry.
	HomepageURL   string              `yaml:"homepage_url"`
	BackendURL    string              `yaml:"backend_url"`
	Credits       CreditConfig        `yaml:"credits"`
	Sandbox       SandboxConfig       `yaml:"sandbox"`
	Storage       StorageConfig       `yaml:"storage"`
	Engine        EngineConfig        `yaml:"engine"`
	Observability ObservabilityConfig `yaml:"observability"`
}

// Load reads the YAML configuration file and parses it.
// If the file does not exist, it returns a Config struct with default values.
func Load(path string) (*Config, error) {
	// Set defaults
	cfg := &Config{
		EncryptionKey: "fused-default-encrypt-key-32b",
		WorkerPool: WorkerPoolConfig{
			Size: 5, // Default worker pool size
		},
		DriftWorkerPool: WorkerPoolConfig{
			Size: 3, // Default drift worker pool size
		},
		UIURL:       "http://localhost:5173", // Default UI URL
		HomepageURL: "http://localhost:3000", // Default homepage URL (matches homepage/Dockerfile's PORT=3000)
		BackendURL:  "wss://run.usefused.com",
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
			RegistryEndpoint: "https://registry.usefused.com/graphql",
			WorkerCounts: EngineWorkerCounts{
				MCPAnalytics:     2,
				WebhookConfig:    2,
				WebhookAnalytics: 2,
			},
		},
	}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	// Env vars always win over both defaults and YAML config.
	if envKey := os.Getenv("FUSED_ENCRYPTION_KEY"); envKey != "" {
		cfg.EncryptionKey = envKey
	}
	if envLicenseKey := os.Getenv("FUSED_LICENSE_KEY"); envLicenseKey != "" {
		cfg.Engine.LicenseKey = envLicenseKey
	}
	if envRegistryEndpoint := os.Getenv("FUSED_REGISTRY_ENDPOINT"); envRegistryEndpoint != "" {
		cfg.Engine.RegistryEndpoint = envRegistryEndpoint
	}
	if envUIURL := os.Getenv("FUSED_UI_URL"); envUIURL != "" {
		cfg.UIURL = envUIURL
	}

	// S3 Storage Environment Variables
	if ep := os.Getenv("FUSED_S3_ENDPOINT"); ep != "" {
		cfg.Storage.Endpoint = ep
	}
	if bkt := os.Getenv("FUSED_S3_BUCKET"); bkt != "" {
		cfg.Storage.Bucket = bkt
	}
	if reg := os.Getenv("FUSED_S3_REGION"); reg != "" {
		cfg.Storage.Region = reg
	}
	if ak := os.Getenv("FUSED_S3_ACCESS_KEY"); ak != "" {
		cfg.Storage.AccessKey = ak
	}
	if sk := os.Getenv("FUSED_S3_SECRET_KEY"); sk != "" {
		cfg.Storage.SecretKey = sk
	}

	// Validate and expose encryption key globally.
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

	if cfg.WorkerPool.Size <= 0 {
		cfg.WorkerPool.Size = 1
	}

	return cfg, nil
}
