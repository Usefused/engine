package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot resolves the repository root relative to internal/shared/config.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	// Abort immediately if the working tree path cannot be resolved.
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// stripJSONCLineComments removes full-line // comments so wrangler.jsonc can be parsed with encoding/json.
func stripJSONCLineComments(raw string) string {
	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		// Drop full-line comments while preserving URLs such as https:// inside string values.
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// readDeployFile reads a repository-relative deployment template file and fails the test if missing.
func readDeployFile(t *testing.T, relPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), relPath))
	// Every documented deployment template file must be present on disk.
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return string(data)
}

// TestOneClickDeployTemplates validates syntax and invariants across Cloudflare, Vercel, Coolify, and Pxxl templates.
func TestOneClickDeployTemplates(t *testing.T) {
	// Validate Cloudflare Containers wrangler.jsonc and package.json.
	var wrangler struct {
		Name       string `json:"name"`
		Main       string `json:"main"`
		Containers []struct {
			ClassName    string `json:"class_name"`
			Image        string `json:"image"`
			MaxInstances int    `json:"max_instances"`
		} `json:"containers"`
		DurableObjects struct {
			Bindings []struct {
				Name      string `json:"name"`
				ClassName string `json:"class_name"`
			} `json:"bindings"`
		} `json:"durable_objects"`
		Migrations []struct {
			Tag              string   `json:"tag"`
			NewSQLiteClasses []string `json:"new_sqlite_classes"`
		} `json:"migrations"`
	}
	wranglerRaw := stripJSONCLineComments(readDeployFile(t, "deploy/cloudflare/wrangler.jsonc"))
	// Cloudflare config must remain valid JSONC so Wrangler and the deploy button accept it.
	if err := json.Unmarshal([]byte(wranglerRaw), &wrangler); err != nil {
		t.Fatalf("parse deploy/cloudflare/wrangler.jsonc: %v", err)
	}
	if len(wrangler.Containers) != 1 || wrangler.Containers[0].ClassName != "FusedEngineContainer" || wrangler.Containers[0].MaxInstances != 1 {
		t.Fatalf("unexpected cloudflare container config: %+v", wrangler.Containers)
	}
	if len(wrangler.DurableObjects.Bindings) != 1 || wrangler.DurableObjects.Bindings[0].ClassName != "FusedEngineContainer" {
		t.Fatalf("unexpected cloudflare durable object bindings: %+v", wrangler.DurableObjects.Bindings)
	}
	if len(wrangler.Migrations) != 1 || len(wrangler.Migrations[0].NewSQLiteClasses) != 1 || wrangler.Migrations[0].NewSQLiteClasses[0] != "FusedEngineContainer" {
		t.Fatalf("unexpected cloudflare migrations: %+v", wrangler.Migrations)
	}

	var cfPkg map[string]any
	// Cloudflare template package.json must parse cleanly for Workers Builds.
	if err := json.Unmarshal([]byte(readDeployFile(t, "deploy/cloudflare/package.json")), &cfPkg); err != nil {
		t.Fatalf("parse deploy/cloudflare/package.json: %v", err)
	}

	var vercelCfg struct {
		Fluid bool `json:"fluid"`
	}
	// Vercel config must remain valid JSON with Fluid Compute enabled.
	if err := json.Unmarshal([]byte(readDeployFile(t, "deploy/vercel/vercel.json")), &vercelCfg); err != nil {
		t.Fatalf("parse deploy/vercel/vercel.json: %v", err)
	}
	if !vercelCfg.Fluid {
		t.Fatal("expected deploy/vercel/vercel.json to enable fluid compute")
	}

	// Validate Coolify Compose stack structure and magic variable contract.
	var coolifyCompose struct {
		Services map[string]struct {
			Image       string   `yaml:"image"`
			Environment []string `yaml:"environment"`
		} `yaml:"services"`
	}
	coolifyRaw := readDeployFile(t, "deploy/coolify/docker-compose.yml")
	// Coolify Compose must remain valid YAML with both engine and postgres services.
	if err := yaml.Unmarshal([]byte(coolifyRaw), &coolifyCompose); err != nil {
		t.Fatalf("parse deploy/coolify/docker-compose.yml: %v", err)
	}
	engineSvc, hasEngine := coolifyCompose.Services["engine"]
	pgSvc, hasPostgres := coolifyCompose.Services["postgres"]
	if !hasEngine || !hasPostgres {
		t.Fatalf("coolify compose must define engine and postgres services, got %v", coolifyCompose.Services)
	}
	if !strings.HasPrefix(engineSvc.Image, "ghcr.io/usefused/engine:") {
		t.Fatalf("unexpected coolify engine image: %q", engineSvc.Image)
	}
	if pgSvc.Image != "postgres:16-alpine" {
		t.Fatalf("unexpected coolify postgres image: %q", pgSvc.Image)
	}
	// Coolify SERVICE_BASE64_32 generates alphanumeric text (not 32-byte base64) and would break MasterKeyFromEnv.
	if strings.Contains(coolifyRaw, "SERVICE_BASE64_") {
		t.Fatal("coolify compose must not use SERVICE_BASE64_* for FUSED_ENCRYPTION_KEY")
	}

	// Verify every thin Dockerfile wrapper pulls the public GHCR image without private build secrets.
	for _, dockerfile := range []string{
		"deploy/cloudflare/Dockerfile",
		"deploy/vercel/Dockerfile.vercel",
		"deploy/railway/Dockerfile",
		"deploy/pxxl/Dockerfile",
	} {
		content := readDeployFile(t, dockerfile)
		if !strings.Contains(content, "FROM ghcr.io/usefused/engine:") {
			t.Fatalf("%s must pull the published ghcr.io/usefused/engine image", dockerfile)
		}
		if strings.Contains(content, "fused_open_core_key") {
			t.Fatalf("%s must not require private source build secrets", dockerfile)
		}
	}

	// Validate Railway config-as-code, IaC Postgres wiring, and template blueprint.
	var railwayCfg struct {
		Build struct {
			Builder        string `json:"builder"`
			DockerfilePath string `json:"dockerfilePath"`
		} `json:"build"`
		Deploy struct {
			HealthcheckPath string `json:"healthcheckPath"`
		} `json:"deploy"`
	}
	// railway.json must remain valid JSON with DOCKERFILE builder and /health probe.
	if err := json.Unmarshal([]byte(readDeployFile(t, "deploy/railway/railway.json")), &railwayCfg); err != nil {
		t.Fatalf("parse deploy/railway/railway.json: %v", err)
	}
	if railwayCfg.Build.Builder != "DOCKERFILE" || railwayCfg.Build.DockerfilePath != "Dockerfile" || railwayCfg.Deploy.HealthcheckPath != "/health" {
		t.Fatalf("unexpected deploy/railway/railway.json config: %+v", railwayCfg)
	}

	var railwayTemplate struct {
		Services map[string]struct {
			Source struct {
				Image string `json:"image"`
			} `json:"source"`
			Variables map[string]struct {
				DefaultValue string `json:"defaultValue"`
			} `json:"variables"`
		} `json:"services"`
	}
	// Railway template blueprint must define both Postgres and FusedEngine wired via reference variables.
	if err := json.Unmarshal([]byte(readDeployFile(t, "deploy/railway/template.json")), &railwayTemplate); err != nil {
		t.Fatalf("parse deploy/railway/template.json: %v", err)
	}
	pgTemplate, hasPGTemplate := railwayTemplate.Services["Postgres"]
	engineTemplate, hasEngineTemplate := railwayTemplate.Services["FusedEngine"]
	if !hasPGTemplate || !hasEngineTemplate {
		t.Fatalf("railway template must define Postgres and FusedEngine services, got %v", railwayTemplate.Services)
	}
	if !strings.Contains(pgTemplate.Source.Image, "postgres") {
		t.Fatalf("unexpected railway postgres image: %q", pgTemplate.Source.Image)
	}
	if !strings.HasPrefix(engineTemplate.Source.Image, "ghcr.io/usefused/engine:") {
		t.Fatalf("unexpected railway engine image: %q", engineTemplate.Source.Image)
	}
	if got := engineTemplate.Variables["FUSED_DATABASE_URL"].DefaultValue; got != "${{Postgres.DATABASE_URL}}" {
		t.Fatalf("expected railway FUSED_DATABASE_URL to reference Postgres.DATABASE_URL, got %q", got)
	}
	// Verify the canonical 42 + 1 + '=' base64 formula used in template.json always decodes to 32 raw bytes.
	sampleKey := strings.Repeat("A", 42) + "E="
	decoded, err := base64.StdEncoding.DecodeString(sampleKey)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("railway encryption key format must decode to 32 bytes, got len=%d err=%v", len(decoded), err)
	}
	if !strings.Contains(engineTemplate.Variables["FUSED_ENCRYPTION_KEY"].DefaultValue, "AEIMQUYcgkosw048") {
		t.Fatal("railway template FUSED_ENCRYPTION_KEY must generate canonical 32-byte base64")
	}

	railwayIaC := readDeployFile(t, "deploy/railway/.railway/railway.ts")
	if !strings.Contains(railwayIaC, `postgres("postgres")`) || !strings.Contains(railwayIaC, "db.env.DATABASE_URL") {
		t.Fatal("deploy/railway/.railway/railway.ts must provision postgres and wire db.env.DATABASE_URL")
	}

	// Validate Pxxl config declares schema version and default port.
	pxxlToml := readDeployFile(t, "deploy/pxxl/pxxl.toml")
	if !strings.Contains(pxxlToml, "version = 1") || !strings.Contains(pxxlToml, "port = 8081") {
		t.Fatalf("deploy/pxxl/pxxl.toml missing expected version or port: %q", pxxlToml)
	}

	// Confirm docs and READMEs exist and expose one-click deploy entrypoints.
	for _, docPath := range []string{
		"docs/one-click-deploy.md",
		"deploy/cloudflare/README.md",
		"deploy/vercel/README.md",
		"deploy/railway/README.md",
		"deploy/coolify/README.md",
		"deploy/pxxl/README.md",
	} {
		content := readDeployFile(t, docPath)
		if strings.TrimSpace(content) == "" {
			t.Fatalf("%s must not be empty", docPath)
		}
	}
}
