package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/secretref"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// webhookConfigDocument is kind: webhook's wire shape -- an independent
// decode target from cli/internal/configfile.WebhookArtifactConfig (this
// package doesn't import the CLI module, same reason sdkConfigDocument
// exists alongside configfile.ArtifactConfig). Name is the registration
// identity for every service listed here -- see
// plans/plan-webhook-kind.md and store.WorkspaceWebhook.OwningConfigKey's
// doc comment for why there's no per-service label anymore.
type webhookConfigDocument struct {
	APIVersion string                             `json:"apiVersion"`
	Kind       string                             `json:"kind"`
	Name       string                             `json:"name"`
	Services   map[string]webhookConfigServiceDoc `json:"services"`
}

type webhookConfigServiceDoc struct {
	Secret string `json:"secret,omitempty"`
}

// webhookResolvedService is one service's fully-resolved registration
// target, computed once per plan/apply call (resolveWebhookServices) and
// reused by every step after -- never re-queried per step.
type webhookResolvedService struct {
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	Version          string
	Secret           string
}

type webhookPlanCall struct {
	apiKey    string
	accountID uuid.UUID
	// request reuses SDKConfigPlanRequest -- the wire shape (config_key,
	// source_hash, config) is identical across every kind, so a kind-
	// specific duplicate would only add drift risk, not clarity.
	request  SDKConfigPlanRequest
	document webhookConfigDocument
}

type webhookConfigApplyResult struct {
	ConfigKey string
	Name      string
	Applied   []appliedWorkspaceWebhook
}

// WebhookConfigPlanHandler owns desired-state validation for kind: webhook --
// completely Engine-owned, like fused_workspace_webhooks itself: no Registry
// round trip, no generation proxy, just a reconciliation of that table
// scoped to this artifact's own config_key. verifier is only used to fetch
// each referenced service's webhook auth shape (see resolveWebhookAuthShape),
// the same Registry-metadata call the legacy runtime_config.webhooks path
// already makes.
func WebhookConfigPlanHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook_config.plan")
		defer span.End()
		accountID, err := resolveWorkspaceActor(ctx, s, r)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"})
			return
		}
		req, doc, err := decodeWebhookConfigPlanRequest(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		span.SetAttributes(attribute.String("config_key", req.ConfigKey), attribute.String("webhook.name", doc.Name))
		plan, summary, err := createWebhookConfigPlan(ctx, configStore, s, registryClient, webhookPlanCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: accountID,
			request: req, document: doc,
		})
		if err != nil {
			span.SetStatus(codes.Error, "webhook config plan failed")
			writeSDKConfigError(w, err)
			return
		}
		span.SetAttributes(attribute.String("outcome", "success"), attribute.String("plan_id", plan.ID.String()))
		writeJSON(w, map[string]any{
			"plan_id": plan.ID.String(), "config_key": plan.ConfigKey,
			"source_hash": plan.SourceHash, "base_generation": plan.BaseGeneration,
			"summary": summary,
		})
	}
}

// WebhookConfigApplyHandler reconciles fused_workspace_webhooks rows owned by
// this artifact's config_key -- creating/updating one row per declared
// service and pruning any this apply no longer declares. No runtime,
// package, or token is produced; unlike SDK/MCP there is nothing else to
// activate.
func WebhookConfigApplyHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook_config.apply")
		defer span.End()
		accountID, err := resolveWorkspaceActor(ctx, s, r)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"})
			return
		}
		req, planID, err := decodeSDKConfigApplyRequest(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		result, err := executeWebhookConfigApply(ctx, configStore, s, verifier, registryClient, sdkApplyCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: accountID,
			planID: planID, sourceHash: req.SourceHash,
		})
		if err != nil {
			span.SetStatus(codes.Error, "webhook config apply failed")
			writeSDKConfigError(w, err)
			return
		}
		span.SetAttributes(
			attribute.String("outcome", "success"), attribute.String("webhook.name", result.Name),
			attribute.Int("webhook.registrations", len(result.Applied)),
		)
		writeJSON(w, webhookConfigApplyResponse(planID, result))
	}
}

