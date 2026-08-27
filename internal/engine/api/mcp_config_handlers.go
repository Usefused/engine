package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type mcpConfigApplyResult struct {
	AppFamilyID    uuid.UUID
	RuntimeID      uuid.UUID
	ExecutionToken string
	ConfigKey      string
	Name           string
	Version        string
	SourceHash     string
	Scope          store.AppRuntime
}

// MCPConfigPlanHandler owns desired-state validation for Engine-projected MCP
// runtimes. It deliberately shares SDK selection and contract resolution, but
// stores an MCP plan so apply can never cross into code generation.
func MCPConfigPlanHandler(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.mcp_config.plan")
		defer span.End()
		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, "plan_admission", "", "not_committed"), ctx)
			return
		}
		req, doc, err := decodeAppConfigPlanRequest(r, store.AppKindMCP.String())
		if err != nil {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}, "plan_admission", "", "not_committed"), ctx)
			return
		}
		setSDKConfigSpanAttributes(span, req.ConfigKey, doc)
		result, err := createMCPConfigPlan(ctx, configStore, s, registryClient, sdkPlanCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: actor.AccountID, actor: actor,
			request: req, document: doc,
		})
		if err != nil {
			span.SetStatus(codes.Error, "mcp config plan failed")
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(err, "planning", "", "unknown"), ctx)
			return
		}
		span.SetAttributes(attribute.String("outcome", "success"), attribute.String("plan_id", result.plan.ID.String()))
		writeJSON(w, map[string]any{
			"plan_id": result.plan.ID.String(), "config_key": result.plan.ConfigKey,
			"owner_type":  planOwnerType(result.plan),
			"source_hash": result.plan.SourceHash, "base_generation": result.plan.BaseGeneration,
			"required_permissions": result.plan.RequiredPermissions,
			"summary":              result.summary, "notifications": result.notifications,
		})
	}
}

// MCPConfigApplyHandler activates a resolved Engine scope directly. No
// Registry SDK generation request or package record exists on this path.
func MCPConfigApplyHandler(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.mcp_config.apply")
		defer span.End()
		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, "request_admission", "", "not_committed"), ctx)
			return
		}
		req, planID, err := decodeSDKConfigApplyRequest(r)
		if err != nil {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}, "request_admission", "", "not_committed"), ctx)
			return
		}
		planRevision, ok := AuthorizedPlanRevisionFromContext(ctx)
		if !ok {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusForbidden, message: "authorized plan revision unavailable"}, "apply_admission", planID.String(), "not_committed"), ctx)
			return
		}
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
		result, err := executeMCPConfigApply(ctx, configStore, s, registryClient, sdkApplyCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: actor.AccountID, actor: actor,
			planID: planID, planRevision: planRevision, sourceHash: req.SourceHash,
		})
		if err != nil {
			span.SetStatus(codes.Error, "mcp config apply failed")
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(err, "apply_execution", planID.String(), "unknown"), ctx)
			return
		}
		span.SetAttributes(
			attribute.String("outcome", "success"), attribute.String("app.kind", store.AppKindMCP.String()),
			attribute.String("app.version", result.Version),
			attribute.String("app.family_id", result.AppFamilyID.String()),
			attribute.String("app.id", result.RuntimeID.String()),
		)
		if result.ExecutionToken != "" {
			setOneTimeSecretResponseHeaders(w)
		}
		writeJSON(w, mcpConfigApplyResponse(planID, result, mcpTransportURLsForApp(r, result.RuntimeID)))
	}
}

// mcpConfigApplyResponse keeps MCP's one-time token on the shared app
// wire key so fused-cli can use one decoder for SDK and MCP apply responses.
func mcpConfigApplyResponse(planID uuid.UUID, result mcpConfigApplyResult, transportURLs mcpTransportURLs) map[string]any {
	resp := map[string]any{
		"status": "applied", "plan_id": planID.String(), "config_key": result.ConfigKey,
		"name": result.Name, "version": result.Version,
		"app_family_id": result.AppFamilyID.String(), "app_id": result.RuntimeID.String(),
		"default_transport": mcpDefaultTransport, "transport_urls": transportURLs,
	}
	if result.ExecutionToken != "" {
		// Token is only present on first creation. Absent on idempotent re-apply.
		resp["execution_token"] = result.ExecutionToken
		resp["token_note"] = "reusable until rotated; store securely"
	}
	return resp
}

