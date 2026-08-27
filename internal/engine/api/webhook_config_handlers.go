package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/secretref"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// webhookConfigDocument is kind: webhook's wire shape. It remains local so
// Engine desired-config handling does not depend on the CLI package. Name is
// the registration
// identity for every service listed here -- see
// plans/plan-webhook-kind.md and store.WorkspaceWebhook.OwningConfigKey's
// doc comment for why there's no per-service label anymore.
type webhookConfigDocument struct {
	APIVersion      string                             `json:"apiVersion"`
	Kind            string                             `json:"kind"`
	Name            string                             `json:"name"`
	CallbackBaseURL string                             `json:"callback_base_url,omitempty"`
	Services        map[string]webhookConfigServiceDoc `json:"services"`
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

type webhookSecretBinding struct {
	Reference string
	BucketID  uuid.UUID
}

type webhookPlanCall struct {
	apiKey    string
	accountID uuid.UUID
	actor     accesscontrol.Actor
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
// scoped to this desired config's own config_key. verifier is only used to fetch
// each referenced service's webhook auth shape in one Registry metadata batch.
func WebhookConfigPlanHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook_config.plan")
		defer span.End()
		// Entitlement denial is still a user-triggered plan attempt on this span.
		if !entitlement.LiveEntitlement.Load().WebhookIngestionEnabled {
			slog.InfoContext(ctx, "webhook config plan denied: webhook ingestion not enabled on current plan")
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusForbidden, message: "webhook ingestion not enabled on current plan"}, "plan_admission", "", "not_committed"), ctx)
			return
		}
		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, "plan_admission", "", "not_committed"), ctx)
			return
		}
		req, doc, err := decodeWebhookConfigPlanRequest(r)
		if err != nil {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}, "plan_admission", "", "not_committed"), ctx)
			return
		}
		span.SetAttributes(attribute.String("config_key", req.ConfigKey), attribute.String("webhook.name", doc.Name))
		plan, summary, err := createWebhookConfigPlan(ctx, configStore, s, registryClient, webhookPlanCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: actor.AccountID, actor: actor,
			request: req, document: doc,
		})
		if err != nil {
			span.SetStatus(codes.Error, "webhook config plan failed")
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(err, "planning", "", "unknown"), ctx)
			return
		}
		span.SetAttributes(attribute.String("outcome", "success"), attribute.String("plan_id", plan.ID.String()))
		writeJSON(w, map[string]any{
			"plan_id": plan.ID.String(), "config_key": plan.ConfigKey,
			"owner_type":  planOwnerType(plan),
			"source_hash": plan.SourceHash, "base_generation": plan.BaseGeneration,
			"required_permissions": plan.RequiredPermissions,
			"summary":              summary,
		})
	}
}

// WebhookConfigApplyHandler reconciles fused_workspace_webhooks rows owned by
// this desired config's config_key -- creating/updating one row per declared
// service and pruning any this apply no longer declares. No runtime,
// package, or token is produced; unlike SDK/MCP there is nothing else to
// activate.
func WebhookConfigApplyHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook_config.apply")
		defer span.End()
		// Entitlement denial is still a user-triggered apply attempt on this span.
		if !entitlement.LiveEntitlement.Load().WebhookIngestionEnabled {
			slog.InfoContext(ctx, "webhook config apply denied: webhook ingestion not enabled on current plan")
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusForbidden, message: "webhook ingestion not enabled on current plan"}, "request_admission", "", "not_committed"), ctx)
			return
		}
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
		result, err := executeWebhookConfigApply(ctx, configStore, s, verifier, registryClient, sdkApplyCall{
			apiKey: r.Header.Get("X-API-Key"), accountID: actor.AccountID, actor: actor,
			planID: planID, planRevision: planRevision, sourceHash: req.SourceHash,
		})
		if err != nil {
			span.SetStatus(codes.Error, "webhook config apply failed")
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(err, "apply_execution", planID.String(), "unknown"), ctx)
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
// decodeAppConfigPlanRequest's SDK/MCP shape, so CLI/UI cannot acquire a
// different contract for this kind.
func decodeWebhookConfigPlanRequest(r *http.Request) (SDKConfigPlanRequest, webhookConfigDocument, error) {
	var req SDKConfigPlanRequest
	if err := decodeOneStrictJSON(r.Body, &req); err != nil {
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
		return req, doc, fmt.Errorf("config_key does not match webhook desired-config identity")
	}
	return req, doc, nil
}

