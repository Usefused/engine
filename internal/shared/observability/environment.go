package observability

import "os"

// EngineEnvironmentEnvVar is the env var the Engine's --environment flag
// resolves into (cmd/engine/cmd/start.go), following the same
// flag-sets-env-var precedence pattern already used for FUSED_LICENSE_KEY.
const EngineEnvironmentEnvVar = "FUSED_ENGINE_ENVIRONMENT"

// EngineEnvironment resolves the deployment environment label (Task 8,
// engine_workspace_registration_plan.md): FUSED_ENGINE_ENVIRONMENT if set,
// otherwise "production" so existing single-Engine deployments see no
// behavior change. Purely an observability/UX label -- it has no effect on
// workspace resolution, database schema, or any business logic. Attached to
// OTel trace/metric resource attributes (see Init/InitMetrics)
// and echoed on the Engine's /health response so operators and the CLI can
// tell which deployment they're talking to before a destructive operation.
func EngineEnvironment() string {
	if env := os.Getenv(EngineEnvironmentEnvVar); env != "" {
		return env
	}
	return "production"
}