// decodeAppConfigPlanRequest applies one strict wire decoder to SDK and
// MCP documents so UI, CLI, and GraphQL cannot acquire different contracts.
func decodeAppConfigPlanRequest(r *http.Request, kind string) (SDKConfigPlanRequest, sdkConfigDocument, error) {
	var req SDKConfigPlanRequest
	if err := decodeOneStrictJSON(r.Body, &req); err != nil {
		return req, sdkConfigDocument{}, errors.New("invalid request body")
	}
	if strings.TrimSpace(req.SourceHash) == "" || strings.TrimSpace(req.ConfigKey) == "" {
		return req, sdkConfigDocument{}, errors.New("source_hash and config_key are required")
	}
	if err := rejectRemovedSDKConfigFields(req.Config); err != nil {
		return req, sdkConfigDocument{}, err
	}
	var doc sdkConfigDocument
	if err := decodeAppConfigJSON(req.Config, &doc); err != nil {
		return req, doc, errors.New("invalid config json")
	}
	if err := validateAppConfigDocument(doc, kind); err != nil {
		return req, doc, err
	}
	if req.ConfigKey != fmt.Sprintf("%s:%s:%s", kind, doc.Name, doc.Version) {
		return req, doc, fmt.Errorf("config_key does not match %s app identity", kind)
	}
	return req, doc, nil
}

// validateAppConfigDocument enforces the shared app identity while
// keeping generation-only fields out of an Engine-projected MCP runtime.
func validateAppConfigDocument(doc sdkConfigDocument, kind string) error {
	if doc.APIVersion != "fused/v1" || doc.Kind != kind {
		return fmt.Errorf("config must use apiVersion fused/v1 and kind %s", kind)
	}
	if strings.TrimSpace(doc.Name) == "" || !validAppVersion(doc.Version) {
		return fmt.Errorf("%s config requires name and a SemVer-compatible version", kind)
	}
	if strings.TrimSpace(doc.Bucket) == "" {
		return fmt.Errorf("%s config requires exactly one bucket", kind)
	}
	if err := validateMCPAppRestrictions(doc, kind); err != nil {
		return err
	}
	// Service selection must be valid before bindings can rely on those exact
	// configured keys and operation allowlists.
	if err := validateAppServiceDocs(doc.Services); err != nil {
		return err
	}
	// MCP shares the SDK graph contract but has no generated language symbols,
	// so only the code-generation checks are disabled at this boundary.
	return validateAppUnifiedOperations(doc, false)
}

// validateMCPAppRestrictions rejects malformed mcp app restrictions before it can cross the Unified operation boundary.
func validateMCPAppRestrictions(doc sdkConfigDocument, kind string) error {
	if kind != store.AppKindMCP.String() {
		return nil
	}
	if strings.TrimSpace(doc.Language) != "" {
		return errors.New("mcp config must not set language")
	}
	for name, service := range doc.Services {
		// MCP is operation-only; webhook attachment belongs to SDK apps.
		if len(service.Webhooks) > 0 || service.WebhooksSelectAll {
			return fmt.Errorf("mcp service %s cannot select webhooks", name)
		}
	}
	return nil
}