// webhookConfigKey has no version segment, unlike SDK/MCP's
// SDK and MCP config keys -- kind: webhook is a continuously-reconciled
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
		ref, err := secretref.Parse(svcDoc.Secret)
		if err != nil || ref.Kind != secretref.KindSecret {
			return fmt.Errorf("webhook config %q service %q secret reference is invalid", doc.Name, svcName)
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
	owner, err := resolveConfigPlanOwner(ctx, s, current, call.actor, call.request.OwnerTeamSlug)
	if err != nil {
		return nil, nil, err
	}
	call.request.OwnerSubjectID, call.request.OwnerTeamID = owner.subjectID, owner.teamID
	resolved, err := resolveWebhookServices(ctx, s, registryClient, call.apiKey, call.document)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureWebhookNameAvailable(ctx, s, call.request.ConfigKey, call.document.Name, resolved); err != nil {
		return nil, nil, err
	}
	desiredState, requiredPermissions, requiredCount, err := webhookPlanPermissionSnapshot(ctx, s, current, call.document, resolved)
	if err != nil {
		return nil, nil, err
	}
	if err := preflightConfigOwnership(ctx, s, call.actor, owner, existingConfigResourceID(current), requiredPermissions); err != nil {
		return nil, nil, err
	}
	plan, err := configStore.CreateConfigPlan(ctx, store.CreateConfigPlanParams{
		ConfigKey: call.request.ConfigKey, ConfigType: store.ConfigTypeWebhook,
		OwnerSubjectID: call.request.OwnerSubjectID,
		OwnerTeamID:    call.request.OwnerTeamID,
		SourceHash:     call.request.SourceHash, BaseGeneration: currentGeneration(current), Actions: []byte("[]"),
		DesiredState: desiredState, ResolvedPayload: desiredState, Blockers: []byte("[]"), Warnings: []byte("[]"),
		RequiredPermissions: requiredPermissions,
		CreatedBy:           call.accountID, SupersedeExisting: true,
	})
	if err != nil {
		return nil, nil, configPlanSaveHTTPError(err)
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("required_permissions_count", requiredCount))
	return plan, map[string]any{"create_webhook": current == nil, "services": sortedWebhookServiceNames(resolved)}, nil
}

func webhookPlanPermissionSnapshot(ctx context.Context, s store.Store, current *store.ConfigState, document webhookConfigDocument, resolved map[string]webhookResolvedService) (json.RawMessage, json.RawMessage, int, error) {
	_, secretBuckets, err := resolveWebhookSecretBindings(ctx, s, document.Name, resolved, sortedWebhookServiceNames(resolved))
	if err != nil {
		return nil, nil, 0, err
	}
	desiredState, err := json.Marshal(document)
	if err != nil {
		return nil, nil, 0, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to canonicalize webhook config"}
	}
	serviceNames := make(map[uuid.UUID]string, len(resolved))
	for serviceName, service := range resolved {
		serviceNames[service.ServiceID] = serviceName
	}
	required, count, err := configPlanRequiredPermissionsWithBuckets(ctx, s, current, serviceNames, secretBuckets, document.Name)
	if err != nil {
		return nil, nil, 0, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to compute required permissions"}
	}
	return desiredState, required, count, nil
}

