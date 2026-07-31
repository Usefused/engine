package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type mcpConfigApplyResult struct {
	RuntimeID      uuid.UUID
	RuntimeURL     string
	ExecutionToken string
	ConfigKey      string
	Name           string
	Version        string
	SourceHash     string
}

// MCPConfigPlanHandler owns desired-state validation for Engine-projected MCP
// runtimes. It deliberately shares SDK selection and contract resolution, but
// stores an MCP plan so apply can never cross into code generation.
func MCPConfigPlanHandler(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.mcp_config.plan")
		defer span.End()
		accountID, err := resolveWorkspaceActor(ctx, s, r)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"})
			return
		}
		req, doc, err := decodeArtifactConfigPlanRequest(r, "mcp")
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		setSDKConfigSpanAttributes(span, req.ConfigKey, doc)
		result, err := createMCPConfigPlan(ctx, configStore, s, registryClient, sdkPlanCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: accountID,
			request: req, document: doc,
		})
		if err != nil {
			span.SetStatus(codes.Error, "mcp config plan failed")
			writeSDKConfigError(w, err)
			return
		}
		span.SetAttributes(attribute.String("outcome", "success"), attribute.String("plan_id", result.plan.ID.String()))
		writeJSON(w, map[string]any{
			"plan_id": result.plan.ID.String(), "config_key": result.plan.ConfigKey,
			"source_hash": result.plan.SourceHash, "base_generation": result.plan.BaseGeneration,
			"summary": result.summary, "notifications": result.notifications,
		})
	}
}

// MCPConfigApplyHandler activates a resolved Engine scope directly. No
// Registry SDK generation request or artifact record exists on this path.
func MCPConfigApplyHandler(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.mcp_config.apply")
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
		result, err := executeMCPConfigApply(ctx, configStore, s, registryClient, sdkApplyCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: accountID,
			planID: planID, sourceHash: req.SourceHash,
		})
		if err != nil {
			span.SetStatus(codes.Error, "mcp config apply failed")
			writeSDKConfigError(w, err)
			return
		}
		result.RuntimeURL = mcpURLForSDK(r, result.RuntimeID)
		span.SetAttributes(
			attribute.String("outcome", "success"), attribute.String("artifact.kind", "mcp"),
			attribute.String("artifact.name", result.Name), attribute.String("artifact.version", result.Version),
			attribute.String("runtime_id", result.RuntimeID.String()),
		)
		writeJSON(w, mcpConfigApplyResponse(planID, result))
	}
}

// mcpConfigApplyResponse keeps MCP's one-time token on the shared artifact
// wire key so fused-cli can use one decoder for SDK and MCP apply responses.
func mcpConfigApplyResponse(planID uuid.UUID, result mcpConfigApplyResult) map[string]any {
	resp := map[string]any{
		"status": "applied", "plan_id": planID.String(), "config_key": result.ConfigKey,
		"name": result.Name, "version": result.Version, "artifact_id": result.RuntimeID.String(),
		"mcp_url": result.RuntimeURL,
	}
	if result.ExecutionToken != "" {
		// Token is only present on first creation. Absent on idempotent re-apply.
		resp["execution_token"] = result.ExecutionToken
		resp["token_note"] = "reusable until rotated; store securely"
	}
	return resp
}

// decodeArtifactConfigPlanRequest applies one strict wire decoder to SDK and
// MCP documents so UI, CLI, and GraphQL cannot acquire different contracts.
func decodeArtifactConfigPlanRequest(r *http.Request, kind string) (SDKConfigPlanRequest, sdkConfigDocument, error) {
	var req SDKConfigPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, sdkConfigDocument{}, errors.New("invalid request body")
	}
	if strings.TrimSpace(req.SourceHash) == "" || strings.TrimSpace(req.ConfigKey) == "" {
		return req, sdkConfigDocument{}, errors.New("source_hash and config_key are required")
	}
	if err := rejectRemovedSDKConfigFields(req.Config); err != nil {
		return req, sdkConfigDocument{}, err
	}
	var doc sdkConfigDocument
	if err := decodeArtifactConfigJSON(req.Config, &doc); err != nil {
		return req, doc, errors.New("invalid config json")
	}
	if err := validateArtifactConfigDocument(doc, kind); err != nil {
		return req, doc, err
	}
	if req.ConfigKey != fmt.Sprintf("%s:%s:%s", kind, doc.Name, doc.Version) {
		return req, doc, fmt.Errorf("config_key does not match %s artifact identity", kind)
	}
	return req, doc, nil
}