// createMCPConfigPlan resolves service versions, operations, and auth policy
// now so apply and later agent calls never need to infer provider setup.
func createMCPConfigPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call sdkPlanCall) (sdkPlanResult, error) {
	current, registryClient, err := loadMCPPlanningState(ctx, configStore, s, registryClient, call.request.ConfigKey)
	// State and local snapshot authority must be available before another immutable version is planned.
	if err != nil {
		return sdkPlanResult{}, err
	}
	owner, bucket, err := resolveAppPlanOwnerAndBucket(
		ctx, s, current, call.actor, call.request.OwnerTeamSlug, call.document.Bucket,
	)
	// Retained provider contracts do not bypass owner or credential-set authorization.
	if err != nil {
		return sdkPlanResult{}, err
	}
	call.request.OwnerSubjectID, call.request.OwnerTeamID = owner.subjectID, owner.teamID
	selections, services, resolved, stateDoc, err := resolveSDKSelections(ctx, configStore, s, registryClient, call.apiKey, call.document, previousSDKDocument(current), *bucket)
	// MCP shares SDK selection/auth decisions rather than implementing an alternate planner.
	if err != nil {
		return sdkPlanResult{}, err
	}
	bindings, err := resolveSDKContractBindings(ctx, registryClient, call.apiKey, resolved)
	// Local snapshot identity fences MCP refreshes without requiring a generated-package pin.
	if err != nil {
		return sdkPlanResult{}, generationPinPlanError(err, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to bind service contract revisions"})
	}
	selections = finalizeAppSelections(selections, bindings)

	selections, unifiedCompilation, err := resolveAndCompileMCPUnifiedOperations(ctx, s, call.document, selections, resolved)
	// A partially frozen or compiled graph must never enter an immutable plan.
	if err != nil {
		return sdkPlanResult{}, err
	}

	desiredState, err := validateMCPDesiredState(stateDoc, current)
	// A changed declaration cannot overwrite an immutable published MCP version.
	if err != nil {
		return sdkPlanResult{}, err
	}
	payload := appResolvedPayload{
		Selections: selections, ContractBindings: bindings, BucketID: bucket.ID,
		UnifiedDefinitionSchemaVersion: unified.DefinitionSchemaVersion,
		UnifiedDefinitions:             unifiedCompilation.DefinitionJSON,
		UnifiedDefinitionHash:          unifiedCompilation.DefinitionHash,
		UnifiedCodegenDescriptorHash:   unifiedCompilation.CodegenDescriptorHash,
		UnifiedOperations:              unifiedCompilation.Descriptors,
	}
	resolvedPayload, _ := json.Marshal(payload)
	requiredPermissions, requiredCount, err := configPlanRequiredPermissionsWithBuckets(
		ctx, s, current, serviceNamesFromResolved(resolved), []store.Bucket{*bucket}, call.document.Name,
	)
	// Required permissions remain attached to the plan regardless of contract storage location.
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to compute required permissions"}
	}
	// Plan creation must not expose a family the actor cannot manage.
	if err := preflightConfigOwnership(ctx, s, call.actor, owner, existingConfigResourceID(current), requiredPermissions); err != nil {
		return sdkPlanResult{}, err
	}
	plan, err := configStore.CreateConfigPlan(ctx, store.CreateConfigPlanParams{
		ConfigKey: call.request.ConfigKey, ConfigType: store.ConfigTypeMCP,
		OwnerSubjectID: call.request.OwnerSubjectID,
		OwnerTeamID:    call.request.OwnerTeamID,
		SourceHash:     call.request.SourceHash, BaseGeneration: currentGeneration(current), Actions: []byte("[]"),
		DesiredState: desiredState, ResolvedPayload: resolvedPayload, Blockers: []byte("[]"), Warnings: []byte("[]"),
		RequiredPermissions: requiredPermissions,
		CreatedBy:           call.accountID, SupersedeExisting: true,
	})
	// Success means the complete plan and its exact pins are durable together.
	if err != nil {
		return sdkPlanResult{}, configPlanSaveHTTPError(err)
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("required_permissions_count", requiredCount))
	return sdkPlanResult{plan: plan, summary: map[string]any{"create_mcp": current == nil, "services": services}, notifications: collectSDKPlanNotifications(ctx, configStore, registryClient, call, resolved)}, nil
}

// loadMCPPlanningState admits local-only dependencies before inspecting the existing immutable configuration.
func loadMCPPlanningState(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, configKey string) (*store.ConfigState, sandbox.RegistryClient, error) {
	client, err := localSnapshotPlanningClient(s, registryClient, false)
	// Missing snapshot support cannot fall back to Registry even when no prior MCP state exists.
	if err != nil {
		return nil, nil, err
	}
	current, err := configStore.GetConfigState(ctx, configKey)
	// Store failure is distinct from a legitimately new immutable version.
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	return current, client, nil
}

// resolveAndCompileMCPUnifiedOperations freezes physical selections before invoking the unchanged SDK Unified compiler.
func resolveAndCompileMCPUnifiedOperations(ctx context.Context, s store.Store, doc sdkConfigDocument, selections []models.SDKSelection, services []sdkResolvedService) ([]models.SDKSelection, sdkUnifiedCompilation, error) {
	resolved, err := resolveMCPEndpointIDs(ctx, s, selections)
	// Compilation requires endpoint IDs from the exact local contract snapshot.
	if err != nil {
		return nil, sdkUnifiedCompilation{}, err
	}
	compiled, err := compileSDKUnifiedOperations(ctx, s, doc, resolved, services)
	// The shared compiler is the sole admission boundary for executable graph bytes.
	if err != nil {
		return nil, sdkUnifiedCompilation{}, err
	}
	return resolved, compiled, nil
}