// executeWebhookConfigApply re-resolves services/versions against current
// workspace state (not just the plan's stored payload) before writing
// anything -- the same defense-in-depth ensureSDKSelectionsStillAllowed
// gives SDK/MCP applies, since a service can be deactivated or reconfigured
// between plan and apply.
func executeWebhookConfigApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, call sdkApplyCall) (webhookConfigApplyResult, error) {
	plan, doc, resolved, err := loadResolvedWebhookApply(ctx, configStore, s, registryClient, call)
	if err != nil {
		return webhookConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	names := sortedWebhookServiceNames(resolved)
	registrations, serviceIDs, err := prepareWebhookRegistrations(ctx, s, verifier, plan.ConfigKey, plan.RequiredPermissions, doc, resolved, names)
	if err != nil {
		return webhookConfigApplyResult{}, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	return commitWebhookConfigApply(ctx, configStore, call, plan, doc.Name, names, registrations, serviceIDs)
}

func loadResolvedWebhookApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call sdkApplyCall) (*store.ConfigPlan, webhookConfigDocument, map[string]webhookResolvedService, error) {
	plan, err := loadAuthorizedConfigPlanForApply(ctx, configStore, s, call, store.ConfigTypeWebhook)
	if err != nil {
		return nil, webhookConfigDocument{}, nil, err
	}
	var doc webhookConfigDocument
	if err := json.Unmarshal(plan.DesiredState, &doc); err != nil {
		return nil, webhookConfigDocument{}, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid resolved webhook plan"}
	}
	resolved, err := resolveWebhookServices(ctx, s, registryClient, call.apiKey, doc)
	if err != nil {
		return nil, webhookConfigDocument{}, nil, err
	}
	if err := ensureWebhookNameAvailable(ctx, s, plan.ConfigKey, doc.Name, resolved); err != nil {
		return nil, webhookConfigDocument{}, nil, err
	}
	return plan, doc, resolved, nil
}

func prepareWebhookRegistrations(ctx context.Context, s store.Store, verifier ServiceVerifier, configKey string, requiredPermissions json.RawMessage, doc webhookConfigDocument, resolved map[string]webhookResolvedService, names []string) ([]store.WorkspaceWebhook, []uuid.UUID, error) {
	registrations := make([]store.WorkspaceWebhook, 0, len(names))
	keepServiceIDs := make([]uuid.UUID, 0, len(names))
	secretBindings, _, err := resolveWebhookSecretBindings(ctx, s, doc.Name, resolved, names)
	if err != nil {
		return nil, nil, err
	}
	if err := validateWebhookSecretBindingPermissions(requiredPermissions, secretBindings); err != nil {
		return nil, nil, err
	}
	authShapes, err := resolveWebhookAuthShapes(ctx, s, verifier, resolved, names)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		r := resolved[name]
		keepServiceIDs = append(keepServiceIDs, r.ServiceID)
		shape := authShapes[name]
		binding := secretBindings[name]
		if err := validateSignaturePolicyBinding(shape.Auth.SignaturePolicy, binding.Reference, doc.CallbackBaseURL); err != nil {
			return nil, nil, err
		}
		var bucketID *uuid.UUID
		if binding.BucketID != uuid.Nil {
			id := binding.BucketID
			bucketID = &id
		}
		registration, err := prepareWorkspaceWebhookRegistration(r.ServiceID, r.ServiceVersionID, doc.Name, binding.Reference, bucketID, shape.Auth, shape.EventExtractionPath, configKey, doc.CallbackBaseURL)
		if err != nil {
			return nil, nil, err
		}
		registrations = append(registrations, registration)
	}
	return registrations, keepServiceIDs, nil
}

func validateSignaturePolicyBinding(policy *signaturepolicy.Config, secretRef, callbackBaseURL string) error {
	if policy == nil {
		return nil
	}
	for _, rule := range policy.Rules {
		if ref := verificationSecretRef(rule.Verification); ref != "" && ref != secretRef {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "webhook signature policy secret_ref must match the registration secret"}
		}
		if recipeUsesCallbackURL(rule.Verification.Signature) && strings.TrimSpace(callbackBaseURL) == "" {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "callback_base_url is required by the webhook signature policy"}
		}
	}
	return nil
}