func webhookConfigApplyResponse(planID uuid.UUID, result webhookConfigApplyResult) map[string]any {
	registrations := make([]map[string]any, 0, len(result.Applied))
	for _, applied := range result.Applied {
		registrations = append(registrations, map[string]any{"service": applied.ServiceKey, "slug": applied.Slug})
	}
	return map[string]any{
		"status": "applied", "plan_id": planID.String(), "config_key": result.ConfigKey,
		"name": result.Name, "registrations": registrations,
	}
}

// decodeWebhookConfigPlanRequest applies one strict wire decoder, mirroring
// decodeArtifactConfigPlanRequest's SDK/MCP shape, so CLI/UI cannot acquire a
// different contract for this kind.
func decodeWebhookConfigPlanRequest(r *http.Request) (SDKConfigPlanRequest, webhookConfigDocument, error) {
	var req SDKConfigPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, webhookConfigDocument{}, fmt.Errorf("invalid request body")
	}
	if strings.TrimSpace(req.SourceHash) == "" || strings.TrimSpace(req.ConfigKey) == "" {
		return req, webhookConfigDocument{}, fmt.Errorf("source_hash and config_key are required")
	}
	var doc webhookConfigDocument
	if err := json.Unmarshal(req.Config, &doc); err != nil {
		return req, doc, fmt.Errorf("invalid config json")
	}
	if err := validateWebhookConfigDocument(doc); err != nil {
		return req, doc, err
	}
	if req.ConfigKey != webhookConfigKey(doc.Name) {
		return req, doc, fmt.Errorf("config_key does not match webhook artifact identity")
	}
	return req, doc, nil
}

// webhookConfigKey has no version segment, unlike SDK/MCP's
// artifactConfigKey -- kind: webhook is a continuously-reconciled
// registration bundle (like kind: workspace), not an immutable release, so
// its identity is just its name.
func webhookConfigKey(name string) string {
	return fmt.Sprintf("webhook:%s", name)
}

// validateWebhookConfigDocument checks structure and each secret ref's
// ${bucket.<name>.secret.<key>} grammar (via secretref.Parse -- the same
// parser resolvedWorkspaceWebhookSecretRef uses at apply time) without
// resolving anything against the store, so a malformed reference is caught
// before any service/bucket lookup.
func validateWebhookConfigDocument(doc webhookConfigDocument) error {
	if doc.APIVersion != "fused/v1" || doc.Kind != "webhook" {
		return fmt.Errorf("config must use apiVersion fused/v1 and kind webhook")
	}
	if strings.TrimSpace(doc.Name) == "" {
		return fmt.Errorf("webhook config requires a name")
	}
	if len(doc.Services) == 0 {
		return fmt.Errorf("webhook config %q requires at least one service", doc.Name)
	}
	for svcName, svcDoc := range doc.Services {
		if strings.TrimSpace(svcDoc.Secret) == "" {
			continue
		}
		if _, err := secretref.Parse(svcDoc.Secret); err != nil {
			return fmt.Errorf("webhook config %q service %q secret is invalid: %w", doc.Name, svcName, err)
		}
	}
	return nil
}

// createWebhookConfigPlan resolves every referenced service/version now so
// apply never has to infer activation state, and rejects a (service, name)
// conflict at plan time rather than letting a user discover it only at
// apply.
func createWebhookConfigPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call webhookPlanCall) (*store.ConfigPlan, map[string]any, error) {
	current, err := configStore.GetConfigState(ctx, call.request.ConfigKey)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	resolved, err := resolveWebhookServices(ctx, s, registryClient, call.apiKey, call.document)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureWebhookNameAvailable(ctx, s, call.request.ConfigKey, call.document.Name, resolved); err != nil {
		return nil, nil, err
	}
	desiredState, err := json.Marshal(call.document)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to canonicalize webhook config"}
	}
	plan, err := configStore.CreateConfigPlan(ctx, store.CreateConfigPlanParams{
		ConfigKey: call.request.ConfigKey, ConfigType: store.ConfigTypeWebhook,
		SourceHash: call.request.SourceHash, BaseGeneration: currentGeneration(current), Actions: []byte("[]"),
		DesiredState: desiredState, ResolvedPayload: desiredState, Blockers: []byte("[]"), Warnings: []byte("[]"),
		CreatedBy: call.accountID, SupersedeExisting: true,
	})
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to save plan"}
	}
	return plan, map[string]any{"create_webhook": current == nil, "services": sortedWebhookServiceNames(resolved)}, nil
}