func validateMCPDesiredState(state sdkConfigDocument, current *store.ConfigState) ([]byte, error) {
	desiredState, err := canonicalAppState(state)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to canonicalize mcp config"}
	}
	// The version identity is immutable, but source formatting and set order
	// are not part of the app contract and must remain idempotent.
	if current != nil && !sameCanonicalAppState(current.DesiredState, desiredState) {
		return nil, workspaceConfigHTTPError{
			status:  http.StatusConflict,
			message: "app_version_immutable: mcp version already applied with different content; bump version to change scope",
		}
	}
	return desiredState, nil
}

// resolveMCPEndpointIDs freezes explicit MCP operation names to immutable endpoint IDs through one Engine snapshot query.
func resolveMCPEndpointIDs(ctx context.Context, s store.Store, selections []models.SDKSelection) ([]models.SDKSelection, error) {
	requests := mcpEndpointResolutionRequests(selections)
	// An empty unresolved set is already immutable and should not issue a
	// database query solely to prove that no work exists.
	if len(requests) == 0 {
		return selections, nil
	}
	contractStore, ok := s.(sdkUnifiedContractStore)
	// Explicit operation names cannot be frozen safely without the set-based
	// snapshot resolver; falling back to Registry would reintroduce N+1 reads.
	if !ok {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "Engine contract snapshot resolver is unavailable"}
	}
	matches, err := contractStore.ListServiceContractEndpointsForSelections(ctx, requests, nil)
	// Snapshot lookup failures stop planning before any immutable payload exists.
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to resolve MCP operations from the Engine contract snapshot"}
	}
	return applyResolvedMCPEndpointIDs(selections, requests, matches)
}

// mcpEndpointResolutionRequests projects every unresolved explicit selection into one set-based snapshot request.
func mcpEndpointResolutionRequests(selections []models.SDKSelection) []store.ServiceContractEndpointSelection {
	requests := make([]store.ServiceContractEndpointSelection, 0, len(selections))
	for index, selection := range selections {
		// Select-all and already-frozen selections need no name-to-ID work, which
		// keeps the single query bounded to unresolved explicit operation sets.
		if len(selection.OperationNames) == 0 || len(selection.EndpointIDs) > 0 {
			continue
		}
		requests = append(requests, store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
			OperationNames: selection.OperationNames, EndpointNames: selection.OperationNames,
		})
	}
	return requests
}

// applyResolvedMCPEndpointIDs aligns database-filtered snapshot rows with their immutable app selections.
func applyResolvedMCPEndpointIDs(selections []models.SDKSelection, requests []store.ServiceContractEndpointSelection, matches []store.ServiceContractEndpointMatch) ([]models.SDKSelection, error) {
	resolvedCounts := make(map[int]int, len(requests))
	for _, match := range matches {
		// The resolver has already applied exact service/version/name predicates;
		// this loop only projects its bounded rows back onto their source selection.
		if match.SelectionIndex < 0 || match.SelectionIndex >= len(selections) {
			return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "MCP operation resolution returned an invalid selection"}
		}
		selections[match.SelectionIndex].EndpointIDs = append(selections[match.SelectionIndex].EndpointIDs, match.Endpoint.ID)
		resolvedCounts[match.SelectionIndex]++
	}
	for _, request := range requests {
		// Every authored operation must freeze to exactly one snapshot endpoint;
		// partial matches would otherwise publish a narrower app silently.
		if resolvedCounts[request.SelectionIndex] != len(request.OperationNames) {
			return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("some requested operations were not found for service %s", request.ServiceID)}
		}
	}
	return selections, nil
}

