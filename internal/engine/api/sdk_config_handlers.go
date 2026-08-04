package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

type SDKConfigPlanRequest struct {
	ConfigKey     string          `json:"config_key"`
	SourceHash    string          `json:"source_hash"`
	OwnerTeamSlug string          `json:"owner_team,omitempty"`
	Config        json.RawMessage `json:"config"`
	// Owner IDs are resolved by the Engine. They are deliberately excluded
	// from the wire contract so people use stable team slugs, never UUIDs.
	OwnerSubjectID *uuid.UUID `json:"-"`
	OwnerTeamID    *uuid.UUID `json:"-"`
}

type SDKConfigApplyRequest struct {
	PlanID     string `json:"plan_id"`
	SourceHash string `json:"source_hash"`
}

type sdkConfigDocument struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Language   string `json:"language"`
	Bucket     string `json:"bucket,omitempty"`
	// WebhookAttachment names one kind: webhook artifact this SDK/MCP wants
	// event delivery from -- mirrors cli/internal/configfile's
	// ArtifactConfig.WebhookAttachment field-for-field (same yaml/json key),
	// since this struct is the Engine-side decode target for that exact wire
	// document. Required (validated in validateSDKConfigDocument) whenever
	// any service below selects webhooks -- the WS bridge
	// (websocket_handler.go) resolves this back out of fused_artifact_scopes at
	// connect time to scope FilterSubjects to just this artifact's
	// registrations. See plans/plan-webhook-kind.md.
	WebhookAttachment string                         `json:"webhook_attachment,omitempty"`
	Services          map[string]sdkConfigServiceDoc `json:"services"`
}

var sdkArtifactVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type sdkConfigServiceDoc struct {
	Version    string   `json:"version"`
	Operations []string `json:"operations"`
	Webhooks   []string `json:"webhooks,omitempty"`
	SelectAll  bool     `json:"select_all,omitempty"`
	// WebhooksSelectAll is the webhook-only counterpart to SelectAll --
	// mirrors ArtifactService.WebhooksSelectAll (same key) so a service can
	// select every webhook event independent of whether it also selects
	// every operation. Threaded into models.SDKSelection.WebhookSelectAll at
	// resolution time; see that field's doc comment for how the Registry
	// generator treats it.
	WebhooksSelectAll bool                   `json:"webhooks_select_all,omitempty"`
	Auth              *sdkArtifactAuthDoc    `json:"auth,omitempty"`
	Connect           *sdkArtifactConnectDoc `json:"connect,omitempty"`
	Injections        []InjectionConfig      `json:"injections,omitempty"`
}

type sdkArtifactAuthDoc struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type sdkArtifactConnectDoc struct {
	Scopes []string `json:"scopes,omitempty"`
}

type GenerateSDKRequest = models.SDKGenerationRequest

type sdkContractBinding = models.SDKContractBinding

// artifactResolvedPayload is the shared, generation-free record of resolved
// selections used by both SDK and MCP config apply. SDK generation adds its
// Registry-specific fields separately, while MCP never carries a target.
type artifactResolvedPayload struct {
	BucketID         uuid.UUID             `json:"bucket_id"`
	Name             string                `json:"name,omitempty"`
	Description      string                `json:"description,omitempty"`
	Version          string                `json:"version,omitempty"`
	Selections       []models.SDKSelection `json:"selections"`
	IncludeMCP       bool                  `json:"include_mcp,omitempty"`
	TargetType       string                `json:"target_type,omitempty"`
	TargetLanguage   string                `json:"target_language,omitempty"`
	ContractBindings []sdkContractBinding  `json:"contract_bindings,omitempty"`
}

type sdkPlanCall struct {
	apiKey    string
	accountID uuid.UUID
	actor     accesscontrol.Actor
	request   SDKConfigPlanRequest
	document  sdkConfigDocument
}

type sdkPlanResult struct {
	plan          *store.ConfigPlan
	summary       map[string]any
	notifications notificationInbox
}

type notificationInbox struct {
	Items    []workspaceNotificationInboxItem `json:"items"`
	Warnings []string                         `json:"warnings"`
}

type sdkResolvedService struct {
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	Version          string
	ServiceName      string
}

type sdkApplyCall struct {
	apiKey       string
	accountID    uuid.UUID
	actor        accesscontrol.Actor
	planID       uuid.UUID
	planRevision int
	applyLeaseID uuid.UUID
	sourceHash   string
}

type sdkGenerationResult struct {
	models.SDKGenerationResult
	ExecutionToken                     string `json:"execution_token,omitempty"`
	createdForPlan                     bool
	registryGenerationAttempted        bool
	registryGenerationOutcomeConfirmed bool
}

type sdkProxyError struct {
	status int
	body   []byte
}

func (e sdkProxyError) Error() string { return "sdk generation proxy failed" }

type sdkServiceSlugResolver interface {
	ResolveServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error)
}

// SDKConfigPlanHandler handles POST /sdk-config/plan.
func SDKConfigPlanHandler(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_config.plan")
		defer span.End()

		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, ctx)
			return
		}
		req, doc, err := decodeSDKConfigPlanRequest(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}, ctx)
			return
		}
		setSDKConfigSpanAttributes(span, req.ConfigKey, doc)
		result, err := createSDKConfigPlan(ctx, configStore, s, registryClient, sdkPlanCall{
			apiKey:    r.Header.Get("X-API-Key"),
			accountID: actor.AccountID,
			actor:     actor,
			request:   req,
			document:  doc,
		})
		if err != nil {
			writeSDKConfigError(w, err, ctx)
			return
		}

		span.SetAttributes(attribute.String("plan_id", result.plan.ID.String()), attribute.String("outcome", "success"))
		writeJSON(w, map[string]any{
			"plan_id":              result.plan.ID.String(),
			"owner_type":           planOwnerType(result.plan),
			"config_key":           result.plan.ConfigKey,
			"source_hash":          result.plan.SourceHash,
			"base_generation":      result.plan.BaseGeneration,
			"required_permissions": result.plan.RequiredPermissions,
			"summary":              result.summary,
			"notifications":        result.notifications,
		})
	}
}

// SDKConfigApplyHandler handles POST /sdk-config/apply.
func SDKConfigApplyHandler(configStore store.ConfigRepository, s store.Store, proxy Forwarder, clients ...sandbox.RegistryClient) http.HandlerFunc {
	var registryClient sandbox.RegistryClient
	if len(clients) > 0 {
		registryClient = clients[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_config.apply")
		defer span.End()

		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, ctx)
			return
		}
		req, planID, err := decodeSDKConfigApplyRequest(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}, ctx)
			return
		}
		planRevision, ok := AuthorizedPlanRevisionFromContext(ctx)
		if !ok {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusForbidden, message: "authorized plan revision unavailable"}, ctx)
			return
		}
		span.SetAttributes(
			attribute.String("config.source_hash", req.SourceHash),
			attribute.String("plan_id", planID.String()),
		)
		result, err := executeSDKConfigApply(ctx, configStore, s, proxy, registryClient, sdkApplyCall{
			apiKey:       r.Header.Get("X-API-Key"),
			accountID:    actor.AccountID,
			actor:        actor,
			planID:       planID,
			planRevision: planRevision,
			sourceHash:   req.SourceHash,
		})
		if err != nil {
			writeSDKConfigError(w, err, ctx)
			return
		}

		span.SetAttributes(attribute.String("outcome", "success"))
		status := "applied"
		if result.Status == models.SDKGenerationStatusPending {
			status = models.SDKGenerationStatusPending
		}
		resp := map[string]string{
			"status":      status,
			"plan_id":     planID.String(),
			"artifact_id": result.ArtifactID.String(),
			"job_id":      result.JobID,
		}
		if result.ExecutionToken != "" {
			resp["execution_token"] = result.ExecutionToken
			setOneTimeSecretResponseHeaders(w)
		}
		writeJSON(w, resp)
	}
}

// decodeSDKConfigPlanRequest validates the envelope and artifact identity
// before Registry lookups or plan persistence can occur.
func decodeSDKConfigPlanRequest(r *http.Request) (SDKConfigPlanRequest, sdkConfigDocument, error) {
	var req SDKConfigPlanRequest
	if err := decodeOneStrictJSON(r.Body, &req); err != nil {
		return req, sdkConfigDocument{}, errors.New("invalid request body")
	}
	if strings.TrimSpace(req.SourceHash) == "" {
		return req, sdkConfigDocument{}, errors.New("source_hash is required")
	}
	if strings.TrimSpace(req.ConfigKey) == "" {
		return req, sdkConfigDocument{}, errors.New("config_key is required")
	}
	if err := rejectRemovedSDKConfigFields(req.Config); err != nil {
		return req, sdkConfigDocument{}, err
	}
	var doc sdkConfigDocument
	if err := decodeArtifactConfigJSON(req.Config, &doc); err != nil {
		return req, doc, errors.New("invalid config json")
	}
	if err := validateSDKConfigDocument(doc); err != nil {
		return req, doc, err
	}
	if err := validateSDKConfigKey(req.ConfigKey, doc); err != nil {
		return req, doc, err
	}
	return req, doc, nil
}

// decodeArtifactConfigJSON rejects misspelled and obsolete fields at the
// Engine boundary; otherwise a valid-looking plan could silently omit policy.
func decodeArtifactConfigJSON(raw []byte, target *sdkConfigDocument) error {
	return decodeOneStrictJSON(bytes.NewReader(raw), target)
}

func decodeOneStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("config must contain exactly one JSON object")
	}
	return nil
}

func rejectRemovedSDKConfigFields(raw json.RawMessage) error {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return errors.New("invalid config json")
	}
	// The new artifact contract has no compatibility bridge: version identifies
	// the artifact directly and kind-specific routes replace target switching.
	for _, field := range []string{"sdkVersion", "target"} {
		if _, ok := doc[field]; ok {
			return fmt.Errorf("config uses the unsupported %s field", field)
		}
	}
	services, _ := doc["services"].(map[string]any)
	for serviceName, rawService := range services {
		service, _ := rawService.(map[string]any)
		if _, ok := service["endpoints"]; ok {
			return fmt.Errorf("service %s uses the unsupported endpoints field; use operations", serviceName)
		}
	}
	return nil
}

func validateSDKConfigDocument(doc sdkConfigDocument) error {
	if err := validateSDKIdentity(doc); err != nil {
		return err
	}
	if err := validateWebhookAttachmentRequired(doc); err != nil {
		return err
	}
	return validateArtifactServiceDocs(doc.Services)
}