func verificationSecretRef(verification signaturepolicy.Verification) string {
	if verification.Signature != nil {
		return verification.Signature.SecretRef
	}
	if verification.JWT != nil {
		return verification.JWT.SecretRef
	}
	return ""
}

func recipeUsesCallbackURL(recipe *signaturepolicy.SignatureVerification) bool {
	if recipe == nil {
		return false
	}
	for _, component := range recipe.Components {
		if component.Kind == signaturepolicy.ComponentExactCallbackURL {
			return true
		}
	}
	return false
}

func resolveWebhookSecretBindings(ctx context.Context, s store.Store, label string, resolved map[string]webhookResolvedService, names []string) (map[string]webhookSecretBinding, []store.Bucket, error) {
	refs, bucketNames, err := parseWebhookSecretRefs(label, resolved, names)
	if err != nil || len(bucketNames) == 0 {
		return webhookBindingsWithoutBuckets(refs), nil, err
	}
	buckets, err := s.GetBucketsByNames(ctx, bucketNames)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to resolve webhook secret buckets"}
	}
	available := make(map[string]store.Bucket, len(buckets))
	for _, bucket := range buckets {
		available[bucket.Name] = bucket
	}
	bindings := make(map[string]webhookSecretBinding, len(refs))
	for _, name := range names {
		ref := refs[name]
		if ref == "" {
			continue
		}
		parsed, _ := secretref.Parse(ref)
		bucket, ok := available[parsed.Bucket]
		if !ok || bucket.ID == uuid.Nil {
			return nil, nil, webhookSecretBucketError(label, parsed.Bucket)
		}
		bindings[name] = webhookSecretBinding{Reference: ref, BucketID: bucket.ID}
	}
	return bindings, buckets, nil
}

func webhookBindingsWithoutBuckets(refs map[string]string) map[string]webhookSecretBinding {
	bindings := make(map[string]webhookSecretBinding, len(refs))
	for name, ref := range refs {
		bindings[name] = webhookSecretBinding{Reference: ref}
	}
	return bindings
}

func validateWebhookSecretBindingPermissions(raw json.RawMessage, bindings map[string]webhookSecretBinding) error {
	requirements, err := accesscontrol.UnmarshalRequiredPermissions(raw)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook plan permission snapshot is invalid"}
	}
	expected := webhookRequiredBucketIDs(requirements)
	actual := make(map[uuid.UUID]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.BucketID != uuid.Nil {
			actual[binding.BucketID] = struct{}{}
		}
	}
	if !sameWebhookBucketIDs(expected, actual) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook secret bucket binding changed; create a new plan"}
	}
	return nil
}

func webhookRequiredBucketIDs(requirements []accesscontrol.Requirement) map[uuid.UUID]struct{} {
	ids := make(map[uuid.UUID]struct{})
	for _, requirement := range requirements {
		if requirement.Permission == accesscontrol.PermissionBucketUse && requirement.Resource.Type == accesscontrol.ResourceBucket {
			ids[requirement.Resource.ID] = struct{}{}
		}
	}
	return ids
}

func sameWebhookBucketIDs(first, second map[uuid.UUID]struct{}) bool {
	if len(first) != len(second) {
		return false
	}
	for id := range first {
		if _, ok := second[id]; !ok {
			return false
		}
	}
	return true
}

func webhookSecretBucketError(label, bucketName string) error {
	return fmt.Errorf("webhook %q: %w", label, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bucket not found: " + bucketName})
}

func parseWebhookSecretRefs(label string, resolved map[string]webhookResolvedService, names []string) (map[string]string, []string, error) {
	refs := make(map[string]string, len(names))
	bucketSet := make(map[string]struct{}, len(names))
	for _, name := range names {
		raw := resolved[name].Secret
		if strings.TrimSpace(raw) == "" {
			continue
		}
		ref, err := secretref.Parse(raw)
		if err != nil || ref.Kind != secretref.KindSecret {
			return nil, nil, fmt.Errorf("webhook %q: %w", label, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "webhook secret reference is invalid"})
		}
		refs[name] = ref.String()
		bucketSet[ref.Bucket] = struct{}{}
	}
	bucketNames := make([]string, 0, len(bucketSet))
	for name := range bucketSet {
		bucketNames = append(bucketNames, name)
	}
	sort.Strings(bucketNames)
	return refs, bucketNames, nil
}