// executeMCPConfigApply persists only an Engine scope and one-time execution
// token; MCP intentionally has no generated package or archive package.
func executeMCPConfigApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call sdkApplyCall) (mcpConfigApplyResult, error) {
	registryClient, err := localSnapshotPlanningClient(s, registryClient, false)
	// An unavailable snapshot store must fail before any app mutation or live lookup.
	if err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	plan, err := loadAuthorizedConfigPlanForApply(ctx, configStore, s, call, store.ConfigTypeMCP)
	// Only the authorized exact plan may establish execution scope.
	if err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	// Archived contracts cannot reactivate a version removed from this workspace.
	if err := ensureSDKSelectionsStillAllowed(ctx, s, plan.ResolvedPayload); err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	// Refreshes between plan and apply must invalidate the old immutable binding.
	if err := ensureAppPayloadContractsCurrent(ctx, registryClient, call.apiKey, plan.ResolvedPayload); err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	doc, payload, err := decodeAppApplyPlan(ctx, configStore, s, plan, store.AppKindMCP.String())
	// MCP-specific admission cannot be borrowed from an SDK payload.
	if err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	// Apply rechecks canonical bytes and both hashes so a tampered plan cannot
	// become executable even though planning already validated the graph.
	if err := normalizeAndValidateResolvedUnifiedPayload(&payload); err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	runtimeID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(plan.ConfigKey))
	scope, err := appRuntimeForApply(persistAppRuntimeParams{
		accountID: call.accountID, appID: runtimeID, ownerSubjectID: planOwnerSubjectID(plan), ownerTeamID: planOwnerTeamID(plan), bucketID: payload.BucketID, bucketName: doc.Bucket,
		selections: payload.Selections, scopeSchemaVersion: models.AppScopeSchemaVersion,
		kind: store.AppKindMCP, name: doc.Name, version: doc.Version, configKey: plan.ConfigKey,
		unifiedDefinitionSchemaVersion: payload.UnifiedDefinitionSchemaVersion,
		unifiedDefinitions:             payload.UnifiedDefinitions,
		unifiedDefinitionHash:          payload.UnifiedDefinitionHash,
		unifiedCodegenDescriptorHash:   payload.UnifiedCodegenDescriptorHash,
	})
	// Runtime scope must be complete before publishing any family version.
	if err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	// Retention changes no entitlement or family-count policy.
	if err := enforceMCPFamilyLimit(ctx, s, call.accountID, doc.Name); err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}

	token, familyID, appID, _, err := applyAppConfigRuntime(ctx, configStore, s, call, plan, scope, doc.Bucket, "", "")
	// A token is returned only after the canonical lifecycle transaction commits.
	if err != nil {
		return mcpConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "workspace_commit", call.planID.String(), "unknown")
	}
	return mcpConfigApplyResult{
		AppFamilyID: familyID, RuntimeID: appID, ExecutionToken: token, ConfigKey: plan.ConfigKey,
		Name: doc.Name, Version: doc.Version, SourceHash: plan.SourceHash, Scope: scope,
	}, nil
}

func enforceMCPFamilyLimit(ctx context.Context, s store.Store, accountID uuid.UUID, name string) error {
	canonicalName, _, err := canonical.AppName(name)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	_, err = s.GetAppFamilyByIdentity(ctx, accountID, store.AppKindMCP.String(), canonicalName)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrAppFamilyNotFound) {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed_to_resolve_mcp_family"}
	}
	currentFamilies, err := s.CountAppFamilies(ctx, accountID, store.AppKindMCP.String())
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed_to_count_mcp_families"}
	}
	if limitErr := entitlement.CheckLimit(trace.SpanFromContext(ctx), "mcp_families", currentFamilies, entitlement.LiveEntitlement.Load().MaxMCPFamilies); limitErr != nil {
		return workspaceConfigHTTPError{status: http.StatusForbidden, message: limitErr.Error()}
	}
	return nil
}

// sameCanonicalAppState compares persisted desired state after applying
// the same normalization used for a new plan, not the caller's raw source.
func sameCanonicalAppState(existing, candidate []byte) bool {
	var document sdkConfigDocument
	if json.Unmarshal(existing, &document) != nil {
		return false
	}
	canonicalExisting, err := canonicalAppState(document)
	return err == nil && string(canonicalExisting) == string(candidate)
}

// loadConfigPlanForApply rejects superseded identity or generation data
// before any runtime state is changed.
func loadConfigPlanForApply(ctx context.Context, configStore store.ConfigRepository, call sdkApplyCall, expected store.ConfigType) (*store.ConfigPlan, *store.ConfigState, error) {
	plan, err := configStore.GetConfigPlan(ctx, call.planID)
	if err != nil {
		return nil, nil, planFetchHTTPError(err)
	}
	if plan.Status != store.ConfigPlanStatusPending || plan.ConfigType != expected || call.sourceHash != plan.SourceHash {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale_or_mismatched"}
	}
	if call.planRevision <= 0 || plan.Revision != call.planRevision {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_revision_changed"}
	}
	state, err := configStore.GetConfigState(ctx, plan.ConfigKey)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	if currentGeneration(state) != plan.BaseGeneration {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale"}
	}
	return plan, state, nil
}