// executeWebhookConfigApply re-resolves services/versions against current
// workspace state (not just the plan's stored payload) before writing
// anything -- the same defense-in-depth ensureSDKSelectionsStillAllowed
// gives SDK/MCP applies, since a service can be deactivated or reconfigured
// between plan and apply.
func executeWebhookConfigApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, call sdkApplyCall) (webhookConfigApplyResult, error) {
	plan, err := loadArtifactPlanForApply(ctx, configStore, call, store.ConfigTypeWebhook)
	if err != nil {
		return webhookConfigApplyResult{}, err
	}
	var doc webhookConfigDocument
	if err := json.Unmarshal(plan.DesiredState, &doc); err != nil {
		return webhookConfigApplyResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid resolved webhook plan"}
	}
	resolved, err := resolveWebhookServices(ctx, s, registryClient, call.apiKey, doc)
	if err != nil {
		return webhookConfigApplyResult{}, err
	}
	if err := ensureWebhookNameAvailable(ctx, s, plan.ConfigKey, doc.Name, resolved); err != nil {
		return webhookConfigApplyResult{}, err
	}

	buckets := workspaceConnectBucketCache{}
	names := sortedWebhookServiceNames(resolved)
	applied := make([]appliedWorkspaceWebhook, 0, len(names))
	keepServiceIDs := make([]uuid.UUID, 0, len(names))
	for _, name := range names {
		r := resolved[name]
		keepServiceIDs = append(keepServiceIDs, r.ServiceID)
		authShape, eventExtractionPath, err := resolveWebhookAuthShape(ctx, s, verifier, r.ServiceID, r.Version, r.ServiceVersionID)
		if err != nil {
			return webhookConfigApplyResult{}, err
		}
		saved, err := upsertOneWorkspaceWebhook(ctx, s, r.ServiceID, r.ServiceVersionID, doc.Name, WebhookConfig{Secret: r.Secret}, authShape, eventExtractionPath, &plan.ConfigKey, buckets)
		if err != nil {
			return webhookConfigApplyResult{}, err
		}
		applied = append(applied, appliedWorkspaceWebhook{ServiceKey: name, Label: doc.Name, Slug: saved.Slug})
	}
	if _, err := s.PruneOwnedWorkspaceWebhooks(ctx, plan.ConfigKey, keepServiceIDs); err != nil {
		return webhookConfigApplyResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to prune removed webhook registrations"}
	}
	if err := persistWebhookConfigApply(ctx, configStore, call, plan); err != nil {
		return webhookConfigApplyResult{}, err
	}
	return webhookConfigApplyResult{ConfigKey: plan.ConfigKey, Name: doc.Name, Applied: applied}, nil
}

// persistWebhookConfigApply mirrors persistMCPConfigApply's transaction
// boundary (desired state + plan completion together) but has no
// LatestResourceID -- kind: webhook produces no runtime/package artifact,
// only rows in fused_workspace_webhooks that upsertOneWorkspaceWebhook
// already wrote.
func persistWebhookConfigApply(ctx context.Context, configStore store.ConfigRepository, call sdkApplyCall, plan *store.ConfigPlan) error {
	if _, err := configStore.ApplyConfigPlan(ctx, store.ApplyConfigPlanParams{
		State: store.UpsertConfigStateParams{
			ConfigKey: plan.ConfigKey, ConfigType: store.ConfigTypeWebhook,
			SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, ManagedResources: []byte("{}"),
			UpdatedBy: call.accountID,
		},
		PlanID: call.planID, BaseGeneration: plan.BaseGeneration,
	}); err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply webhook config state"}
	}
	return nil
}

// resolveWebhookServices resolves every service name in doc against this
// workspace's activated services and allowed versions in two batched calls
// (workspaceServicesByName, ListWorkspaceServiceVersionsForServices) rather
// than one query per service -- reused by both plan and apply so they can
// never resolve a service differently.
func unresolvedWebhookServiceKeys(doc webhookConfigDocument, services map[string]store.WorkspaceService) []string {
	var missing []string
	for serviceName := range doc.Services {
		if _, ok := services[serviceName]; !ok {
			missing = append(missing, serviceName)
		}
	}
	sort.Strings(missing)
	return missing
}