func validateSDKIdentity(doc sdkConfigDocument) error {
	if doc.APIVersion != "fused/v1" {
		return errors.New("config apiVersion must be fused/v1")
	}
	if doc.Kind != "sdk" {
		return errors.New("config kind must be sdk")
	}
	if strings.TrimSpace(doc.Name) == "" || strings.TrimSpace(doc.Version) == "" {
		return errors.New("sdk config requires name and version")
	}
	if !sdkArtifactVersionPattern.MatchString(doc.Version) {
		return errors.New("sdk config requires a SemVer-compatible version")
	}
	if doc.Language != "typescript" && doc.Language != "python" && doc.Language != "go" {
		return fmt.Errorf("invalid sdk language %q", doc.Language)
	}
	if strings.TrimSpace(doc.Bucket) == "" {
		return errors.New("sdk config requires exactly one bucket")
	}
	return nil
}

// validateWebhookAttachmentRequired mirrors the CLI's own plan-time check
// (cli/internal/configfile.validateArtifactServices) server-side: a service
// that selects webhooks with nothing named to attach them to would silently
// receive no deliveries at all (websocket_handler.go has no registration
// identity to scope FilterSubjects to), so this is rejected up front instead
// of failing quietly at runtime.
func validateWebhookAttachmentRequired(doc sdkConfigDocument) error {
	if strings.TrimSpace(doc.WebhookAttachment) != "" {
		return nil
	}
	for name, service := range doc.Services {
		if len(service.Webhooks) > 0 || service.WebhooksSelectAll {
			return fmt.Errorf("service %s selects webhooks but the config has no webhook_attachment", name)
		}
	}
	return nil
}

// validateWebhookAttachmentCoverage mirrors validateArtifactBucketReadiness's
// pattern for webhook_attachment, the same way this artifact already
// requires its one bucket to exist: the named kind: webhook artifact must
// already exist, and must register every service this artifact selects
// webhooks for. Without this, a service selecting webhooks whose attached
// artifact never registered it fails silently -- no signing secret, no
// endpoint accepting deliveries under this label, the events just never
// arrive with no error anywhere (see plans/plan-webhook-kind.md's "known
// gap" note). Checked once at plan time (resolveSDKSelections) and again at
// apply (generateSDKForApply/executeMCPConfigApply), same defense-in-depth
// as bucket readiness, since the referenced webhook artifact can be edited
// or removed between plan and apply.
func validateWebhookAttachmentCoverage(ctx context.Context, configStore store.ConfigRepository, doc sdkConfigDocument) error {
	name := strings.TrimSpace(doc.WebhookAttachment)
	if name == "" {
		return nil // validateWebhookAttachmentRequired already guarantees no service needs one
	}
	state, err := configStore.GetConfigState(ctx, "webhook:"+name)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch webhook attachment state"}
	}
	if state == nil {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "webhook attachment not found: " + name}
	}
	registered, err := webhookAttachmentServiceNames(state.DesiredState)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "invalid webhook attachment state"}
	}
	for svcName, service := range doc.Services {
		if len(service.Webhooks) == 0 && !service.WebhooksSelectAll {
			continue
		}
		if !registered[svcName] {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf(
				"service %s selects webhooks but webhook attachment %q does not register it", svcName, name)}
		}
	}
	return nil
}

// decodeArtifactApplyPlan decodes a stored SDK/MCP plan's desired state and
// resolved payload, then re-verifies both cross-references apply depends on
// (bucket readiness, webhook attachment coverage) still hold -- grouped into
// one call so generateSDKForApply/executeMCPConfigApply each need only one
// branch here instead of four, keeping them under the complexity budget.
// kind customizes the "invalid resolved ... plan" message only; decoding and
// validation are identical for both, so this is the one place that logic
// lives rather than duplicated per artifact kind.
func decodeArtifactApplyPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, plan *store.ConfigPlan, kind string) (sdkConfigDocument, artifactResolvedPayload, error) {
	var doc sdkConfigDocument
	var payload artifactResolvedPayload
	if json.Unmarshal(plan.DesiredState, &doc) != nil || json.Unmarshal(plan.ResolvedPayload, &payload) != nil {
		return sdkConfigDocument{}, artifactResolvedPayload{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid resolved " + kind + " plan"}
	}
	if err := validateArtifactBucketIdentity(ctx, s, doc.Bucket, payload.BucketID); err != nil {
		return sdkConfigDocument{}, artifactResolvedPayload{}, err
	}
	if err := validateArtifactBucketReadiness(ctx, s, payload.BucketID, payload.Selections); err != nil {
		return sdkConfigDocument{}, artifactResolvedPayload{}, err
	}
	if err := validateWebhookAttachmentCoverage(ctx, configStore, doc); err != nil {
		return sdkConfigDocument{}, artifactResolvedPayload{}, err
	}
	return doc, payload, nil
}

// webhookAttachmentServiceNames extracts just the service keys from a
// kind: webhook artifact's stored desired state -- the only thing coverage
// validation needs. Kept local rather than importing
// webhook_config_handlers.go's own decode target, since that struct's
// Services value shape (secret ref, string) is irrelevant here and would be
// a coupling this check doesn't actually need.
func webhookAttachmentServiceNames(desiredState json.RawMessage) (map[string]bool, error) {
	var doc struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(desiredState, &doc); err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(doc.Services))
	for name := range doc.Services {
		names[name] = true
	}
	return names, nil
}

// validateArtifactServiceDocs rejects empty operation surfaces and unknown
// auth types before selectors are resolved against Registry metadata.
func validateArtifactServiceDocs(services map[string]sdkConfigServiceDoc) error {
	if len(services) == 0 {
		return errors.New("artifact config requires at least one service")
	}
	for name, service := range services {
		// A service may select only webhooks (no operations at all) -- MCP
		// already rejects non-empty Webhooks/WebhooksSelectAll earlier in
		// validateArtifactConfigDocument, so by the time this runs for an mcp
		// document those are always empty/false and this check degrades to
		// the original operations-only gate for that kind.
		if err := validateArtifactServiceDoc(name, service); err != nil {
			return err
		}
	}
	return nil
}

func validateArtifactServiceDoc(name string, service sdkConfigServiceDoc) error {
	if len(service.Operations) == 0 && !service.SelectAll && len(service.Webhooks) == 0 && !service.WebhooksSelectAll {
		return fmt.Errorf("service %s requires at least one operation or webhook", name)
	}
	if service.Auth == nil {
		return nil
	}
	if strings.TrimSpace(service.Auth.Type) == "" {
		return fmt.Errorf("service %s auth requires type", name)
	}
	if !validArtifactAuthType(service.Auth.Type) {
		return fmt.Errorf("service %s auth type must be one of basic, bearer, api_key, oauth, oidc, or mtls", name)
	}
	return nil
}

// validArtifactAuthType keeps the public config vocabulary independent of
// provider-specific OpenAPI scheme names.
func validArtifactAuthType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "basic", "bearer", "api_key", "oauth", "oidc", "mtls":
		return true
	default:
		return false
	}
}

func validateSDKConfigKey(configKey string, doc sdkConfigDocument) error {
	expected := fmt.Sprintf("sdk:%s:%s", doc.Name, doc.Version)
	// Engine enforces the same identity the CLI derives, so hand-written API
	// callers cannot store plans under stale or ambiguous sdk:<name> keys.
	if configKey != expected {
		return fmt.Errorf("config_key %q must match %q", configKey, expected)
	}
	return nil
}

func setSDKConfigSpanAttributes(span trace.Span, configKey string, doc sdkConfigDocument) {
	span.SetAttributes(
		attribute.String("config_key", configKey),
		attribute.String("artifact.kind", doc.Kind),
		attribute.String("artifact.name", doc.Name),
		attribute.String("artifact.version", doc.Version),
		attribute.String("artifact.language", doc.Language),
		attribute.Int("artifact.service_count", len(doc.Services)),
	)
}

func createSDKConfigPlan(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	registryClient sandbox.RegistryClient,
	call sdkPlanCall,
) (sdkPlanResult, error) {
	currentState, err := configStore.GetConfigState(ctx, call.request.ConfigKey)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	owner, bucket, err := resolveArtifactPlanOwnerAndBucket(
		ctx, s, currentState, call.actor, call.request.OwnerTeamSlug, call.document.Bucket,
	)
	if err != nil {
		return sdkPlanResult{}, err
	}
	call.request.OwnerSubjectID, call.request.OwnerTeamID = owner.subjectID, owner.teamID
	selections, services, resolvedServices, stateDoc, err := resolveSDKSelections(ctx, configStore, s, registryClient, call.apiKey, call.document, previousSDKDocument(currentState), bucket.ID)
	if err != nil {
		return sdkPlanResult{}, err
	}
	bindings, err := resolveSDKContractBindings(ctx, registryClient, call.apiKey, resolvedServices)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to bind service contract revisions"}
	}
	selections = attachSDKServiceVersionIDs(selections, bindings)
	notifications := collectSDKPlanNotifications(ctx, configStore, registryClient, call, resolvedServices)
	resolvedPayload, _ := json.Marshal(resolvedSDKPayload(sdkGenerateRequest(call.document, selections, bindings), bucket.ID))
	desiredState, _ := json.Marshal(stateDoc)
	requiredPermissions, requiredCount, err := artifactPlanRequiredPermissionsWithBuckets(
		ctx, currentState, serviceNamesFromResolved(resolvedServices), []store.Bucket{*bucket}, call.document.Name,
	)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to compute required permissions"}
	}
	if err := preflightArtifactOwnership(ctx, s, call.actor, owner, existingArtifactID(currentState), requiredPermissions); err != nil {
		return sdkPlanResult{}, err
	}
	plan, err := configStore.CreateConfigPlan(ctx, store.CreateConfigPlanParams{
		ConfigKey:           call.request.ConfigKey,
		ConfigType:          store.ConfigTypeSDK,
		OwnerSubjectID:      call.request.OwnerSubjectID,
		OwnerTeamID:         call.request.OwnerTeamID,
		SourceHash:          call.request.SourceHash,
		BaseGeneration:      currentGeneration(currentState),
		Actions:             []byte("[]"),
		DesiredState:        desiredState,
		ResolvedPayload:     resolvedPayload,
		Blockers:            []byte("[]"),
		Warnings:            []byte("[]"),
		RequiredPermissions: requiredPermissions,
		CreatedBy:           call.accountID,
		SupersedeExisting:   true,
	})
	if err != nil {
		slog.ErrorContext(ctx, "SDKConfigPlanHandler: CreateConfigPlan error", slog.Any("error", err))
		return sdkPlanResult{}, configPlanSaveHTTPError(err)
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("required_permissions_count", requiredCount))
	return sdkPlanResult{
		plan:          plan,
		summary:       sdkPlanSummary(currentState == nil, services),
		notifications: notifications,
	}, nil
}

func resolveArtifactPlanOwnerAndBucket(
	ctx context.Context,
	s store.Store,
	current *store.ConfigState,
	actor accesscontrol.Actor,
	requestedOwnerTeamSlug string,
	bucketName string,
) (artifactOwner, *store.Bucket, error) {
	owner, err := resolveArtifactPlanOwner(ctx, s, current, actor, requestedOwnerTeamSlug)
	if err != nil {
		return artifactOwner{}, nil, err
	}
	bucket, err := resolveArtifactBucket(ctx, s, bucketName)
	if err != nil {
		return artifactOwner{}, nil, err
	}
	return owner, bucket, nil
}