// validateArtifactConfigDocument enforces the shared artifact identity while
// keeping generation-only fields out of an Engine-projected MCP runtime.
func validateArtifactConfigDocument(doc sdkConfigDocument, kind string) error {
	if doc.APIVersion != "fused/v1" || doc.Kind != kind {
		return fmt.Errorf("config must use apiVersion fused/v1 and kind %s", kind)
	}
	if strings.TrimSpace(doc.Name) == "" || !sdkArtifactVersionPattern.MatchString(doc.Version) {
		return fmt.Errorf("%s config requires name and a SemVer-compatible version", kind)
	}
	if strings.TrimSpace(doc.Bucket) == "" {
		return fmt.Errorf("%s config requires exactly one bucket", kind)
	}
	if kind == "mcp" && strings.TrimSpace(doc.Language) != "" {
		return errors.New("mcp config must not set language")
	}
	if kind == "mcp" {
		for name, service := range doc.Services {
			// WebhooksSelectAll is checked alongside Webhooks -- it's the
			// webhook-only counterpart to SelectAll (see
			// models.SDKSelection.WebhookSelectAll's doc comment) added
			// alongside webhook_attachment, and mirrors the CLI's own
			// validateArtifactServices check (cli/internal/configfile/parser.go),
			// which already rejects both for MCP.
			if len(service.Webhooks) > 0 || service.WebhooksSelectAll {
				return fmt.Errorf("mcp service %s cannot select webhooks", name)
			}
		}
	}
	return validateArtifactServiceDocs(doc.Services)
}

// createMCPConfigPlan resolves service versions, operations, and auth policy
// now so apply and later agent calls never need to infer provider setup.
func createMCPConfigPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call sdkPlanCall) (sdkPlanResult, error) {
	current, err := configStore.GetConfigState(ctx, call.request.ConfigKey)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	selections, services, resolved, stateDoc, err := resolveSDKSelections(ctx, configStore, s, registryClient, call.apiKey, call.document, previousSDKDocument(current))
	if err != nil {
		return sdkPlanResult{}, err
	}
	bindings, err := resolveSDKContractBindings(ctx, registryClient, call.apiKey, resolved)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to bind service contract revisions"}
	}
	selections = attachSDKServiceVersionIDs(selections, bindings)
	
	// Resolve EndpointIDs for MCP so the scope is strictly tied to immutable endpoint IDs
	for i, sel := range selections {
		if len(sel.OperationNames) > 0 && len(sel.EndpointIDs) == 0 {
			endpoints, err := registryClient.FetchEndpointsByNames(ctx, sel.ServiceID, sel.ServiceVersionID, sel.OperationNames)
			if err != nil {
				return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("failed to resolve operations for service %s: %v", sel.ServiceID, err)}
			}
			if len(endpoints) != len(sel.OperationNames) {
				return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("some requested operations were not found for service %s", sel.ServiceID)}
			}
			for _, ep := range endpoints {
				selections[i].EndpointIDs = append(selections[i].EndpointIDs, ep.ID)
			}
		}
	}

	desiredState, err := canonicalArtifactState(stateDoc)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to canonicalize mcp config"}
	}
	if current != nil && !sameCanonicalArtifactState(current.DesiredState, desiredState) {
		// The version identity is immutable, but source formatting and set order
		// are not part of the artifact contract and must remain idempotent.
		return sdkPlanResult{}, workspaceConfigHTTPError{
			status:  http.StatusConflict,
			message: "artifact_version_immutable: mcp version already applied with different content; bump version to change scope",
		}
	}
	payload := artifactResolvedPayload{Selections: selections, ContractBindings: bindings}
	resolvedPayload, _ := json.Marshal(payload)
	plan, err := configStore.CreateConfigPlan(ctx, store.CreateConfigPlanParams{
		ConfigKey: call.request.ConfigKey, ConfigType: store.ConfigTypeMCP,
		SourceHash: call.request.SourceHash, BaseGeneration: currentGeneration(current), Actions: []byte("[]"),
		DesiredState: desiredState, ResolvedPayload: resolvedPayload, Blockers: []byte("[]"), Warnings: []byte("[]"),
		CreatedBy: call.accountID, SupersedeExisting: true,
	})
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to save plan"}
	}
	return sdkPlanResult{plan: plan, summary: map[string]any{"create_mcp": current == nil, "services": services}, notifications: collectSDKPlanNotifications(ctx, configStore, registryClient, call, resolved)}, nil
}

