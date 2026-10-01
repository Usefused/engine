package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
)

// WebhookSigningSecretPlanHandler patches one reference in the complete stored bundle, then reuses normal plan/apply authorization.
func WebhookSigningSecretPlanHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient) http.HandlerFunc {
	// Only a review receipt is created here; the existing apply endpoint owns the actual mutation.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook_config.signing_secret.plan")
		defer span.End()
		actor, ok := accesscontrol.ActorFromContext(ctx)
		// Enforce management even when this handler is mounted without the control router.
		if !ok {
			writeSDKConfigError(w, accesscontrol.ErrAuthenticationRequired, ctx)
			return
		}
		requirement := accesscontrol.Requirement{Permission: accesscontrol.PermissionAppWebhookManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}
		if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, requirement); err != nil {
			writeSDKConfigError(w, err, ctx)
			return
		}
		// Editing ingress still requires an enabled webhook entitlement.
		if !entitlement.LiveEntitlement.Load().WebhookIngestionEnabled {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusForbidden, message: "webhook ingestion not enabled on current plan"}, ctx)
			return
		}
		var req struct {
			Slug   string `json:"slug"`
			Secret string `json:"secret"`
		}
		// A replacement is mandatory; this action cannot silently turn off verification.
		if err := decodeOneStrictJSON(http.MaxBytesReader(w, r.Body, 16<<10), &req); err != nil || strings.TrimSpace(req.Slug) == "" || strings.TrimSpace(req.Secret) == "" {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "webhook slug and signing secret reference are required"}, ctx)
			return
		}
		plan, err := planWebhookSigningSecret(ctx, configStore, s, verifier, registryClient, actor, r.Header.Get("X-API-Key"), req.Slug, strings.TrimSpace(req.Secret))
		// No configuration or secret reference is returned on failure.
		if err != nil {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(err, "plan_admission", "", "not_committed"), ctx)
			return
		}
		writeJSON(w, map[string]any{"plan_id": plan.ID.String(), "source_hash": plan.SourceHash, "base_generation": plan.BaseGeneration, "summary": map[string]any{"update_signing_secret": true}})
	}
}

// planWebhookSigningSecret preserves sibling registrations and rejects a concurrent configuration edit before offering a receipt.
func planWebhookSigningSecret(ctx context.Context, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, actor accesscontrol.Actor, apiKey, slug, secret string) (*store.ConfigPlan, error) {
	registration, err := s.GetWorkspaceWebhookBySlug(ctx, slug)
	// Ingress lookup is global, so compare the account before reading private desired state.
	if err != nil || registration == nil || registration.AccountID != actor.AccountID {
		return nil, workspaceConfigHTTPError{status: http.StatusNotFound, message: "webhook registration not found"}
	}
	read := accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: registration.ServiceID}}
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, read); err != nil {
		return nil, err
	}
	current, err := configStore.GetConfigState(ctx, registration.OwningConfigKey)
	// Only named webhook configs can be safely reconciled through this action.
	if err != nil || current == nil || current.ConfigType != store.ConfigTypeWebhook {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook configuration is unavailable"}
	}
	var doc webhookConfigDocument
	if err := json.Unmarshal(current.DesiredState, &doc); err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook configuration is invalid"}
	}
	resolved, err := resolveWebhookServices(ctx, s, registryClient, apiKey, doc)
	if err != nil {
		return nil, err
	}
	target := ""
	for name, service := range resolved {
		// Preserve the exact authored service key rather than guessing from a display name.
		if service.ServiceID == registration.ServiceID {
			target = name
		}
		// Every service in the retained bundle remains subject to read authorization.
		requirement := accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service.ServiceID}}
		if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, requirement); err != nil {
			return nil, err
		}
	}
	// A registration whose owning document no longer includes it cannot be recreated implicitly.
	if target == "" {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook no longer belongs to this configuration"}
	}
	updated, err := webhookDocumentWithSigningSecret(doc, target, secret)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	raw, err := json.Marshal(updated)
	if err != nil {
		return nil, err
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	plan, _, err := createWebhookConfigPlan(ctx, configStore, s, verifier, registryClient, webhookPlanCall{apiKey: apiKey, accountID: actor.AccountID, actor: actor, request: SDKConfigPlanRequest{ConfigKey: current.ConfigKey, SourceHash: hash, Config: raw}, document: updated})
	if err != nil {
		return nil, err
	}
	// A later apply also checks generation; this closes the gap between source read and planning.
	if plan.BaseGeneration != current.Generation {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "webhook changed while reviewing; refresh and try again"}
	}
	return plan, nil
}

// webhookDocumentWithSigningSecret replaces only one reference, preserving callback and relay policy plus every sibling service.
func webhookDocumentWithSigningSecret(doc webhookConfigDocument, target, secret string) (webhookConfigDocument, error) {
	service, ok := doc.Services[target]
	// Replacements must name an existing service and cannot disable signature verification.
	if !ok || strings.TrimSpace(secret) == "" {
		return doc, fmt.Errorf("an existing service and signing secret reference are required")
	}
	service.Secret = strings.TrimSpace(secret)
	doc.Services[target] = service
	return doc, validateWebhookConfigDocument(doc)
}