func resolveSDKSelections(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	registryClient sandbox.RegistryClient,
	apiKey string,

	doc sdkConfigDocument,
	previous sdkConfigDocument,
	bucketID uuid.UUID,
) ([]models.SDKSelection, []map[string]any, []sdkResolvedService, sdkConfigDocument, error) {
	doc = canonicalArtifactDocument(doc)
	services, err := workspaceServicesByConfigKey(ctx, s, registryClient, apiKey, doc)
	if err != nil {
		return nil, nil, nil, sdkConfigDocument{}, err
	}
	// One batched lookup for every service this SDK config references,
	// instead of one ListWorkspaceServiceVersions call per service inside the
	// loop below -- see ensureSDKVersionAllowed's doc comment.
	allowedVersions, err := s.ListWorkspaceServiceVersionsForServices(ctx, sdkReferencedServiceIDs(doc, services))
	if err != nil {
		return nil, nil, nil, sdkConfigDocument{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list allowed versions"}
	}
	var selections []models.SDKSelection
	var summary []map[string]any
	var resolved []sdkResolvedService
	stateDoc := doc
	stateDoc.Services = make(map[string]sdkConfigServiceDoc, len(doc.Services))
	for serviceName, serviceDoc := range doc.Services {
		activation, ok := services[serviceName]
		resolvedServiceVersionID, resolvedVersionStr, err := validateSDKServiceSelection(ctx, registryClient, serviceName, serviceDoc, activation, ok, allowedVersions)
		if err != nil {
			return nil, nil, nil, sdkConfigDocument{}, err
		}
		serviceDoc.Version = resolvedVersionStr
		selections = append(selections, models.SDKSelection{
			ServiceID:        activation.ServiceID,
			ServiceVersionID: resolvedServiceVersionID,
			OperationNames:   serviceDoc.Operations,
			WebhookNames:     serviceDoc.Webhooks,
			SelectAll:        serviceDoc.SelectAll,
			WebhookSelectAll: serviceDoc.WebhooksSelectAll,
			AuthType:         artifactAuthType(serviceDoc.Auth),
			AuthName:         artifactAuthName(serviceDoc.Auth),
			ConnectScopes:    artifactConnectScopes(serviceDoc.Connect),
			Injections:       artifactInjections(serviceDoc.Injections),
		})
		summary = append(summary, sdkServiceSummary(serviceName, serviceDoc, previous.Services[serviceName].Operations))
		resolved = append(resolved, sdkResolvedService{
			ServiceID: activation.ServiceID, ServiceVersionID: resolvedServiceVersionID,
			Version: resolvedVersionStr, ServiceName: activation.ServiceName,
		})
		stateDoc.Services[activation.ServiceName] = serviceDoc
	}
	if err := resolveArtifactAuthPolicies(ctx, registryClient, apiKey, resolved, selections); err != nil {
		return nil, nil, nil, sdkConfigDocument{}, err
	}
	if err := validateArtifactBucketReadiness(ctx, s, bucketID, selections); err != nil {
		return nil, nil, nil, sdkConfigDocument{}, err
	}
	if err := validateWebhookAttachmentCoverage(ctx, configStore, doc); err != nil {
		return nil, nil, nil, sdkConfigDocument{}, err
	}

	if registryClient != nil {
		if err := registryClient.ValidateSDKSelections(ctx, selections); err != nil {
			return nil, nil, nil, sdkConfigDocument{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
		}
	}

	return selections, summary, resolved, stateDoc, nil
}

// canonicalArtifactDocument removes presentation-only ordering differences
// before an artifact is resolved, hashed, or persisted as immutable state.
func canonicalArtifactDocument(doc sdkConfigDocument) sdkConfigDocument {
	canonical := doc
	canonical.APIVersion = strings.TrimSpace(doc.APIVersion)
	canonical.Kind = strings.TrimSpace(doc.Kind)
	canonical.Name = strings.TrimSpace(doc.Name)
	canonical.Version = strings.TrimSpace(doc.Version)
	canonical.Language = strings.TrimSpace(doc.Language)
	canonical.Bucket = strings.TrimSpace(doc.Bucket)
	canonical.WebhookAttachment = strings.TrimSpace(doc.WebhookAttachment)
	canonical.Services = make(map[string]sdkConfigServiceDoc, len(doc.Services))
	for name, service := range doc.Services {
		service.Version = strings.TrimSpace(service.Version)
		service.Operations = sortedUniqueStrings(service.Operations)
		service.Webhooks = sortedUniqueStrings(service.Webhooks)
		if service.Auth != nil {
			auth := *service.Auth
			auth.Type = strings.ToLower(strings.TrimSpace(auth.Type))
			auth.Name = strings.TrimSpace(auth.Name)
			service.Auth = &auth
		}
		if service.Connect != nil {
			connect := *service.Connect
			connect.Scopes = sortedUniqueStrings(connect.Scopes)
			service.Connect = &connect
		}
		canonical.Services[strings.TrimSpace(name)] = service
	}
	return canonical
}

// canonicalArtifactState serializes the canonical desired state so identity
// comparisons ignore YAML/JSON ordering and harmless whitespace changes.
func canonicalArtifactState(doc sdkConfigDocument) ([]byte, error) {
	return json.Marshal(canonicalArtifactDocument(doc))
}

// artifactAuthType normalizes only the public type selector; exact scheme
// resolution remains Registry-owned.
func artifactAuthType(auth *sdkArtifactAuthDoc) string {
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Type))
}

// artifactAuthName preserves an explicit scheme name for same-type
// disambiguation without treating it as credential material.
func artifactAuthName(auth *sdkArtifactAuthDoc) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.Name)
}

// artifactConnectScopes canonicalizes the consent ceiling so source order and
// duplicate values cannot alter artifact identity at runtime.
func artifactConnectScopes(connect *sdkArtifactConnectDoc) []string {
	if connect == nil {
		return nil
	}
	return sortedUniqueStrings(connect.Scopes)
}

type sdkServiceVersionAuthConfigFetcher interface {
	FetchServiceVersionAuthConfigs(context.Context, []sandbox.ServiceVersionRef, string) ([]sandbox.ServiceVersionAuthConfigs, error)
}

// resolveArtifactAuthPolicies turns human-facing auth selectors into the
// exact Registry scheme used at dispatch. Doing this once during planning
// keeps agents and SDK consumers from guessing provider-specific auth names.
func resolveArtifactAuthPolicies(ctx context.Context, registryClient sandbox.RegistryClient, apiKey string, services []sdkResolvedService, selections []models.SDKSelection) error {
	fetcher, ok := registryClient.(sdkServiceVersionAuthConfigFetcher)
	if !ok {
		return validateNoExplicitAuthPolicy(selections)
	}
	configs, err := fetcher.FetchServiceVersionAuthConfigs(ctx, sdkServiceVersionRefs(services), apiKey)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to resolve service auth policies"}
	}
	byService := make(map[uuid.UUID]sandbox.ServiceVersionAuthConfigs, len(configs))
	for _, config := range configs {
		byService[config.ServiceID] = config
	}
	for index := range selections {
		if err := resolveSelectionAuthPolicy(&selections[index], byService[selections[index].ServiceID].AuthConfigs); err != nil {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
		}
	}
	return nil
}

// validateNoExplicitAuthPolicy fails closed when Registry cannot resolve a
// policy the user explicitly requested; auth-free artifacts remain usable.
func validateNoExplicitAuthPolicy(selections []models.SDKSelection) error {
	for _, selection := range selections {
		if selection.AuthType != "" || selection.AuthName != "" || len(selection.ConnectScopes) > 0 {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "registry auth policy resolution is unavailable"}
		}
	}
	return nil
}

// resolveSelectionAuthPolicy pins one exact provider scheme and its approved
// consent ceiling into the immutable runtime selection.
func resolveSelectionAuthPolicy(selection *models.SDKSelection, auths fusedobject.AuthConfigs) error {
	if len(auths) == 0 {
		if selection.AuthType != "" || selection.AuthName != "" || len(selection.ConnectScopes) > 0 {
			return fmt.Errorf("service %s does not declare authentication", selection.ServiceID)
		}
		return nil
	}
	matches := matchingArtifactAuths(auths, selection.AuthType, selection.AuthName)
	if len(matches) == 0 {
		return fmt.Errorf("service %s does not support the selected authentication", selection.ServiceID)
	}
	if len(matches) > 1 {
		return fmt.Errorf("service %s auth selection is ambiguous; set auth.name", selection.ServiceID)
	}
	selected := matches[0]
	selection.AuthType = sandbox.CanonicalFusedAuthType(selected)
	selection.AuthName = sandbox.AuthCredentialName(selected)
	return validateArtifactScopes(selection, selected.Scopes)
}

// matchingArtifactAuths defaults to the provider's first declared scheme but
// requires a unique match whenever the artifact supplies selectors.
func matchingArtifactAuths(auths fusedobject.AuthConfigs, authType, authName string) fusedobject.AuthConfigs {
	if authType == "" && authName == "" {
		return auths[:1]
	}
	var matches fusedobject.AuthConfigs
	for _, auth := range auths {
		if authName != "" && auth.Name != authName {
			continue
		}
		if authType != "" && sandbox.CanonicalFusedAuthType(auth) != authType {
			continue
		}
		matches = append(matches, auth)
	}
	return matches
}

// validateArtifactScopes lets an artifact narrow OAuth/OIDC permissions while
// preventing config from requesting scopes absent from the provider contract.
func validateArtifactScopes(selection *models.SDKSelection, allowed []string) error {
	if len(selection.ConnectScopes) == 0 {
		return nil
	}
	if selection.AuthType != "oauth" && selection.AuthType != "oidc" {
		return fmt.Errorf("service %s connect scopes require oauth or oidc auth", selection.ServiceID)
	}
	allowedSet := stringSet(allowed)
	for _, scope := range selection.ConnectScopes {
		if !allowedSet[scope] {
			return fmt.Errorf("service %s connect scope %q is not provider-approved", selection.ServiceID, scope)
		}
	}
	return nil
}

// sortedUniqueStrings produces deterministic policy data for plans, hashes,
// generated types, and consent URLs.
func sortedUniqueStrings(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			set[trimmed] = true
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// sdkReferencedServiceIDs collects the activated service IDs an SDK config
// actually names, so callers can fetch allowed versions for exactly that set
// in one batched call. Services the config names but that aren't activated
// are skipped here -- validateSDKServiceSelection's own "not allowed in this
// workspace" check reports those, so there's no version list to fetch for
// them anyway.
func sdkReferencedServiceIDs(doc sdkConfigDocument, services map[string]store.WorkspaceService) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(doc.Services))
	for serviceName := range doc.Services {
		if activation, ok := services[serviceName]; ok {
			ids = append(ids, activation.ServiceID)
		}
	}
	return ids
}