type webhookAuthShape struct {
	Auth                fusedobject.IncomingWebhookConfig
	EventExtractionPath string
}

type webhookMetadataBatchVerifier interface {
	FetchServiceMetadataBatch(context.Context, []sandbox.ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error)
}

func resolveWebhookAuthShapes(ctx context.Context, s store.Store, verifier ServiceVerifier, resolved map[string]webhookResolvedService, names []string) (map[string]webhookAuthShape, error) {
	refs := make([]sandbox.ServiceMetadataRef, 0, len(names))
	policyRefs := make([]store.WorkspaceExecutionPolicyRef, 0, len(names))
	for _, name := range names {
		service := resolved[name]
		refs = append(refs, sandbox.ServiceMetadataRef{ServiceID: service.ServiceID, Version: service.Version})
		policyRefs = append(policyRefs, store.WorkspaceExecutionPolicyRef{ServiceID: service.ServiceID, ServiceVersionID: service.ServiceVersionID})
	}
	metadata, err := fetchWebhookMetadata(ctx, verifier, refs)
	if err != nil {
		return nil, err
	}
	batchStore, ok := s.(store.WorkspaceExecutionPolicyBatchStore)
	if !ok {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to resolve webhook execution policy"}
	}
	overrides, err := batchStore.GetEffectiveWorkspaceExecutionPolicyOverrides(ctx, policyRefs)
	if err != nil {
		slog.ErrorContext(ctx, "workspace execution policy override batch lookup failed", slog.Any("error", err))
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to resolve webhook execution policy"}
	}
	shapes := make(map[string]webhookAuthShape, len(names))
	for i, name := range names {
		shape := webhookAuthShape{}
		serviceMetadata := metadata[sandbox.ServiceMetadataRefKey(refs[i])]
		if serviceMetadata == nil {
			return nil, fmt.Errorf("fetch webhook auth shape for service %s: metadata missing", refs[i].ServiceID)
		}
		shape.EventExtractionPath = serviceMetadata.EventExtractionPath
		if serviceMetadata.IncomingWebhookConfig != nil {
			shape.Auth = *serviceMetadata.IncomingWebhookConfig
		}
		applyWebhookPolicyOverride(&shape, overrides[policyRefs[i]])
		shapes[name] = shape
	}
	return shapes, nil
}

func fetchWebhookMetadata(ctx context.Context, verifier ServiceVerifier, refs []sandbox.ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	if batch, ok := verifier.(webhookMetadataBatchVerifier); ok {
		metadata, err := batch.FetchServiceMetadataBatch(ctx, refs)
		if err != nil {
			return nil, fmt.Errorf("fetch webhook auth shapes: %w", err)
		}
		return metadata, nil
	}
	if len(refs) != 1 {
		return nil, errors.New("webhook metadata batch lookup is unavailable")
	}
	metadata, err := verifier.FetchServiceMetadata(ctx, refs[0].ServiceID, refs[0].Version)
	if err != nil {
		return nil, fmt.Errorf("fetch webhook auth shape for service %s: %w", refs[0].ServiceID, err)
	}
	return map[string]*fusedobject.ServiceMetadata{sandbox.ServiceMetadataRefKey(refs[0]): metadata}, nil
}

func applyWebhookPolicyOverride(shape *webhookAuthShape, override *store.WorkspaceExecutionPolicyOverride) {
	if override == nil {
		return
	}
	if override.EventExtractionPath != nil {
		shape.EventExtractionPath = *override.EventExtractionPath
	}
	if override.IncomingWebhookConfig != nil {
		shape.Auth = *override.IncomingWebhookConfig
	}
}