// executeMCPConfigApply persists only an Engine scope and one-time execution
// token; MCP intentionally has no generated package or archive artifact.
func executeMCPConfigApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call sdkApplyCall) (mcpConfigApplyResult, error) {
	plan, err := loadArtifactPlanForApply(ctx, configStore, call, store.ConfigTypeMCP)
	if err != nil {
		return mcpConfigApplyResult{}, err
	}
	if err := ensureSDKSelectionsStillAllowed(ctx, s, plan.ResolvedPayload); err != nil {
		return mcpConfigApplyResult{}, err
	}
	bindings, err := sdkContractBindingsFromPayload(plan.ResolvedPayload)
	if err != nil {
		return mcpConfigApplyResult{}, err
	}
	if err := ensureSDKContractBindingsCurrent(ctx, registryClient, call.apiKey, bindings); err != nil {
		return mcpConfigApplyResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
	}
	doc, payload, err := decodeArtifactApplyPlan(ctx, configStore, s, plan, "mcp")
	if err != nil {
		return mcpConfigApplyResult{}, err
	}
	runtimeID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(plan.ConfigKey))
	selections, _ := json.Marshal(payload.Selections)
	token, created, err := persistArtifactScope(ctx, s, persistArtifactScopeParams{
		accountID: call.accountID, artifactID: runtimeID, bucketName: doc.Bucket,
		selections: selections, scopeSchemaVersion: models.ArtifactScopeSchemaVersion,
		kind: "mcp", name: doc.Name, version: doc.Version, configKey: plan.ConfigKey,
	})
	if err != nil {
		return mcpConfigApplyResult{}, err
	}
	if err := s.ReactivateSDK(ctx, call.accountID, runtimeID); err != nil {
		rollbackNewArtifactScope(ctx, s, call.accountID, runtimeID, created)
		return mcpConfigApplyResult{}, sdkLifecycleStoreError(err, "failed to activate mcp server")
	}
	if err := persistMCPConfigApply(ctx, configStore, call, plan, runtimeID); err != nil {
		rollbackNewArtifactScope(ctx, s, call.accountID, runtimeID, created)
		return mcpConfigApplyResult{}, err
	}
	return mcpConfigApplyResult{
		RuntimeID: runtimeID, ExecutionToken: token, ConfigKey: plan.ConfigKey,
		Name: doc.Name, Version: doc.Version, SourceHash: plan.SourceHash,
	}, nil
}

// sameCanonicalArtifactState compares persisted desired state after applying
// the same normalization used for a new plan, not the caller's raw source.
func sameCanonicalArtifactState(existing, candidate []byte) bool {
	var document sdkConfigDocument
	if json.Unmarshal(existing, &document) != nil {
		return false
	}
	canonicalExisting, err := canonicalArtifactState(document)
	return err == nil && string(canonicalExisting) == string(candidate)
}

// loadArtifactPlanForApply rejects superseded identity or generation data
// before any runtime state is changed.
func loadArtifactPlanForApply(ctx context.Context, configStore store.ConfigRepository, call sdkApplyCall, expected store.ConfigType) (*store.ConfigPlan, error) {
	plan, err := configStore.GetConfigPlan(ctx, call.planID)
	if err != nil {
		return nil, planFetchHTTPError(err)
	}
	if plan.Status != store.ConfigPlanStatusPending || plan.ConfigType != expected || call.sourceHash != plan.SourceHash {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale_or_mismatched"}
	}
	state, err := configStore.GetConfigState(ctx, plan.ConfigKey)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	if currentGeneration(state) != plan.BaseGeneration {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale"}
	}
	return plan, nil
}

// persistMCPConfigApply uses the repository's transaction boundary so desired
// state and plan completion cannot disagree after a partial database failure.
func persistMCPConfigApply(ctx context.Context, configStore store.ConfigRepository, call sdkApplyCall, plan *store.ConfigPlan, runtimeID uuid.UUID) error {
	if _, err := configStore.ApplyConfigPlan(ctx, store.ApplyConfigPlanParams{
		State: store.UpsertConfigStateParams{
			ConfigKey: plan.ConfigKey, ConfigType: store.ConfigTypeMCP,
			SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, ManagedResources: []byte("{}"),
			LatestResourceID: &runtimeID, UpdatedBy: call.accountID,
		},
		PlanID: call.planID, BaseGeneration: plan.BaseGeneration,
	}); err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply mcp config state"}
	}
	return nil
}