func collectSDKPlanNotifications(
	ctx context.Context,
	configStore store.ConfigRepository,
	registryClient sandbox.RegistryClient,
	call sdkPlanCall,
	services []sdkResolvedService,
) notificationInbox {
	serviceVersions := sdkServiceVersionMap(services)
	inbox := notificationInbox{}
	notifications, err := configStore.ListWorkspaceNotifications(ctx, store.WorkspaceNotificationStatusPending)
	if err != nil {
		inbox.Warnings = append(inbox.Warnings, "engine_notifications_unavailable")
	} else {
		inbox.Items = append(inbox.Items, filterSDKEngineNotifications(notifications, call.request.ConfigKey, serviceVersions)...)
	}
	if registryClient == nil {
		return inbox
	}
	// One batched Registry call for every service this SDK config
	// references, instead of one FetchDriftSnapshots call per service.
	snapshots, err := registryClient.FetchDriftSnapshotsForServices(ctx, sortedUUIDKeys(serviceVersions), call.apiKey)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch sdk drift snapshots", slog.Any("error", err))
		inbox.Warnings = append(inbox.Warnings, "registry_notifications_unavailable")
		return inbox
	}
	for _, snapshot := range snapshots {
		inbox.Items = append(inbox.Items, registryDriftInboxItem(snapshot))
	}
	return inbox
}

func sdkServiceVersionMap(services []sdkResolvedService) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(services))
	for _, service := range services {
		out[service.ServiceID] = service.Version
	}
	return out
}

func filterSDKEngineNotifications(
	notifications []store.WorkspaceNotification,
	configKey string,
	serviceVersions map[uuid.UUID]string,
) []workspaceNotificationInboxItem {
	var items []workspaceNotificationInboxItem
	for _, notification := range notifications {
		if sdkNotificationMatches(notification, configKey, serviceVersions) {
			items = append(items, workspaceNotificationInboxItems([]store.WorkspaceNotification{notification})...)
		}
	}
	return items
}

func sdkNotificationMatches(notification store.WorkspaceNotification, configKey string, serviceVersions map[uuid.UUID]string) bool {
	if notification.ConfigKey == configKey {
		return true
	}
	if notification.ServiceID == nil {
		return false
	}
	version, ok := serviceVersions[*notification.ServiceID]
	if !ok {
		return false
	}
	return notification.Version == "" || notification.Version == version
}

func sortedUUIDKeys(items map[uuid.UUID]string) []uuid.UUID {
	keys := make([]uuid.UUID, 0, len(items))
	for id := range items {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

// registryDriftInboxItem builds an inbox item straight from a
// service-scoped DriftSnapshot -- ServiceID now comes from the snapshot
// itself (resolved by the repository's integration_objects/webhook_objects
// join), not a separately threaded loop variable, since the batched
// FetchDriftSnapshotsForServices call returns snapshots for many services
// at once and each one has to carry its own attribution.
func registryDriftInboxItem(snapshot models.DriftSnapshot) workspaceNotificationInboxItem {
	item := workspaceNotificationInboxItem{
		ID:                  "registry:" + snapshot.ID.String(),
		Source:              "registry",
		Type:                registryDriftType(snapshot),
		Severity:            driftSeverity(snapshot.Diff),
		Status:              snapshot.Status,
		ServiceID:           nonZeroUUIDString(snapshot.ServiceID),
		Message:             registryDriftMessage(snapshot),
		IntegrationObjectID: nonZeroUUIDString(snapshot.IntegrationObjectID),
		WebhookObjectID:     pointerUUIDString(snapshot.WebhookObjectID),
		Diff:                snapshot.Diff,
	}
	if !snapshot.DetectedAt.IsZero() {
		item.DetectedAt = snapshot.DetectedAt.UTC().Format(time.RFC3339)
	}
	return item
}

func registryDriftType(snapshot models.DriftSnapshot) string {
	if snapshot.WebhookObjectID != nil {
		return "webhook_drift"
	}
	return "endpoint_drift"
}

func driftSeverity(diff models.DriftChanges) string {
	for _, change := range diff {
		if change.Severity == "breaking" {
			return "breaking"
		}
	}
	return "non-breaking"
}

func registryDriftMessage(snapshot models.DriftSnapshot) string {
	for _, change := range snapshot.Diff {
		if strings.TrimSpace(change.Description) != "" {
			return change.Description
		}
	}
	return fmt.Sprintf("Drift detected for object %s", driftObjectID(snapshot))
}

func driftObjectID(snapshot models.DriftSnapshot) string {
	if snapshot.WebhookObjectID != nil {
		return snapshot.WebhookObjectID.String()
	}
	return snapshot.IntegrationObjectID.String()
}

func nonZeroUUIDString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func pointerUUIDString(id *uuid.UUID) string {
	if id == nil || *id == uuid.Nil {
		return ""
	}
	return id.String()
}

func workspaceServicesByName(ctx context.Context, s store.Store) (map[string]store.WorkspaceService, error) {
	workspaceServices, err := s.ListWorkspaceServices(ctx, nil)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list workspace services"}
	}
	return workspaceServicesByDisplayName(workspaceServices), nil
}

func workspaceServicesByConfigKey(
	ctx context.Context,
	s store.Store,
	registryClient sandbox.RegistryClient,
	apiKey string,

	doc sdkConfigDocument,
) (map[string]store.WorkspaceService, error) {
	workspaceServices, err := s.ListWorkspaceServices(ctx, nil)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list workspace services"}
	}
	services := workspaceServicesByDisplayName(workspaceServices)
	missing := unresolvedSDKServiceKeys(doc, services)
	if len(missing) == 0 {
		return services, nil
	}
	resolver, ok := registryClient.(sdkServiceSlugResolver)
	if !ok || resolver == nil {
		return services, nil
	}
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
	return services, nil
}

// workspaceServicesByDisplayName supports human-readable service keys. Display
// names are not unique, so slug binding uses the full activation list instead.
func workspaceServicesByDisplayName(workspaceServices []store.WorkspaceService) map[string]store.WorkspaceService {
	out := make(map[string]store.WorkspaceService, len(workspaceServices))
	for _, activation := range workspaceServices {
		out[activation.ServiceName] = activation
	}
	return out
}

func unresolvedSDKServiceKeys(doc sdkConfigDocument, services map[string]store.WorkspaceService) []string {
	var missing []string
	for serviceName := range doc.Services {
		if _, ok := services[serviceName]; !ok {
			missing = append(missing, serviceName)
		}
	}
	sort.Strings(missing)
	return missing
}

func workspaceServicesByID(workspaceServices []store.WorkspaceService) map[uuid.UUID]store.WorkspaceService {
	out := make(map[uuid.UUID]store.WorkspaceService, len(workspaceServices))
	for _, activation := range workspaceServices {
		out[activation.ServiceID] = activation
	}
	return out
}

func validateSDKServiceSelection(
	ctx context.Context,
	registryClient sandbox.RegistryClient,
	serviceName string,
	serviceDoc sdkConfigServiceDoc,
	activation store.WorkspaceService,
	found bool,
	allowedVersions map[uuid.UUID][]store.WorkspaceServiceVersion,
) (uuid.UUID, string, error) {
	if len(serviceDoc.Operations) == 0 && len(serviceDoc.Webhooks) == 0 && !serviceDoc.SelectAll && !serviceDoc.WebhooksSelectAll {
		return uuid.Nil, "", workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("service %s requires at least one operation or webhook", serviceName)}
	}
	if !found {
		return uuid.Nil, "", workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("service %s is not activated in this workspace. Run 'fused-cli workspace service add %s' to activate it.", serviceName, serviceName)}
	}
	resolvedVersion, err := resolveSDKVersionAllowed(activation, serviceDoc.Version, serviceName, allowedVersions[activation.ServiceID])
	if err != nil {
		return uuid.Nil, "", err
	}
	return resolvedVersion.ServiceVersionID, resolvedVersion.Version, nil
}

// resolveSDKVersionAllowed is a pure check against an already-fetched
// versions list -- callers fetch every referenced service's allowed
// versions once via ListWorkspaceServiceVersionsForServices and pass the relevant
// slice in, instead of this function querying the store itself once per
// service (the shape both resolveSDKSelections and
// ensureSDKDownloadAvailable used to call it in).
func resolveSDKVersionAllowed(
	activation store.WorkspaceService,
	version string,
	serviceName string,
	allowedVersions []store.WorkspaceServiceVersion,
) (*store.WorkspaceServiceVersion, error) {
	if len(allowedVersions) == 0 {
		return nil, workspaceConfigHTTPError{
			status:  http.StatusBadRequest,
			message: fmt.Sprintf("no allowed versions found for service %s", serviceName),
		}
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return &allowedVersions[0], nil
	}
	for _, allowed := range allowedVersions {
		if allowed.Version == version {
			return &allowed, nil
		}
	}
	return nil, workspaceConfigHTTPError{
		status:  http.StatusBadRequest,
		message: fmt.Sprintf("version %s for service %s is not allowed in this workspace", version, serviceName),
	}
}

func activationVersionExists(versions []store.WorkspaceServiceVersion, version string) bool {
	for _, allowed := range versions {
		if allowed.Version == version {
			return true
		}
	}
	return false
}

func activationVersionExistsByUUID(versions []store.WorkspaceServiceVersion, serviceVersionID uuid.UUID) bool {
	for _, allowed := range versions {
		if allowed.ServiceVersionID == serviceVersionID {
			return true
		}
	}
	return false
}

func normalizedOperationName(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "_"))
}

func previousSDKDocument(state *store.ConfigState) sdkConfigDocument {
	var previous sdkConfigDocument
	if state != nil && len(state.DesiredState) > 0 {
		_ = json.Unmarshal(state.DesiredState, &previous)
	}
	return previous
}

func sdkGenerateRequest(doc sdkConfigDocument, selections []models.SDKSelection, bindings []sdkContractBinding) GenerateSDKRequest {
	return GenerateSDKRequest{
		Name:             doc.Name,
		Description:      fmt.Sprintf("GitOps managed SDK %s", doc.Name),
		Version:          doc.Version,
		Selections:       selections,
		IncludeMCP:       false,
		TargetType:       "sdk",
		TargetLanguage:   doc.Language,
		ContractBindings: bindings,
	}
}

func resolvedSDKPayload(request GenerateSDKRequest, bucketID uuid.UUID) artifactResolvedPayload {
	return artifactResolvedPayload{
		BucketID: bucketID, Name: request.Name, Description: request.Description, Version: request.Version,
		Selections: request.Selections, IncludeMCP: request.IncludeMCP, TargetType: request.TargetType,
		TargetLanguage: request.TargetLanguage, ContractBindings: request.ContractBindings,
	}
}