func commitWebhookConfigApply(ctx context.Context, configStore store.ConfigRepository, call sdkApplyCall, plan *store.ConfigPlan, name string, names []string, registrations []store.WorkspaceWebhook, keepServiceIDs []uuid.UUID) (webhookConfigApplyResult, error) {
	result, err := configStore.ApplyWebhookConfigPlan(ctx, store.ApplyWebhookConfigPlanParams{
		Plan: store.ApplyConfigPlanParams{
			State: store.UpsertConfigStateParams{
				ConfigKey: plan.ConfigKey, ConfigType: store.ConfigTypeWebhook,
				OwnerSubjectID: plan.OwnerSubjectID,
				OwnerTeamID:    plan.OwnerTeamID,
				SourceHash:     plan.SourceHash, DesiredState: plan.DesiredState, ManagedResources: []byte("{}"),
				UpdatedBy: call.accountID,
			},
			PlanID: call.planID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: call.planRevision,
		},
		Registrations: registrations, KeepServiceIDs: keepServiceIDs,
	})
	if err != nil {
		// Typed repository conflicts reject the atomic transaction before commit.
		if errors.Is(err, store.ErrConfigOwnerInactive) || errors.Is(err, store.ErrConfigPlanRevisionMismatch) || errors.Is(err, store.ErrConfigPlanNotFound) || errors.Is(err, store.ErrWorkspaceWebhookOwnerConflict) || errors.Is(err, store.ErrAppOwnerMismatch) || errors.Is(err, store.ErrConfigStateIdentityMismatch) {
			return webhookConfigApplyResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook plan is stale or its owner is inactive", phase: "workspace_commit", operationID: call.planID.String(), commitState: "not_committed"}
		}
		// An unclassified commit-boundary failure cannot prove the transaction outcome.
		return webhookConfigApplyResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to atomically apply webhook config", phase: "workspace_commit", operationID: call.planID.String(), commitState: "unknown"}
	}
	// A complete database result proves commit before response projection checks.
	if len(result.Registrations) != len(names) {
		return webhookConfigApplyResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "webhook apply returned an incomplete result", phase: "response_projection", operationID: call.planID.String(), commitState: "committed"}
	}
	applied := make([]appliedWorkspaceWebhook, 0, len(result.Registrations))
	for i, saved := range result.Registrations {
		emitWebhookAppliedSpan(ctx, saved)
		applied = append(applied, appliedWorkspaceWebhook{ServiceKey: names[i], Label: name, Slug: saved.Slug})
	}
	return webhookConfigApplyResult{ConfigKey: plan.ConfigKey, Name: name, Applied: applied}, nil
}

// unresolvedWebhookServiceKeys checks the caller's already-batched activation
// map so resolving a document never adds one query per service.
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
// (ListWorkspaceServices, ListWorkspaceServiceVersionsForServices) rather than
// one query per service. Plan and apply share it so they cannot resolve a
// service differently.
func resolveWebhookServices(ctx context.Context, s store.Store, registryClient sandbox.RegistryClient, apiKey string, doc webhookConfigDocument) (map[string]webhookResolvedService, error) {
	keys := sortedWebhookDocumentServiceKeys(doc)
	workspaceServices, err := s.ListWorkspaceServices(ctx, keys)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list workspace services"}
	}
	services := workspaceWebhookServicesByKey(workspaceServices, keys)
	if err := addResolvedWebhookSlugs(ctx, registryClient, apiKey, doc, workspaceServices, services); err != nil {
		return nil, err
	}
	serviceIDs, err := validateWebhookServiceIdentities(keys, services)
	if err != nil {
		return nil, err
	}
	allowedVersions, err := s.ListWorkspaceServiceVersionsForServices(ctx, serviceIDs)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list allowed versions"}
	}
	return buildResolvedWebhookServices(doc, keys, services, allowedVersions)
}