// resolveWebhookServices is the (service, version) lookup bottleneck
// (workspaceServicesByName, ListWorkspaceServiceVersionsForServices) rather
// than one query per service -- reused by both plan and apply so they can
// never resolve a service differently.
func resolveWebhookServices(ctx context.Context, s store.Store, registryClient sandbox.RegistryClient, apiKey string, doc webhookConfigDocument) (map[string]webhookResolvedService, error) {
	workspaceServices, err := s.ListWorkspaceServices(ctx, nil)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list workspace services"}
	}
	services := workspaceServicesByDisplayName(workspaceServices)
	missing := unresolvedWebhookServiceKeys(doc, services)
	if len(missing) > 0 {
		resolver, ok := registryClient.(sdkServiceSlugResolver)
		if ok && resolver != nil {
			resolved, err := resolver.ResolveServiceIDsBySlugs(ctx, missing, apiKey)
			if err != nil {
				return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to resolve service slugs"}
			}
			byID := workspaceServicesByID(workspaceServices)
			for _, slug := range missing {
				if activation, ok := byID[resolved[slug]]; ok {
					services[slug] = activation
				}
			}
		}
	}

	serviceIDs := make([]uuid.UUID, 0, len(doc.Services))
	for name := range doc.Services {
		activation, ok := services[name]
		if !ok {
			return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("service %s is not activated in this workspace. Run 'fused-cli workspace service add %s' to activate it.", name, name)}
		}
		serviceIDs = append(serviceIDs, activation.ServiceID)
	}
	allowedVersions, err := s.ListWorkspaceServiceVersionsForServices(ctx, serviceIDs)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list allowed versions"}
	}
	resolved := make(map[string]webhookResolvedService, len(doc.Services))
	for name, svcDoc := range doc.Services {
		activation := services[name]
		// kind: webhook has no per-service version field (unlike SDK/MCP) --
		// an empty version resolves to allowedVersions[0], the same
		// "register against the first/default enabled version" precedent
		// the legacy runtime_config.webhooks path already used.
		version, err := resolveSDKVersionAllowed(activation, "", name, allowedVersions[activation.ServiceID])
		if err != nil {
			return nil, err
		}
		resolved[name] = webhookResolvedService{
			ServiceID: activation.ServiceID, ServiceVersionID: version.ServiceVersionID,
			Version: version.Version, Secret: svcDoc.Secret,
		}
	}
	return resolved, nil
}

// ensureWebhookNameAvailable is the (service, name) uniqueness check
// (plans/plan-webhook-kind.md): one batched WorkspaceWebhookOwnersByLabel
// call across every resolved service, not one query per service, then a
// pure in-memory check of who (if anyone) already owns that pair. A nil
// owner means a legacy runtime_config.webhooks row already claimed this
// exact name for that service; a non-nil owner different from configKey
// means a different kind: webhook artifact got there first. Either way this
// is a plan/apply-time conflict, never a silent takeover of someone else's
// registration.
func ensureWebhookNameAvailable(ctx context.Context, s store.Store, configKey, name string, resolved map[string]webhookResolvedService) error {
	serviceIDs := make([]uuid.UUID, 0, len(resolved))
	for _, r := range resolved {
		serviceIDs = append(serviceIDs, r.ServiceID)
	}
	owners, err := s.WorkspaceWebhookOwnersByLabel(ctx, serviceIDs, name)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to check webhook name availability"}
	}
	for svcName, r := range resolved {
		owner, exists := owners[r.ServiceID]
		if !exists {
			continue
		}
		if owner == nil {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf(
				"service %s already has a legacy webhook registration named %q (from runtime_config.webhooks) -- rename this webhook artifact, or remove/rename the legacy registration first",
				svcName, name,
			)}
		}
		if *owner != configKey {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf(
				"service %s webhook %q is already registered by another webhook config (%s)", svcName, name, *owner,
			)}
		}
	}
	return nil
}

// sortedWebhookServiceNames keeps plan summaries and apply's upsert order
// deterministic -- ranging a Go map directly would make plan output and
// audit span ordering vary run to run for the same config.
func sortedWebhookServiceNames(resolved map[string]webhookResolvedService) []string {
	names := make([]string, 0, len(resolved))
	for name := range resolved {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