func sdkPlanSummary(create bool, services []map[string]any) map[string]any {
	return map[string]any{
		"create_sdk": create,
		"update_sdk": !create,
		"services":   services,
	}
}

func sdkServiceSummary(serviceName string, serviceDoc sdkConfigServiceDoc, oldOperations []string) map[string]any {
	added, removed := diffStrings(oldOperations, serviceDoc.Operations)
	return map[string]any{
		"name":               serviceName,
		"version":            serviceDoc.Version,
		"operations_added":   added,
		"operations_removed": removed,
	}
}

func diffStrings(oldItems, newItems []string) ([]string, []string) {
	oldSet := stringSet(oldItems)
	newSet := stringSet(newItems)
	added := missingFrom(newSet, oldSet)
	removed := missingFrom(oldSet, newSet)
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func stringSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func missingFrom(source, comparison map[string]bool) []string {
	var out []string
	for item := range source {
		if !comparison[item] {
			out = append(out, item)
		}
	}
	return out
}

func decodeSDKConfigApplyRequest(r *http.Request) (SDKConfigApplyRequest, uuid.UUID, error) {
	var req SDKConfigApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, uuid.Nil, errors.New("invalid request body")
	}
	planID, err := uuid.Parse(req.PlanID)
	if err != nil {
		return req, uuid.Nil, errors.New("invalid plan_id")
	}
	return req, planID, nil
}

func executeSDKConfigApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	proxy Forwarder,
	registryClient sandbox.RegistryClient,
	call sdkApplyCall,
) (sdkGenerationResult, error) {
	// A plan has one deterministic Registry artifact. Serializing that identity
	// prevents a losing first apply from deleting the artifact committed by a
	// concurrent winner after both initially observed an empty local state.
	unlockArtifact := sdkGenerationApplies.lock(stableArtifactIDForPlan(call.planID))
	defer unlockArtifact()
	if _, err := loadAuthorizedSDKPlanForApply(ctx, configStore, s, call); err != nil {
		return sdkGenerationResult{}, err
	}
	lease, err := configStore.ReserveConfigPlanApply(ctx, call.planID, call.planRevision)
	if err != nil {
		return sdkGenerationResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_apply_in_progress_or_revision_changed"}
	}
	leaseGuard := workspaceApplyLeaseGuard{configStore: configStore, planID: call.planID, revision: call.planRevision, leaseID: lease.ID, releasable: true}
	defer leaseGuard.release()
	applyCtx, stopLease := workspaceApplyLeaseContextWithTimeout(ctx, configStore, call.planID, call.planRevision, lease.ID, sdkGenerationApplyTimeout+time.Minute)
	defer stopLease()
	call.applyLeaseID = lease.ID
	// From the first Registry call onward, release requires either a committed
	// local transaction or positively confirmed compensation. Unknown external
	// outcomes remain fenced until the database lease expires.
	leaseGuard.releasable = false

	plan, result, err := generateSDKForApply(applyCtx, configStore, s, proxy, registryClient, call)
	if err != nil {
		if result.createdForPlan {
			leaseGuard.releasable = compensateNewRegistryArtifact(applyCtx, proxy, result)
		} else if !result.registryGenerationAttempted || result.registryGenerationOutcomeConfirmed {
			leaseGuard.releasable = true
		}
		return sdkGenerationResult{}, err
	}
	applyCtx, scopeSpan := otel.Tracer("engine").Start(applyCtx, "engine.sdk_scope.persist")
	defer scopeSpan.End()
	scopeSpan.SetAttributes(
		attribute.String("artifact_id", result.ArtifactID.String()),
		attribute.String("sdk_generation_status", result.Status),
		attribute.Int("scope_schema_version", result.ScopeSchemaVersion),
	)
	if err := validateSDKGenerationResult(plan.ResolvedPayload, call, result.SDKGenerationResult); err != nil {
		scopeSpan.SetStatus(codes.Error, err.Error())
		scopeSpan.SetAttributes(attribute.String("outcome", "validation_failed"))
		leaseGuard.releasable = compensateNewRegistryArtifact(applyCtx, proxy, result)
		return sdkGenerationResult{}, err
	}
	token, _, err := applyGeneratedArtifactScope(applyCtx, configStore, s, call, plan, result)
	if err != nil {
		scopeSpan.SetStatus(codes.Error, err.Error())
		scopeSpan.SetAttributes(attribute.String("outcome", "scope_persist_failed"))
		leaseGuard.releasable = compensateNewRegistryArtifact(applyCtx, proxy, result)
		return sdkGenerationResult{}, err
	}
	leaseGuard.releasable = true
	result.ExecutionToken = token
	scopeSpan.SetAttributes(attribute.String("outcome", "success"))
	return result, nil
}

func generateSDKForApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	proxy Forwarder,
	registryClient sandbox.RegistryClient,
	call sdkApplyCall,
) (*store.ConfigPlan, sdkGenerationResult, error) {
	input, err := prepareSDKGenerationForApply(ctx, configStore, s, registryClient, call)
	if err != nil {
		return nil, sdkGenerationResult{}, err
	}
	result, err := runTrackedSDKGeneration(ctx, proxy, call.apiKey, input.payload)
	if err != nil {
		return input.plan, result, err
	}
	if err := validateRegistryArtifactIdentity(input.payload, result.ArtifactID); err != nil {
		// Preserve the unexpected Registry identity so the caller retains the
		// lease instead of treating this external outcome as safely untouched.
		return input.plan, result, err
	}
	// Ownership is established as soon as Registry creates the deterministic
	// identity, so stream failures can compensate it as well as DB failures.
	result.createdForPlan = input.existingArtifactID == uuid.Nil
	completed, err := awaitSDKGenerationCompletion(ctx, proxy, call.apiKey, result)
	if err != nil {
		return input.plan, result, err
	}
	return input.plan, completed, nil
}

type sdkGenerationApplyInput struct {
	plan               *store.ConfigPlan
	existingArtifactID uuid.UUID
	payload            json.RawMessage
}

func prepareSDKGenerationForApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	registryClient sandbox.RegistryClient,
	call sdkApplyCall,
) (sdkGenerationApplyInput, error) {
	plan, err := loadAuthorizedSDKPlanForApply(ctx, configStore, s, call)
	if err != nil {
		return sdkGenerationApplyInput{}, err
	}
	if err := ensureSDKSelectionsStillAllowed(ctx, s, plan.ResolvedPayload); err != nil {
		return sdkGenerationApplyInput{}, err
	}
	bindings, err := sdkContractBindingsFromPayload(plan.ResolvedPayload)
	if err != nil {
		return sdkGenerationApplyInput{}, err
	}
	if err := ensureSDKContractBindingsCurrent(ctx, registryClient, call.apiKey, bindings); err != nil {
		return sdkGenerationApplyInput{}, workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
	}
	if _, _, err := decodeArtifactApplyPlan(ctx, configStore, s, plan, "sdk"); err != nil {
		return sdkGenerationApplyInput{}, err
	}

	existingArtifactID := existingArtifactIDForApply(ctx, configStore, plan.ConfigKey)
	generationPayload, err := sdkGenerationPayloadForPlan(plan.ResolvedPayload, call, existingArtifactID)
	if err != nil {
		return sdkGenerationApplyInput{}, err
	}
	return sdkGenerationApplyInput{plan: plan, existingArtifactID: existingArtifactID, payload: generationPayload}, nil
}

func runTrackedSDKGeneration(ctx context.Context, proxy Forwarder, apiKey string, payload json.RawMessage) (sdkGenerationResult, error) {
	result := sdkGenerationResult{registryGenerationAttempted: true}
	generated, err := runSDKGeneration(ctx, proxy, apiKey, payload)
	if err != nil {
		var proxyErr sdkProxyError
		if errors.As(err, &proxyErr) && proxyErr.status >= 400 && proxyErr.status < 500 {
			result.registryGenerationOutcomeConfirmed = true
		}
		return result, err
	}
	result = generated
	result.registryGenerationAttempted = true
	return result, nil
}

func compensateNewRegistryArtifact(ctx context.Context, proxy Forwarder, result sdkGenerationResult) bool {
	if !result.createdForPlan || result.ArtifactID == uuid.Nil {
		return true
	}
	// Cleanup must outlive a disconnected caller, but remains bounded so a
	// Registry outage cannot strand an Engine request indefinitely.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cleanupCtx, span := otel.Tracer("engine").Start(cleanupCtx, "engine.sdk_generation.compensate")
	defer span.End()
	span.SetAttributes(attribute.String("artifact_id", result.ArtifactID.String()))
	if err := retireRegistrySDK(cleanupCtx, proxy, result.ArtifactID); err != nil {
		// The plan remains pending and keeps the same deterministic artifact ID,
		// so a retry reclaims the same Registry record and retries compensation.
		slog.ErrorContext(cleanupCtx, "failed to compensate rejected Registry artifact", slog.String("artifact_id", result.ArtifactID.String()))
		span.SetStatus(codes.Error, "registry artifact compensation failed")
		span.SetAttributes(attribute.String("outcome", "failed"))
		return false
	}
	span.SetAttributes(attribute.String("outcome", "deleted"))
	return true
}

func validateRegistryArtifactIdentity(payload json.RawMessage, returnedID uuid.UUID) error {
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil || request.ArtifactID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_requested_artifact_id_invalid"}
	}
	// Registry may update bytes and job state, but it cannot replace the stable
	// identity Engine authorized and placed in the generation request.
	if returnedID != request.ArtifactID {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_artifact_id_mismatch"}
	}
	return nil
}

// existingArtifactIDForApply looks up the SDK/MCP's currently-active resource ID
// (if any) so regeneration reuses the same identity instead of minting a new
// one on every apply -- split out only to keep generateSDKForApply's own
// branching under the complexity budget.
func existingArtifactIDForApply(ctx context.Context, configStore store.ConfigRepository, configKey string) uuid.UUID {
	currentState, _ := configStore.GetConfigState(ctx, configKey)
	if currentState == nil || currentState.LatestResourceID == nil {
		return uuid.UUID{}
	}
	return *currentState.LatestResourceID
}

const (
	sdkGenerationApplyTimeout = 2 * time.Minute
)