func validateWebhookServiceIdentities(keys []string, services map[string]store.WorkspaceService) ([]uuid.UUID, error) {
	serviceIDs := make([]uuid.UUID, 0, len(keys))
	seenServiceIDs := make(map[uuid.UUID]string, len(keys))
	for _, name := range keys {
		activation, ok := services[name]
		if !ok {
			return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("service %s is not activated in this workspace. Run 'fused-cli workspace service add %s' to activate it.", name, name)}
		}
		if previousKey, duplicate := seenServiceIDs[activation.ServiceID]; duplicate {
			return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("services %s and %s resolve to the same activated service", previousKey, name)}
		}
		seenServiceIDs[activation.ServiceID] = name
		serviceIDs = append(serviceIDs, activation.ServiceID)
	}
	return serviceIDs, nil
}

// buildResolvedWebhookServices pins each inbound binding to an already-enabled local service version.
func buildResolvedWebhookServices(doc webhookConfigDocument, keys []string, services map[string]store.WorkspaceService, allowedVersions map[uuid.UUID][]store.WorkspaceServiceVersion) (map[string]webhookResolvedService, error) {
	resolved := make(map[string]webhookResolvedService, len(doc.Services))
	for _, name := range keys {
		svcDoc := doc.Services[name]
		activation := services[name]
		// kind: webhook has no per-service version field (unlike SDK/MCP) --
		// an empty version resolves to allowedVersions[0], the same
		// "register against the first/default enabled version" contract.
		version, err := resolveSDKVersionAllowed("", name, allowedVersions[activation.ServiceID])
		// Missing enabled versions cannot be repaired by creating a partial inbound binding.
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

// addResolvedWebhookSlugs reuses canonical identity resolution while retaining local activation as the admission boundary.
func addResolvedWebhookSlugs(ctx context.Context, registryClient sandbox.RegistryClient, apiKey string, doc webhookConfigDocument, workspaceServices []store.WorkspaceService, services map[string]store.WorkspaceService) error {
	missing := unresolvedWebhookServiceKeys(doc, services)
	resolver, ok := registryClient.(ServiceSlugResolver)
	// Resolved names need no remote work; narrower clients cannot expand their existing activation map.
	if len(missing) == 0 || !ok || resolver == nil {
		return nil
	}
	resolved, err := resolver.ResolveServiceIDsBySlugs(ctx, missing, apiKey)
	// Lookup failures must not be interpreted as absent or differently owned services.
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to resolve service slugs"}
	}
	byID := workspaceServicesByID(workspaceServices)
	for _, slug := range missing {
		// A Registry identity is selectable only when the same service is already activated locally.
		if activation, exists := byID[resolved[slug]]; exists {
			services[slug] = activation
		}
	}
	return nil
}

func sortedWebhookDocumentServiceKeys(doc webhookConfigDocument) []string {
	keys := make([]string, 0, len(doc.Services))
	for key := range doc.Services {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func workspaceWebhookServicesByKey(activations []store.WorkspaceService, keys []string) map[string]store.WorkspaceService {
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	resolved := make(map[string]store.WorkspaceService, len(keys))
	for _, activation := range activations {
		if _, ok := wanted[activation.ServiceName]; ok {
			resolved[activation.ServiceName] = activation
		}
		if _, ok := wanted[activation.ServiceSlug]; ok {
			resolved[activation.ServiceSlug] = activation
		}
	}
	return resolved
}

// ensureWebhookNameAvailable is the (service, name) uniqueness check
// (plans/plan-webhook-kind.md): one batched WorkspaceWebhookOwnersByLabel
// call across every resolved service, not one query per service, then a
// pure in-memory check of who (if anyone) already owns that pair. An owner
// different from configKey means another webhook desired config got there first.
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
		if owner != configKey {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf(
				"service %s webhook %q is already registered by another webhook config (%s)", svcName, name, owner,
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
