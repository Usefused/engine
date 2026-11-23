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

// TestOneClickDeployTemplates validates syntax and invariants across Cloudflare, Vercel, Railway, Coolify, Pxxl, and Hostlier templates.
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

	var cfPkg struct {
		Cloudflare struct {
			Bindings map[string]struct {
				Description string `json:"description"`
			} `json:"bindings"`
		} `json:"cloudflare"`
	}
	// Cloudflare template package.json must parse cleanly and document required bindings for the Deploy UI.
	if err := json.Unmarshal([]byte(readDeployFile(t, "deploy/cloudflare/package.json")), &cfPkg); err != nil {
		t.Fatalf("parse deploy/cloudflare/package.json: %v", err)
	}
	cfDevVarsExample := readDeployFile(t, "deploy/cloudflare/.dev.vars.example")
	for _, secretName := range []string{"FUSED_DATABASE_URL", "FUSED_LICENSE_KEY", "FUSED_ENCRYPTION_KEY"} {
		// Each required Engine secret must appear in .dev.vars.example so Cloudflare prompts for it during one-click deploy.
		if !strings.Contains(cfDevVarsExample, secretName+"=") {
			t.Fatalf("deploy/cloudflare/.dev.vars.example must declare %s=", secretName)
		}
		// Each prompted secret must have a user-facing description in package.json cloudflare.bindings.
		if strings.TrimSpace(cfPkg.Cloudflare.Bindings[secretName].Description) == "" {
			t.Fatalf("deploy/cloudflare/package.json missing cloudflare.bindings.%s.description", secretName)
		}
	}

	var vercelCfg struct {
		Framework string `json:"framework"`
		Fluid     bool   `json:"fluid"`
	}
	// Vercel config must remain valid JSON with container framework and Fluid Compute enabled.
	if err := json.Unmarshal([]byte(readDeployFile(t, "deploy/vercel/vercel.json")), &vercelCfg); err != nil {
		t.Fatalf("parse deploy/vercel/vercel.json: %v", err)
	}
	// Explicit container framework prevents auto-detection from missing Dockerfile.vercel, and fluid enables Containers.
	if vercelCfg.Framework != "container" || !vercelCfg.Fluid {
		t.Fatalf("expected deploy/vercel/vercel.json to enable container framework and fluid compute, got %+v", vercelCfg)
	}
	// Vercel Deploy links must target the deploy/vercel subdirectory so cloning does not fork the entire repository.
	for _, linkDoc := range []string{"README.md", "docs/one-click-deploy.md", "deploy/vercel/README.md"} {
		docContent := readDeployFile(t, linkDoc)
		// Guard against regressing back to cloning the full engine repo with root-directory.
		if !strings.Contains(docContent, "engine%2Ftree%2Fmain%2Fdeploy%2Fvercel") || strings.Contains(docContent, "root-directory=deploy%2Fvercel") {
			t.Fatalf("%s must link Vercel deploy button to deploy/vercel subdirectory", linkDoc)
		}
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
	// Pxxl Web Service template must declare schema version 1 and Engine's default HTTP port.
	if !strings.Contains(pxxlToml, "version = 1") || !strings.Contains(pxxlToml, "port = 8081") {
		t.Fatalf("deploy/pxxl/pxxl.toml missing expected version or port: %q", pxxlToml)
	}

	// Validate Hostlier v2 managed-Postgres and sidecar-Postgres manifests.
	type hostlierManifest struct {
		Version         int    `yaml:"version"`
		Name            string `yaml:"name"`
		Workload        string `yaml:"workload"`
		PrimaryEndpoint string `yaml:"primaryEndpoint"`
		Source          struct {
			Image string `yaml:"image"`
		} `yaml:"source"`
		Endpoints map[string]struct {
			Exposure string `yaml:"exposure"`
			Port     int    `yaml:"port"`
			Protocol string `yaml:"protocol"`
		} `yaml:"endpoints"`
		Secrets map[string]string `yaml:"secrets"`
		Volumes map[string]struct {
			Mount string `yaml:"mount"`
		} `yaml:"volumes"`
		Health struct {
			Path string `yaml:"path"`
		} `yaml:"health"`
		Dependencies map[string]struct {
			Type   string `yaml:"type"`
			Source string `yaml:"source"`
		} `yaml:"dependencies"`
		Bindings map[string]string `yaml:"bindings"`
		Sidecars map[string]struct {
			Source struct {
				Image string `yaml:"image"`
			} `yaml:"source"`
			Endpoint struct {
				Exposure string `yaml:"exposure"`
				Port     int    `yaml:"port"`
			} `yaml:"endpoint"`
		} `yaml:"sidecars"`
	}

	var hostlierManaged hostlierManifest
	// Primary Hostlier manifest must parse as valid v2 YAML.
	if err := yaml.Unmarshal([]byte(readDeployFile(t, "deploy/hostlier/.hostlier")), &hostlierManaged); err != nil {
		t.Fatalf("parse deploy/hostlier/.hostlier: %v", err)
	}
	// Primary manifest must pull the public GHCR image and route HTTP traffic on port 8081 with /health readiness.
	if hostlierManaged.Version != 2 || hostlierManaged.Workload != "service" || !strings.HasPrefix(hostlierManaged.Source.Image, "ghcr.io/usefused/engine:") {
		t.Fatalf("unexpected deploy/hostlier/.hostlier metadata: %+v", hostlierManaged)
	}
	httpEP, hasHTTP := hostlierManaged.Endpoints["http"]
	// Primary endpoint must expose port 8081 over HTTP for Admin UI, CLI, REST, and MCP traffic.
	if hostlierManaged.PrimaryEndpoint != "http" || !hasHTTP || httpEP.Exposure != "public" || httpEP.Port != 8081 || httpEP.Protocol != "http" || hostlierManaged.Health.Path != "/health" {
		t.Fatalf("unexpected deploy/hostlier/.hostlier endpoint or health config: %+v", hostlierManaged)
	}
	// Managed Postgres dependency must provision postgres and bind its URL to FUSED_DATABASE_URL.
	dbDep, hasDBDep := hostlierManaged.Dependencies["database"]
	if !hasDBDep || dbDep.Type != "postgres" || dbDep.Source != "provision" || hostlierManaged.Bindings["FUSED_DATABASE_URL"] != "database.url" {
		t.Fatalf("unexpected deploy/hostlier/.hostlier postgres dependency/binding: deps=%+v bindings=%+v", hostlierManaged.Dependencies, hostlierManaged.Bindings)
	}
	// Engine secrets must reference Hostlier controller secrets rather than inline credentials.
	if hostlierManaged.Secrets["FUSED_LICENSE_KEY"] != "secret://fused-license-key" || hostlierManaged.Secrets["FUSED_ENCRYPTION_KEY"] != "secret://fused-encryption-key" {
		t.Fatalf("unexpected deploy/hostlier/.hostlier secrets: %+v", hostlierManaged.Secrets)
	}
	// Persistent runtime volume must mount at /app/data so embedded NATS and sandbox cache survive restarts.
	if hostlierManaged.Volumes["runtime-data"].Mount != "/app/data" {
		t.Fatalf("unexpected deploy/hostlier/.hostlier volume mount: %+v", hostlierManaged.Volumes)
	}

	var hostlierSidecar hostlierManifest
	// Sidecar Hostlier manifest must parse as valid v2 YAML with a loopback PostgreSQL 16 sidecar.
	if err := yaml.Unmarshal([]byte(readDeployFile(t, "deploy/hostlier/sidecar.hostlier")), &hostlierSidecar); err != nil {
		t.Fatalf("parse deploy/hostlier/sidecar.hostlier: %v", err)
	}
	pgSidecar, hasPGSidecar := hostlierSidecar.Sidecars["database"]
	// Sidecar variant must use postgres:16-alpine on loopback port 5432 and reference FUSED_DATABASE_URL as a secret.
	if hostlierSidecar.Version != 2 || !hasPGSidecar || pgSidecar.Source.Image != "postgres:16-alpine" || pgSidecar.Endpoint.Exposure != "loopback" || pgSidecar.Endpoint.Port != 5432 || hostlierSidecar.Secrets["FUSED_DATABASE_URL"] != "secret://fused-database-url" {
		t.Fatalf("unexpected deploy/hostlier/sidecar.hostlier database sidecar config: %+v", hostlierSidecar)
	}

	// Confirm docs, READMEs, and local deploy button SVG assets exist and are non-empty.
	for _, docPath := range []string{
		"docs/one-click-deploy.md",
		"deploy/cloudflare/README.md",
		"deploy/vercel/README.md",
		"deploy/railway/README.md",
		"deploy/coolify/README.md",
		"deploy/pxxl/README.md",
		"deploy/hostlier/README.md",
		".github/readme/deploy-coolify.svg",
		".github/readme/deploy-pxxl.svg",
		".github/readme/deploy-hostlier.svg",
	} {
		content := readDeployFile(t, docPath)
		// Reject blank documentation or SVG assets so README badges never render broken links.
		if strings.TrimSpace(content) == "" {
			t.Fatalf("%s must not be empty", docPath)
		}
	}
}