func awaitSDKGenerationCompletion(
	ctx context.Context,
	proxy Forwarder,
	apiKey string,
	initial sdkGenerationResult,
) (sdkGenerationResult, error) {
	if initial.Status != models.SDKGenerationStatusPending {
		return initial, nil
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.sdk_generation.await")
	defer span.End()
	span.SetAttributes(
		attribute.String("artifact_id", initial.ArtifactID.String()),
		attribute.String("job_id", initial.JobID),
		attribute.String("initial_status", initial.Status),
	)
	if strings.TrimSpace(initial.JobID) == "" {
		span.SetStatus(codes.Error, "sdk_job_id_required")
		span.SetAttributes(attribute.String("outcome", "missing_job_id"))
		return sdkGenerationResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_job_id_required"}
	}
	if err := waitForSDKGenerationEvent(ctx, proxy, apiKey, initial.JobID); err != nil {
		span.SetStatus(codes.Error, "sdk_generation_stream_failed")
		span.SetAttributes(attribute.String("outcome", "stream_failed"))
		return sdkGenerationResult{}, err
	}
	span.SetAttributes(attribute.String("outcome", "complete"))
	initial.Status = models.SDKGenerationStatusComplete
	return initial, nil
}

func artifactInjections(injections []InjectionConfig) []models.SDKInjectionConfig {
	if len(injections) == 0 {
		return nil
	}
	out := make([]models.SDKInjectionConfig, len(injections))
	for i, inj := range injections {
		out[i] = models.SDKInjectionConfig{
			Location: inj.Location,
			Name:     inj.Name,
			Value:    inj.Value,
		}
	}
	return out
}

type sdkGenerationStreamEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func waitForSDKGenerationEvent(ctx context.Context, proxy Forwarder, apiKey, jobID string) error {
	streamCtx, cancel := context.WithTimeout(ctx, sdkGenerationApplyTimeout)
	defer cancel()
	proxyReq, err := http.NewRequestWithContext(streamCtx, "GET", "/sdks/job/"+jobID+"/stream", nil)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to create sdk generation stream request"}
	}
	proxyReq.Header.Set("Accept", "text/event-stream")
	proxyReq.Header.Set("X-API-Key", apiKey)

	recorder := httptest.NewRecorder()
	proxy.Forward(recorder, proxyReq, "")
	if recorder.Code >= 400 {
		return sdkProxyError{status: recorder.Code, body: recorder.Body.Bytes()}
	}
	return terminalSDKGenerationEvent(recorder.Body.Bytes())
}

func terminalSDKGenerationEvent(body []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		event, ok := parseSDKGenerationEventLine(scanner.Bytes())
		if !ok {
			continue
		}
		switch event.Type {
		case "complete", "auth_key_generated":
			return nil
		case "error":
			// Registry-controlled event text may echo generated configuration or
			// secrets. Keep both the HTTP error and OTEL status fixed and safe.
			return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_failed"}
		}
	}
	if err := scanner.Err(); err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to read sdk generation stream"}
	}
	return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_pending"}
}

func parseSDKGenerationEventLine(line []byte) (sdkGenerationStreamEvent, bool) {
	var event sdkGenerationStreamEvent
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return event, false
	}
	data := bytes.TrimPrefix(line, []byte("data: "))
	if err := json.Unmarshal(data, &event); err != nil {
		return sdkGenerationStreamEvent{}, false
	}
	return event, true
}

func sdkGenerationPayloadForPlan(payload json.RawMessage, call sdkApplyCall, existingArtifactID uuid.UUID) (json.RawMessage, error) {
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid sdk generation payload"}
	}
	request.IdempotencyKey = call.planID.String()
	if existingArtifactID != uuid.Nil {
		request.ArtifactID = existingArtifactID
	} else {
		request.ArtifactID = stableArtifactIDForPlan(call.planID)
	}
	out, _ := json.Marshal(request)
	return out, nil
}

// stableArtifactIDForPlan derives a deterministic Artifact ID from planID alone --
// Engine is mono-workspace, so there is no separate workspace dimension left
// to fold into the hash for cross-workspace disambiguation.
func stableArtifactIDForPlan(planID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("sdk-config:"+planID.String()))
}

func resolveSDKContractBindings(ctx context.Context, registryClient sandbox.RegistryClient, apiKey string, services []sdkResolvedService) ([]sdkContractBinding, error) {
	if len(services) == 0 {
		return nil, nil
	}
	if registryClient == nil {
		return nil, errors.New("contract_revision_unavailable")
	}
	return resolveSDKContractBindingsBatch(ctx, registryClient, apiKey, services)
}

func resolveSDKContractBindingsBatch(ctx context.Context, resolver sandbox.RegistryClient, apiKey string, services []sdkResolvedService) ([]sdkContractBinding, error) {
	revisions, err := resolver.FetchServiceVersionRevisions(ctx, sdkServiceVersionRefs(services), apiKey)
	if err != nil {
		return nil, err
	}
	byService := sdkRevisionMap(revisions)
	bindings := make([]sdkContractBinding, 0, len(services))
	for _, service := range services {
		revision, ok := byService[service.ServiceID]
		if !ok || revision.ServiceVersionID == uuid.Nil {
			return nil, fmt.Errorf("service version %s for service %s was not resolved", service.Version, service.ServiceID)
		}
		bindings = append(bindings, sdkBindingFromRevision(revision))
	}
	return bindings, nil
}

func sdkServiceVersionRefs(services []sdkResolvedService) []sandbox.ServiceVersionRef {
	refs := make([]sandbox.ServiceVersionRef, 0, len(services))
	for _, service := range services {
		refs = append(refs, sandbox.ServiceVersionRef{ServiceID: service.ServiceID, Version: service.Version})
	}
	return refs
}

func sdkBindingFromRevision(revision sandbox.ServiceVersionRevision) sdkContractBinding {
	return sdkContractBinding{
		ServiceID:        revision.ServiceID,
		Version:          revision.Version,
		ServiceVersionID: revision.ServiceVersionID,
		Revision:         revision.Revision,
		SourceHash:       revision.SourceHash,
	}
}

func attachSDKServiceVersionIDs(selections []models.SDKSelection, bindings []sdkContractBinding) []models.SDKSelection {
	byService := sdkBindingMap(bindings)
	for i := range selections {
		if binding, ok := byService[selections[i].ServiceID]; ok && binding.ServiceVersionID != uuid.Nil {
			selections[i].ServiceVersionID = binding.ServiceVersionID
		}
	}
	return selections
}

func sdkContractBindingsFromPayload(payload json.RawMessage) ([]sdkContractBinding, error) {
	resolved, err := artifactPayloadFromJSON(payload)
	if err != nil {
		return nil, err
	}
	return resolved.ContractBindings, nil
}

func artifactPayloadFromJSON(payload json.RawMessage) (artifactResolvedPayload, error) {
	var resolved artifactResolvedPayload
	if err := json.Unmarshal(payload, &resolved); err != nil {
		return artifactResolvedPayload{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid artifact resolved payload"}
	}
	return resolved, nil
}

func ensureSDKContractBindingsCurrent(ctx context.Context, registryClient sandbox.RegistryClient, apiKey string, bindings []sdkContractBinding) error {
	if len(bindings) == 0 {
		return errors.New("contract_revision_unavailable")
	}
	if registryClient == nil {
		return errors.New("contract_revision_unavailable")
	}
	return ensureSDKContractBindingsCurrentBatch(ctx, registryClient, apiKey, bindings)
}

func ensureSDKContractBindingsCurrentBatch(ctx context.Context, resolver sandbox.RegistryClient, apiKey string, bindings []sdkContractBinding) error {
	current, err := resolver.FetchServiceVersionRevisions(ctx, sdkBindingVersionRefs(bindings), apiKey)
	if err != nil {
		return errors.New("contract_revision_unavailable")
	}
	currentByService := sdkRevisionMap(current)
	for _, binding := range bindings {
		revision, ok := currentByService[binding.ServiceID]
		if !ok || !sdkRevisionMatchesBinding(revision, binding) {
			return errors.New("contract_revision_stale")
		}
	}
	return nil
}

func sdkBindingVersionRefs(bindings []sdkContractBinding) []sandbox.ServiceVersionRef {
	refs := make([]sandbox.ServiceVersionRef, 0, len(bindings))
	for _, binding := range bindings {
		refs = append(refs, sandbox.ServiceVersionRef{ServiceID: binding.ServiceID, Version: binding.Version})
	}
	return refs
}

func sdkRevisionMap(revisions []sandbox.ServiceVersionRevision) map[uuid.UUID]sandbox.ServiceVersionRevision {
	out := make(map[uuid.UUID]sandbox.ServiceVersionRevision, len(revisions))
	for _, revision := range revisions {
		out[revision.ServiceID] = revision
	}
	return out
}

func sdkRevisionMatchesBinding(current sandbox.ServiceVersionRevision, binding sdkContractBinding) bool {
	return current.ServiceVersionID == binding.ServiceVersionID &&
		current.Revision == binding.Revision &&
		current.SourceHash == binding.SourceHash
}

func loadSDKPlanForApply(ctx context.Context, configStore store.ConfigRepository, call sdkApplyCall) (*store.ConfigPlan, *store.ConfigState, error) {
	plan, err := configStore.GetConfigPlan(ctx, call.planID)
	if err != nil {
		return nil, nil, planFetchHTTPError(err)
	}
	if err := validateSDKPlanForApply(plan, call.sourceHash); err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
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

func validateSDKPlanForApply(plan *store.ConfigPlan, sourceHash string) error {
	if plan.Status == store.ConfigPlanStatusSuperseded {
		return errors.New("plan_superseded")
	}
	if plan.Status != store.ConfigPlanStatusPending {
		return errors.New("plan_stale")
	}
	if plan.ConfigType != store.ConfigTypeSDK {
		return errors.New("plan_type_mismatch")
	}
	if sourceHash != "" && sourceHash != plan.SourceHash {
		return errors.New("source_hash_mismatch")
	}
	return nil
}

func ensureSDKSelectionsStillAllowed(ctx context.Context, s store.Store, payload json.RawMessage) error {
	resolved, err := artifactPayloadFromJSON(payload)
	if err != nil {
		return err
	}
	bindings := sdkBindingMap(resolved.ContractBindings)
	for _, selection := range resolved.Selections {
		if err := ensurePinnedSDKSelection(selection, bindings[selection.ServiceID]); err != nil {
			return err
		}
	}
	allowedVersions, err := s.ListWorkspaceServiceVersionsForServices(ctx, sdkSelectionServiceIDs(resolved.Selections))
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list allowed versions"}
	}
	for _, selection := range resolved.Selections {
		if err := ensurePinnedSDKSelectionAllowed(selection, bindings[selection.ServiceID], allowedVersions[selection.ServiceID]); err != nil {
			return err
		}
	}
	return nil
}

func ensurePinnedSDKSelection(selection models.SDKSelection, binding sdkContractBinding) error {
	if selection.ServiceVersionID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf("service %s is missing service_version_id", selection.ServiceID)}
	}
	if binding.ServiceID == uuid.Nil || binding.ServiceVersionID != selection.ServiceVersionID {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf("service %s has an invalid pinned SDK version", selection.ServiceID)}
	}
	return nil
}

func ensurePinnedSDKSelectionAllowed(selection models.SDKSelection, binding sdkContractBinding, allowed []store.WorkspaceServiceVersion) error {
	if selection.ServiceVersionID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf("service %s is missing service_version_id", selection.ServiceID)}
	}
	if !activationVersionExistsByUUID(allowed, selection.ServiceVersionID) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf("version %s for service %s is no longer allowed in this workspace", binding.Version, selection.ServiceID)}
	}
	return nil
}

func sdkSelectionServiceIDs(selections []models.SDKSelection) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(selections))
	seen := map[uuid.UUID]bool{}
	for _, selection := range selections {
		if !seen[selection.ServiceID] {
			ids = append(ids, selection.ServiceID)
			seen[selection.ServiceID] = true
		}
	}
	return ids
}

func sdkBindingMap(bindings []sdkContractBinding) map[uuid.UUID]sdkContractBinding {
	out := make(map[uuid.UUID]sdkContractBinding, len(bindings))
	for _, binding := range bindings {
		out[binding.ServiceID] = binding
	}
	return out
}

func runSDKGeneration(ctx context.Context, proxy Forwarder, apiKey string, payload json.RawMessage) (sdkGenerationResult, error) {
	proxyReq, err := http.NewRequestWithContext(ctx, "POST", "/sdks/generate", bytes.NewReader(payload))
	if err != nil {
		return sdkGenerationResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to create internal generation request"}
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("X-API-Key", apiKey)

	recorder := httptest.NewRecorder()
	proxy.Forward(recorder, proxyReq, "")
	bodyBytes, _ := io.ReadAll(recorder.Result().Body)
	if recorder.Code >= 400 {
		// Registry errors may echo user configuration, including secret values.
		// Record fixed metadata only; the typed response still reaches the caller.
		slog.ErrorContext(ctx, "SDK generation proxy failed", slog.Int("status", recorder.Code), slog.Int("response_bytes", len(bodyBytes)))
		return sdkGenerationResult{}, sdkProxyError{status: recorder.Code, body: bodyBytes}
	}
	return parseSDKGenerationResponse(ctx, bodyBytes)
}

func parseSDKGenerationResponse(ctx context.Context, bodyBytes []byte) (sdkGenerationResult, error) {
	var result sdkGenerationResult
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		slog.ErrorContext(ctx, "failed to parse SDK generation response", slog.Any("error", err))
		return result, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to parse generation response"}
	}
	if result.ArtifactID == uuid.Nil {
		return result, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "invalid sdk ID returned from generator"}
	}
	return result, nil
}

func resolveArtifactBucket(ctx context.Context, s store.Store, bucketName string) (*store.Bucket, error) {
	bucketName = strings.TrimSpace(bucketName)
	if bucketName == "" {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "artifact config requires exactly one bucket"}
	}
	b, err := s.GetBucketByName(ctx, bucketName)
	if err != nil || b == nil || b.ID == uuid.Nil {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bucket not found: " + bucketName}
	}
	return b, nil
}

func validateArtifactBucketIdentity(ctx context.Context, s store.Store, bucketName string, authorizedBucketID uuid.UUID) error {
	bucket, err := resolveArtifactBucket(ctx, s, bucketName)
	if err != nil || authorizedBucketID == uuid.Nil || bucket.ID != authorizedBucketID {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact bucket identity changed; create a new plan"}
	}
	return nil
}

// validateArtifactBucketReadiness checks the one bucket selected by an
// artifact before a plan is persisted and again during apply. OAuth/OIDC
// selections need an enabled client application in that bucket; static
// schemes intentionally remain bucket-secret managed and are resolved at
// dispatch. One bucket-scoped read reports every missing OAuth/OIDC material
// item together rather than failing selected services one at a time.
func validateArtifactBucketReadiness(ctx context.Context, s store.Store, bucketID uuid.UUID, selections []models.SDKSelection) error {
	if bucketID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "artifact config requires exactly one bucket"}
	}
	ready, secretKeys, err := loadArtifactBucketMaterial(ctx, s, bucketID)
	if err != nil {
		return err
	}
	missing := make([]string, 0)
	for _, selection := range selections {
		missing = append(missing, missingArtifactBucketMaterial(selection, ready, secretKeys)...)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return workspaceConfigHTTPError{
		status:      http.StatusBadRequest,
		code:        "bucket_credentials_missing",
		message:     "The selected credential set is missing required authentication material.",
		category:    "validation",
		details:     map[string]any{"missing": missing},
		remediation: "Add the required credentials to the credential set and create the plan again.",
	}
}

func loadArtifactBucketMaterial(ctx context.Context, s store.Store, bucketID uuid.UUID) (map[string]bool, map[string]bool, error) {
	configs, err := s.ListConnectConfigsForBucket(ctx, bucketID)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to read bucket readiness"}
	}
	secretMetas, err := s.ListSecretMeta(ctx, bucketID)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to read bucket readiness"}
	}
	ready := make(map[string]bool, len(configs))
	for _, config := range configs {
		if config.Enabled {
			ready[config.ServiceID.String()+"\x00"+canonicalWorkspaceStaticAuthType(config.AuthType)] = true
		}
	}
	secretKeys := make(map[string]bool, len(secretMetas))
	for _, secret := range secretMetas {
		secretKeys[secret.ServiceID.String()+"\x00"+secret.KeyName] = true
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("bucket_connect_config_count", len(configs)),
		attribute.Int("bucket_secret_metadata_count", len(secretMetas)),
	)
	return ready, secretKeys, nil
}

func missingArtifactBucketMaterial(selection models.SDKSelection, ready, secretKeys map[string]bool) []string {
	authType := canonicalWorkspaceStaticAuthType(selection.AuthType)
	serviceID := selection.ServiceID.String()
	if authType == "oauth" || authType == "oidc" {
		if ready[serviceID+"\x00"+authType] {
			return nil
		}
		return []string{serviceID + " (" + authType + ")"}
	}
	missing := make([]string, 0)
	for _, key := range artifactRequiredSecretKeys(selection, authType) {
		if !secretKeys[serviceID+"\x00"+key] {
			missing = append(missing, serviceID+" ("+authType+":"+key+")")
		}
	}
	return missing
}

// artifactRequiredSecretKeys mirrors workspace auth material naming. AuthName
// is pinned during Registry policy resolution, so readiness can identify every
// static credential required without decrypting or loading its value.
func artifactRequiredSecretKeys(selection models.SDKSelection, authType string) []string {
	if authType == "" {
		// Auth-free artifacts have no bucket material prerequisite. A populated
		// auth selector is always pinned by Registry before it reaches this path.
		return nil
	}
	name := strings.TrimSpace(selection.AuthName)
	if name == "" {
		return []string{"<credential-name>"}
	}
	switch authType {
	case "basic":
		return []string{name + "_username", name + "_password"}
	case "mtls":
		return []string{name + "_cert", name + "_key"}
	case "api_key", "bearer":
		return []string{name}
	default:
		return nil
	}
}

// persistArtifactScopeParams carries what persistArtifactScope needs to save an
// ArtifactScope row and link its bucket, decoupled from *how* the scope was
// produced. The config-apply path derives these from a GenerateSDKRequest
// response (see persistGeneratedArtifactScope below); the activate handler
// (sdk_lifecycle_handlers.go) has neither a plan nor a generation result --
// it takes selections/bucket straight from the activate request body --
// so this struct only carries the values both paths actually share.
type persistArtifactScopeParams struct {
	accountID          uuid.UUID
	artifactID         uuid.UUID
	ownerSubjectID     uuid.UUID
	ownerTeamID        uuid.UUID
	bucketID           uuid.UUID
	bucketName         string
	selections         json.RawMessage
	scopeSchemaVersion int
	// kind/name label the scope for the MCP servers list page
	// (store.ArtifactScope.Kind/Name) -- see persistArtifactScope below.
	kind           string
	name           string
	version        string
	configKey      string
	description    string
	targetLanguage string
	sourceHash     string
}

func artifactScopeForApply(p persistArtifactScopeParams) (store.ArtifactScope, error) {
	if p.bucketID == uuid.Nil || strings.TrimSpace(p.bucketName) == "" {
		return store.ArtifactScope{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact bucket identity unavailable"}
	}
	return store.ArtifactScope{
		AccountID:          p.accountID,
		ArtifactID:         p.artifactID,
		OwnerSubjectID:     p.ownerSubjectID,
		OwnerTeamID:        p.ownerTeamID,
		BucketID:           p.bucketID,
		Selections:         p.selections,
		ScopeSchemaVersion: p.scopeSchemaVersion,
		Kind:               p.kind,
		Name:               p.name,
		Version:            p.version,
		ConfigKey:          p.configKey,
	}, nil
}

func applyGeneratedArtifactScope(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	call sdkApplyCall,
	plan *store.ConfigPlan,
	result sdkGenerationResult,
) (string, bool, error) {
	// DesiredState is an sdkConfigDocument, not the Registry generation payload.
	// sdkConfigDocument (stateDoc, built in resolveSDKSelections), not the
	// GenerateSDKRequest shape plan.ResolvedPayload.
	var doc sdkConfigDocument
	_ = json.Unmarshal(plan.DesiredState, &doc)
	payload, err := artifactPayloadFromJSON(plan.ResolvedPayload)
	if err != nil {
		return "", false, err
	}
	selections, _ := json.Marshal(result.Selections)
	return applyArtifactConfigPlan(ctx, configStore, s, call, plan, persistArtifactScopeParams{
		accountID:          call.accountID,
		artifactID:         result.ArtifactID,
		ownerSubjectID:     planOwnerSubjectID(plan),
		ownerTeamID:        planOwnerTeamID(plan),
		bucketID:           payload.BucketID,
		bucketName:         doc.Bucket,
		selections:         selections,
		scopeSchemaVersion: result.ScopeSchemaVersion,
		kind:               "sdk",
		name:               doc.Name,
		version:            doc.Version,
		configKey:          fmt.Sprintf("sdk:%s:%s", doc.Name, doc.Version),
		description:        payload.Description,
		targetLanguage:     payload.TargetLanguage,
		sourceHash:         plan.SourceHash,
	})
}

func applyArtifactConfigPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, plan *store.ConfigPlan, params persistArtifactScopeParams) (string, bool, error) {
	scope, err := artifactScopeForApply(params)
	if err != nil {
		return "", false, err
	}
	snapshot := store.ArtifactSnapshot{ArtifactID: params.artifactID, AccountID: params.accountID,
		Kind: params.kind, Name: params.name, Description: params.description, Version: params.version,
		TargetLanguage: params.targetLanguage, Selections: params.selections,
		ScopeSchemaVersion: params.scopeSchemaVersion, SourceHash: params.sourceHash}
	return applyArtifactConfigScope(ctx, configStore, s, call, plan, scope, snapshot, params.bucketName)
}

func applyArtifactConfigScope(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, plan *store.ConfigPlan, scope store.ArtifactScope, snapshot store.ArtifactSnapshot, authorizedBucketName string) (string, bool, error) {
	rawToken, tokenHash, err := newSDKExecutionCredential()
	if err != nil {
		return "", false, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to issue sdk execution credential"}
	}
	result, err := configStore.ApplyArtifactConfigPlan(ctx, store.ApplyArtifactConfigPlanParams{
		Plan: store.ApplyConfigPlanParams{
			State: store.UpsertConfigStateParams{
				ConfigKey: plan.ConfigKey, ConfigType: plan.ConfigType, SourceHash: plan.SourceHash,
				DesiredState: plan.DesiredState, ManagedResources: []byte("{}"), LatestResourceID: &scope.ArtifactID, UpdatedBy: call.accountID,
			},
			PlanID: call.planID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: call.planRevision,
			ApplyLeaseID: call.applyLeaseID,
		},
		Scope: scope, Snapshot: snapshot, AuthorizedBucketName: authorizedBucketName, TokenHash: tokenHash, TokenName: "default", Activate: true,
	})
	if err != nil {
		return "", false, artifactApplyPersistenceError(ctx, err, scope.ArtifactID)
	}
	notifyArtifactScopeChanged(ctx, s, scope.ArtifactID)
	if !result.ScopeCreated {
		return "", false, nil
	}
	return rawToken, true, nil
}

type artifactScopeChangeNotifier interface {
	NotifyArtifactScopeChanged(context.Context, uuid.UUID)
}

func notifyArtifactScopeChanged(ctx context.Context, s store.Store, artifactID uuid.UUID) {
	if notifier, ok := s.(artifactScopeChangeNotifier); ok {
		notifier.NotifyArtifactScopeChanged(ctx, artifactID)
	}
}

func artifactApplyPersistenceError(ctx context.Context, err error, artifactID uuid.UUID) error {
	slog.ErrorContext(ctx, "artifact config apply persistence failed", slog.Any("error", err), slog.String("artifact_id", artifactID.String()))
	if errors.Is(err, store.ErrSDKBucketImmutable) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact bucket assignment is immutable"}
	}
	if errors.Is(err, store.ErrConfigPlanNotFound) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale_or_mismatched"}
	}
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply artifact config"}
}

func planOwnerTeamID(plan *store.ConfigPlan) uuid.UUID {
	if plan == nil || plan.OwnerTeamID == nil {
		return uuid.Nil
	}
	return *plan.OwnerTeamID
}

func planOwnerSubjectID(plan *store.ConfigPlan) uuid.UUID {
	if plan == nil || plan.OwnerSubjectID == nil {
		return uuid.Nil
	}
	return *plan.OwnerSubjectID
}

func planOwnerType(plan *store.ConfigPlan) string {
	if plan != nil && plan.OwnerTeamID != nil {
		return "team"
	}
	return "subject"
}

func validateSDKGenerationResult(payload json.RawMessage, call sdkApplyCall, result models.SDKGenerationResult) error {
	if result.ArtifactID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "artifact_id_required"}
	}
	if strings.TrimSpace(result.JobID) == "" {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_job_id_required"}
	}
	if result.AccountID == uuid.Nil || result.AccountID != call.accountID {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_account_mismatch"}
	}
	if result.Status == models.SDKGenerationStatusFailed {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_failed"}
	}
	if result.Status != models.SDKGenerationStatusPending && result.Status != models.SDKGenerationStatusComplete {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_status_invalid"}
	}
	if result.ScopeSchemaVersion != models.ArtifactScopeSchemaVersion {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_schema_version_mismatch"}
	}
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid sdk generation payload"}
	}
	return validateGeneratedScopeSelections(request.Selections, result.Selections)
}

func validateGeneratedScopeSelections(planned []models.SDKSelection, returned []models.SDKSelection) error {
	if len(planned) != len(returned) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	plannedByService, err := sdkSelectionMap(planned)
	if err != nil {
		return err
	}
	seenReturned := map[uuid.UUID]bool{}
	for _, selection := range returned {
		if seenReturned[selection.ServiceID] {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
		}
		seenReturned[selection.ServiceID] = true
		plannedSelection, ok := plannedByService[selection.ServiceID]
		if !ok || !sameSDKServiceVersion(plannedSelection, selection) {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
		}
		if err := validateConcreteReturnedSelection(plannedSelection, selection); err != nil {
			return err
		}
	}
	return nil
}

func validateConcreteReturnedSelection(planned, returned models.SDKSelection) error {
	if returned.SelectAll || len(returned.OperationNames) > 0 || len(returned.EndpointIDs)+len(returned.WebhookIDs) == 0 {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	if hasDuplicateUUID(returned.EndpointIDs) || hasDuplicateUUID(returned.WebhookIDs) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	if len(planned.EndpointIDs) > 0 && !sameUUIDSet(planned.EndpointIDs, returned.EndpointIDs) {
		return endpointSelectionMismatch(planned, returned)
	}
	if len(planned.WebhookIDs) > 0 && !sameUUIDSet(planned.WebhookIDs, returned.WebhookIDs) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	return nil
}

func endpointSelectionMismatch(planned, returned models.SDKSelection) error {
	returnedIDs := make(map[uuid.UUID]bool, len(returned.EndpointIDs))
	for _, id := range returned.EndpointIDs {
		returnedIDs[id] = true
	}
	missing := make([]string, 0)
	required := make([]string, 0, len(planned.EndpointIDs))
	for _, id := range planned.EndpointIDs {
		required = append(required, id.String())
		if !returnedIDs[id] {
			missing = append(missing, id.String())
		}
	}
	return sdkScopeSelectionMismatchError{Detail: struct {
		Reason         string
		ServiceID      uuid.UUID
		MissingScopes  []string
		RequiredScopes []string
	}{Reason: "endpoint_id_drift", ServiceID: planned.ServiceID, MissingScopes: missing, RequiredScopes: required}}
}

type sdkScopeSelectionMismatchError struct {
	Detail struct {
		Reason         string
		ServiceID      uuid.UUID
		MissingScopes  []string
		RequiredScopes []string
	}
}

func (e sdkScopeSelectionMismatchError) Error() string {
	return "sdk_scope_selection_mismatch"
}

func (e sdkScopeSelectionMismatchError) Unwrap() error {
	return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
}

func hasDuplicateUUID(ids []uuid.UUID) bool {
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			return true
		}
		seen[id] = true
	}
	return false
}

func sameUUIDSet(expected, actual []uuid.UUID) bool {
	if len(expected) != len(actual) {
		return false
	}
	expectedSet := make(map[uuid.UUID]bool, len(expected))
	for _, id := range expected {
		if id == uuid.Nil || expectedSet[id] {
			return false
		}
		expectedSet[id] = true
	}
	for _, id := range actual {
		if !expectedSet[id] {
			return false
		}
	}
	return true
}

func sdkSelectionMap(selections []models.SDKSelection) (map[uuid.UUID]models.SDKSelection, error) {
	out := make(map[uuid.UUID]models.SDKSelection, len(selections))
	for _, selection := range selections {
		if selection.ServiceID == uuid.Nil {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
		}
		if _, exists := out[selection.ServiceID]; exists {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
		}
		out[selection.ServiceID] = selection
	}
	return out, nil
}

func sameSDKServiceVersion(a, b models.SDKSelection) bool {
	if a.ServiceVersionID == uuid.Nil || b.ServiceVersionID == uuid.Nil {
		return false
	}
	return a.ServiceVersionID == b.ServiceVersionID
}

func newSDKExecutionCredential() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := "fused_sdk_" + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(hash[:]), nil
}

func writeSDKConfigError(w http.ResponseWriter, err error, contexts ...context.Context) {
	if isArtifactAuthorizationError(err) {
		accesscontrol.WriteAuthorizationError(w, err)
		return
	}
	var proxyErr sdkProxyError
	if errors.As(err, &proxyErr) {
		writeWorkspaceConfigError(w, workspaceConfigHTTPError{
			status:    proxyErr.status,
			code:      "registry_request_failed",
			message:   "The Registry could not complete SDK generation.",
			category:  "dependency",
			retryable: proxyErr.status >= http.StatusInternalServerError,
			details:   map[string]any{"http_status": proxyErr.status},
		}, contexts...)
		return
	}
	writeWorkspaceConfigError(w, err, contexts...)
}

// SDKConfigDownloadHandler handles GET /sdk-config/{name}/download.
func SDKConfigDownloadHandler(configStore store.ConfigRepository, s store.Store, proxy Forwarder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_config.download")
		defer span.End()

		_, err := resolveWorkspaceActor(ctx)
		if err != nil {
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}

		pathParts := strings.Split(r.URL.Path, "/")
		name := pathParts[len(pathParts)-2] // /sdk-config/{name}/download

		configKey := "sdk:" + name
		if strings.HasPrefix(name, "sdk:") {
			configKey = name
		}

		state, err := configStore.GetConfigState(ctx, configKey)
		if err != nil || state == nil {
			http.Error(w, `{"error":"sdk config not found"}`, http.StatusNotFound)
			return
		}

		if state.LatestResourceID == nil || *state.LatestResourceID == uuid.Nil {
			http.Error(w, `{"error":"no generated SDK found for this config"}`, http.StatusNotFound)
			return
		}
		if err := accesscontrol.AuthorizeAll(ctx, accesscontrol.SnapshotAuthorizer{}, accesscontrol.Requirement{
			Permission: accesscontrol.PermissionArtifactRead,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *state.LatestResourceID},
		}); err != nil {
			accesscontrol.WriteAuthorizationError(w, err)
			return
		}
		if err := ensureSDKDownloadAvailable(ctx, s, state.DesiredState); err != nil {
			writeSDKConfigError(w, err)
			return
		}

		proxyReq, err := http.NewRequestWithContext(ctx, "GET", "/sdks/"+state.LatestResourceID.String()+"/download", nil)
		if err != nil {
			http.Error(w, `{"error":"failed to create internal download request"}`, http.StatusInternalServerError)
			return
		}
		proxyReq.Header.Set("X-API-Key", r.Header.Get("X-API-Key"))

		proxy.Forward(w, proxyReq, "")
	}
}

func ensureSDKDownloadAvailable(ctx context.Context, s store.Store, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var doc sdkConfigDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid sdk config state"}
	}
	services, err := workspaceServicesByName(ctx, s)
	if err != nil {
		return err
	}
	// Same batching as resolveSDKSelections: one lookup for every service
	// this stored config references, not one per service in the loop below.
	allowedVersions, err := s.ListWorkspaceServiceVersionsForServices(ctx, sdkReferencedServiceIDs(doc, services))
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to list allowed versions"}
	}
	for serviceName, serviceDoc := range doc.Services {
		activation, ok := services[serviceName]
		if !ok {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: fmt.Sprintf("service %s is no longer allowed in this workspace", serviceName)}
		}
		if _, err := resolveSDKVersionAllowed(activation, serviceDoc.Version, serviceName, allowedVersions[activation.ServiceID]); err != nil {
			return workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
		}
	}
	return nil
}
