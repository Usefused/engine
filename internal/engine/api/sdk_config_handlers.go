package api

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/Usefused/engine/internal/engine/applifecycle"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
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
	// Generate is tri-state on purpose: absent means the historical default of
	// building a package. Only an explicit false suppresses codegen, leaving a
	// published app version that is reachable over REST execution and
	// describable via sdk openapi, with no downloadable artifact behind it.
	// Mirrors cli/internal/configfile's app config generate field.
	Generate *bool `json:"generate,omitempty"`
	// WebhookAttachment names one kind: webhook config this SDK/MCP wants
	// event delivery from -- mirrors cli/internal/configfile's
	// app config webhook_attachment field-for-field (same yaml/json key),
	// since this struct is the Engine-side decode target for that exact wire
	// document. Required (validated in validateSDKConfigDocument) whenever
	// any service below selects webhooks. The gRPC subscription resolves this
	// from the exact fused_apps row to scope FilterSubjects to this app's
	// registrations. See plans/plan-webhook-kind.md.
	WebhookAttachment string                            `json:"webhook_attachment,omitempty"`
	Services          map[string]sdkConfigServiceDoc    `json:"services"`
	UnifiedOperations map[string]sdkUnifiedOperationDoc `json:"unified_operations,omitempty"`
}

const maxAppVersionLength = 128

var appVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

const appServerVariableLocation = "server_variable"

type sdkConfigServiceDoc struct {
	Version    string   `json:"version"`
	Operations []string `json:"operations"`
	Webhooks   []string `json:"webhooks,omitempty"`
	SelectAll  bool     `json:"select_all,omitempty"`
	// WebhooksSelectAll is the webhook-only counterpart to SelectAll --
	// mirrors app service webhooks_select_all (same key) so a service can
	// select every webhook event independent of whether it also selects
	// every operation. Threaded into models.SDKSelection.WebhookSelectAll at
	// resolution time; see that field's doc comment for how the Registry
	// generator treats it.
	WebhooksSelectAll bool              `json:"webhooks_select_all,omitempty"`
	Auth              *sdkAppAuthDoc    `json:"auth,omitempty"`
	Connect           *sdkAppConnectDoc `json:"connect,omitempty"`
	Injections        []InjectionConfig `json:"injections,omitempty"`
}

type sdkAppAuthDoc struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type sdkAppConnectDoc struct {
	Scopes []string `json:"scopes,omitempty"`
}

type GenerateSDKRequest = models.SDKGenerationRequest

type sdkContractBinding = models.SDKContractBinding

// appResolvedPayload is the shared, generation-free record of resolved
// selections used by both SDK and MCP config apply. SDK generation adds its
// Registry-specific fields separately, while MCP never carries a target.
type appResolvedPayload struct {
	AppID                          uuid.UUID                              `json:"app_id,omitempty"`
	Noop                           bool                                   `json:"noop,omitempty"`
	BucketID                       uuid.UUID                              `json:"bucket_id"`
	Name                           string                                 `json:"name,omitempty"`
	Description                    string                                 `json:"description,omitempty"`
	Version                        string                                 `json:"version,omitempty"`
	Selections                     []models.SDKSelection                  `json:"selections"`
	IncludeMCP                     bool                                   `json:"include_mcp,omitempty"`
	TargetType                     string                                 `json:"target_type,omitempty"`
	TargetLanguage                 string                                 `json:"target_language,omitempty"`
	DefaultEngineURL               string                                 `json:"default_engine_url,omitempty"`
	SkipSandbox                    bool                                   `json:"skip_sandbox,omitempty"`
	SkipPackaging                  bool                                   `json:"skip_packaging,omitempty"`
	ContractBindings               []sdkContractBinding                   `json:"contract_bindings,omitempty"`
	UnifiedDefinitionSchemaVersion int                                    `json:"unified_definition_schema_version,omitempty"`
	UnifiedDefinitions             json.RawMessage                        `json:"unified_definitions,omitempty"`
	UnifiedDefinitionHash          string                                 `json:"unified_definition_hash,omitempty"`
	UnifiedCodegenDescriptorHash   string                                 `json:"unified_codegen_descriptor_hash,omitempty"`
	UnifiedOperations              *models.SDKUnifiedOperationDescriptors `json:"unified_operations,omitempty"`
}

type sdkPlanCall struct {
	apiKey           string
	accountID        uuid.UUID
	actor            accesscontrol.Actor
	request          SDKConfigPlanRequest
	document         sdkConfigDocument
	defaultEngineURL string
}

type sdkPlanResult struct {
	plan          *store.ConfigPlan
	summary       map[string]any
	notifications notificationInbox
}

type sdkPlanDefinition struct {
	services         []map[string]any
	resolvedServices []sdkResolvedService
	desiredState     json.RawMessage
	resolvedPayload  json.RawMessage
	noop             bool
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
	PublicTarget     string
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
func SDKConfigPlanHandler(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, defaultEngineURLs ...string) http.HandlerFunc {
	defaultEngineURL := ""
	if len(defaultEngineURLs) > 0 {
		defaultEngineURL = strings.TrimSpace(defaultEngineURLs[0])
	}
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
			apiKey:           r.Header.Get("X-API-Key"),
			accountID:        actor.AccountID,
			actor:            actor,
			request:          req,
			document:         doc,
			defaultEngineURL: defaultEngineURL,
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
			attribute.String("plan_id", planID.String()),
			attribute.String("actor.type", string(actor.Kind)),
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
			"status":        status,
			"plan_id":       planID.String(),
			"app_family_id": result.AppFamilyID.String(),
			"app_id":        result.AppID.String(),
			"job_id":        result.JobID,
		}
		if result.ExecutionToken != "" {
			resp["execution_token"] = result.ExecutionToken
			setOneTimeSecretResponseHeaders(w)
		}
		writeJSON(w, resp)
	}
}

// decodeSDKConfigPlanRequest validates the envelope and app identity
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
	if err := decodeAppConfigJSON(req.Config, &doc); err != nil {
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

// decodeAppConfigJSON rejects misspelled and obsolete fields at the
// Engine boundary; otherwise a valid-looking plan could silently omit policy.
func decodeAppConfigJSON(raw []byte, target *sdkConfigDocument) error {
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
	// The new app contract has no compatibility bridge: version identifies
	// the app directly and kind-specific routes replace target switching.
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

// validateSDKConfigDocument rejects malformed sdk config document before it can cross the Unified operation boundary.
func validateSDKConfigDocument(doc sdkConfigDocument) error {
	if err := validateSDKIdentity(doc); err != nil {
		return err
	}
	if err := validateWebhookAttachmentRequired(doc); err != nil {
		return err
	}
	if err := validateAppServiceDocs(doc.Services); err != nil {
		return err
	}
	return validateSDKUnifiedOperations(doc)
}

func validateSDKIdentity(doc sdkConfigDocument) error {
	if doc.APIVersion != "fused/v1" {
		return errors.New("config apiVersion must be fused/v1")
	}
	if doc.Kind != store.AppKindSDK.String() {
		return errors.New("config kind must be sdk")
	}
	if strings.TrimSpace(doc.Name) == "" || strings.TrimSpace(doc.Version) == "" {
		return errors.New("sdk config requires name and version")
	}
	if !validAppVersion(doc.Version) {
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

func validAppVersion(version string) bool {
	return len(version) <= maxAppVersionLength && appVersionPattern.MatchString(version)
}

// validateWebhookAttachmentRequired mirrors the CLI's own plan-time check
// CLI validation server-side: a service
// that selects webhooks with nothing named to attach them to has no
// registration identity for scoped delivery, so reject it before runtime.
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

// validateWebhookAttachmentCoverage mirrors validateAppBucketReadiness's
// pattern for webhook_attachment, the same way this app already
// requires its one bucket to exist: the named kind: webhook config must
// already exist, and must register every service this app selects
// webhooks for. Without this, a service selecting webhooks whose attached
// webhook config never registered it fails silently -- no signing secret, no
// endpoint accepting deliveries under this label, the events just never
// arrive with no error anywhere (see plans/plan-webhook-kind.md's "known
// gap" note). Checked once at plan time (resolveSDKSelections) and again at
// apply (generateSDKForApply/executeMCPConfigApply), same defense-in-depth
// as bucket readiness, since the referenced webhook config can be edited
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

// decodeAppApplyPlan decodes a stored SDK/MCP plan's desired state and
// resolved payload, then re-verifies both cross-references apply depends on
// (bucket readiness, webhook attachment coverage) still hold -- grouped into
// one call so generateSDKForApply/executeMCPConfigApply each need only one
// branch here instead of four, keeping them under the complexity budget.
// kind customizes the "invalid resolved ... plan" message only; decoding and
// validation are identical for both, so this is the one place that logic
// lives rather than duplicated per app kind.
func decodeAppApplyPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, plan *store.ConfigPlan, kind string) (sdkConfigDocument, appResolvedPayload, error) {
	var doc sdkConfigDocument
	var payload appResolvedPayload
	if json.Unmarshal(plan.DesiredState, &doc) != nil || json.Unmarshal(plan.ResolvedPayload, &payload) != nil {
		return sdkConfigDocument{}, appResolvedPayload{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid resolved " + kind + " plan"}
	}
	bucket, err := validateAppBucketIdentity(ctx, s, doc.Bucket, payload.BucketID)
	if err != nil {
		return sdkConfigDocument{}, appResolvedPayload{}, err
	}
	if err := validateAppBucketReadiness(ctx, s, *bucket, payload.Selections, nil); err != nil {
		return sdkConfigDocument{}, appResolvedPayload{}, err
	}
	if err := validateWebhookAttachmentCoverage(ctx, configStore, doc); err != nil {
		return sdkConfigDocument{}, appResolvedPayload{}, err
	}
	return doc, payload, nil
}

// webhookAttachmentServiceNames extracts just the service keys from a
// kind: webhook config's stored desired state -- the only thing coverage
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

// validateAppServiceDocs rejects empty operation surfaces and unknown
// auth types before selectors are resolved against Registry metadata.
func validateAppServiceDocs(services map[string]sdkConfigServiceDoc) error {
	if len(services) == 0 {
		return errors.New("app config requires at least one service")
	}
	for name, service := range services {
		// A service may select only webhooks (no operations at all) -- MCP
		// already rejects non-empty Webhooks/WebhooksSelectAll earlier in
		// validateAppConfigDocument, so by the time this runs for an mcp
		// document those are always empty/false and this check degrades to
		// the original operations-only gate for that kind.
		if err := validateAppServiceDoc(name, service); err != nil {
			return err
		}
	}
	return nil
}

func validateAppServiceDoc(name string, service sdkConfigServiceDoc) error {
	if len(service.Operations) == 0 && !service.SelectAll && len(service.Webhooks) == 0 && !service.WebhooksSelectAll {
		return fmt.Errorf("service %s requires at least one operation or webhook", name)
	}
	// Injection grammar must fail before any Registry or bucket lookup begins.
	if err := validateAppInjectionDocs(name, service.Injections); err != nil {
		return err
	}
	if service.Auth == nil {
		return nil
	}
	if strings.TrimSpace(service.Auth.Type) == "" {
		return fmt.Errorf("service %s auth requires type", name)
	}
	if !validAppAuthType(service.Auth.Type) {
		return fmt.Errorf("service %s auth type must be one of basic, bearer, api_key, oauth, oidc, or mtls", name)
	}
	return nil
}

// validateAppInjectionDocs reserves host-template binding for non-secret
// values from the app's own bucket and rejects ambiguous duplicate targets.
func validateAppInjectionDocs(service string, injections []InjectionConfig) error {
	serverTargets := make(map[string]struct{})
	// Other injection locations retain their established transport behavior;
	// this boundary owns only the new privileged server-template target.
	for _, injection := range injections {
		// Non-routing targets continue through their existing transport owners.
		if !strings.EqualFold(strings.TrimSpace(injection.Location), appServerVariableLocation) {
			continue
		}
		name := strings.TrimSpace(injection.Name)
		// Target names must match the canonical provider placeholder grammar.
		if !serverrouting.IsVariableName(name) {
			return fmt.Errorf("service %s server_variable injection name is invalid", service)
		}
		// URL material must not pull from secret storage because resolved hosts
		// and paths are provider routing data rather than credential transport.
		if !sandbox.IsExactNonSecretBucketReference(strings.TrimSpace(injection.Value)) {
			return fmt.Errorf("service %s server_variable injection %q requires ${bucket.env.*} or ${bucket.values.*}", service, name)
		}
		mode := strings.ToLower(strings.TrimSpace(injection.Mode))
		// Closed mode admission prevents an ignored binding from looking valid.
		if mode != "" && mode != "force" && mode != "default" {
			return fmt.Errorf("service %s server_variable injection %q mode must be force or default", service, name)
		}
		// A single app must not provide two competing values for one placeholder.
		if _, duplicate := serverTargets[name]; duplicate {
			return fmt.Errorf("service %s has duplicate server_variable injection %q", service, name)
		}
		serverTargets[name] = struct{}{}
	}
	return nil
}

// validAppAuthType keeps the public config vocabulary independent of
// provider-specific OpenAPI scheme names.
func validAppAuthType(value string) bool {
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

// setSDKConfigSpanAttributes records only bounded SDK plan counts and language metadata on the configuration span.
func setSDKConfigSpanAttributes(span trace.Span, configKey string, doc sdkConfigDocument) {
	span.SetAttributes(
		attribute.String("config_key", configKey),
		attribute.String("app.kind", doc.Kind),
		attribute.String("app.version", doc.Version),
		attribute.String("app.language", doc.Language),
		attribute.Int("app.service_count", len(doc.Services)),
		attribute.Int("app.unified_operation_count", len(doc.UnifiedOperations)),
		attribute.Int("app.unified_binding_count", unifiedBindingCount(doc.UnifiedOperations)),
	)
}

func createSDKConfigPlan(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	registryClient sandbox.RegistryClient,
	call sdkPlanCall,
) (sdkPlanResult, error) {
	currentState, appID, err := loadSDKPlanState(ctx, configStore, s, call)
	if err != nil {
		return sdkPlanResult{}, err
	}
	owner, bucket, err := resolveAppPlanOwnerAndBucket(
		ctx, s, currentState, call.actor, call.request.OwnerTeamSlug, call.document.Bucket,
	)
	if err != nil {
		return sdkPlanResult{}, err
	}
	call.request.OwnerSubjectID, call.request.OwnerTeamID = owner.subjectID, owner.teamID
	definition, err := resolveSDKPlanDefinition(ctx, configStore, s, registryClient, call, currentState, *bucket, appID)
	if err != nil {
		return sdkPlanResult{}, err
	}
	notifications := sdkPlanNotifications(ctx, configStore, registryClient, call, definition.resolvedServices, definition.noop)
	requiredPermissions, requiredCount, err := configPlanRequiredPermissionsWithBuckets(
		ctx, s, appPermissionState(currentState, appID), serviceNamesFromResolved(definition.resolvedServices), []store.Bucket{*bucket}, call.document.Name,
	)
	if err != nil {
		return sdkPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to compute required permissions"}
	}
	if err := preflightConfigOwnership(ctx, s, call.actor, owner, optionalAppID(appID), requiredPermissions); err != nil {
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
		DesiredState:        definition.desiredState,
		ResolvedPayload:     definition.resolvedPayload,
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
		summary:       sdkPlanSummary(appID == uuid.Nil, !definition.noop && appID != uuid.Nil, definition.services),
		notifications: notifications,
	}, nil
}

// resolveSDKPlanDefinition resolves sdk plan definition from immutable app scope before provider dispatch.
func resolveSDKPlanDefinition(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, call sdkPlanCall, current *store.ConfigState, bucket store.Bucket, appID uuid.UUID) (sdkPlanDefinition, error) {
	selections, services, resolvedServices, stateDoc, err := resolveSDKSelections(ctx, configStore, s, registryClient, call.apiKey, call.document, previousSDKDocument(current), bucket)
	if err != nil {
		return sdkPlanDefinition{}, err
	}
	bindings, err := resolveSDKContractBindings(ctx, registryClient, call.apiKey, resolvedServices)
	if err != nil {
		return sdkPlanDefinition{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to bind service contract revisions"}
	}
	selections = finalizeAppSelections(selections, bindings)
	unifiedCompilation, err := compileSDKUnifiedOperations(ctx, s, call.document, selections, resolvedServices)
	if err != nil {
		return sdkPlanDefinition{}, err
	}
	desiredState, _ := json.Marshal(stateDoc)
	unchanged := current != nil && sameCanonicalAppState(current.DesiredState, desiredState)
	noop, err := sdkPlanIsNoop(ctx, s, call.accountID, appID, unchanged, unifiedCompilation)
	if err != nil {
		return sdkPlanDefinition{}, err
	}
	generationRequest := sdkGenerateRequest(call.document, selections, bindings, call.defaultEngineURL)
	generationRequest.UnifiedOperations = unifiedCompilation.Descriptors
	payload := resolvedSDKPayload(generationRequest, bucket.ID, appID, noop)
	payload.UnifiedDefinitionSchemaVersion = unified.DefinitionSchemaVersion
	payload.UnifiedDefinitions = unifiedCompilation.DefinitionJSON
	payload.UnifiedDefinitionHash = unifiedCompilation.DefinitionHash
	payload.UnifiedCodegenDescriptorHash = unifiedCompilation.CodegenDescriptorHash
	resolvedPayload, _ := json.Marshal(payload)
	return sdkPlanDefinition{services: services, resolvedServices: resolvedServices, desiredState: desiredState, resolvedPayload: resolvedPayload, noop: noop}, nil
}

// sdkPlanIsNoop requires canonical desired state and compiled Unified hashes to match the existing app runtime.
func sdkPlanIsNoop(ctx context.Context, s store.Store, accountID, appID uuid.UUID, unchanged bool, compilation sdkUnifiedCompilation) (bool, error) {
	if !unchanged || appID == uuid.Nil {
		return false, nil
	}
	scope, err := s.GetAppRuntime(ctx, appID)
	if errors.Is(err, store.ErrAppRuntimeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to verify sdk runtime state"}
	}
	return scope.AccountID == accountID &&
		scope.UnifiedDefinitionSchemaVersion == unified.DefinitionSchemaVersion &&
		scope.UnifiedDefinitionHash == compilation.DefinitionHash &&
		scope.UnifiedCodegenDescriptorHash == compilation.CodegenDescriptorHash, nil
}

func appPermissionState(current *store.ConfigState, appID uuid.UUID) *store.ConfigState {
	if current != nil || appID == uuid.Nil {
		return current
	}
	// A reconciled snapshot is an existing Registry package even though its
	// Engine config state was intentionally cleared. Authorize manage on that
	// exact identity instead of treating reconfiguration as a fresh create.
	return &store.ConfigState{LatestResourceID: &appID}
}

func loadSDKPlanState(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkPlanCall) (*store.ConfigState, uuid.UUID, error) {
	current, err := configStore.GetConfigState(ctx, call.request.ConfigKey)
	if err != nil {
		return nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	appID, err := resolvedAppIDForPlan(ctx, s, call.accountID, current, call.document)
	return current, appID, err
}

func sdkPlanNotifications(ctx context.Context, configStore store.ConfigRepository, registryClient sandbox.RegistryClient, call sdkPlanCall, services []sdkResolvedService, unchanged bool) notificationInbox {
	if unchanged {
		return notificationInbox{}
	}
	return collectSDKPlanNotifications(ctx, configStore, registryClient, call, services)
}

func resolvedAppIDForPlan(ctx context.Context, s store.Store, accountID uuid.UUID, current *store.ConfigState, doc sdkConfigDocument) (uuid.UUID, error) {
	if id := existingConfigResourceID(current); id != nil {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("app.identity_source", "config_state"))
		return *id, nil
	}
	// Config state is the authoritative pointer for an Engine-owned app. The
	// Registry no longer restores SDK/MCP definitions after a database reset.
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("app.identity_source", "new"))
	return uuid.Nil, nil
}

func optionalAppID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func resolveAppPlanOwnerAndBucket(
	ctx context.Context,
	s store.Store,
	current *store.ConfigState,
	actor accesscontrol.Actor,
	requestedOwnerTeamSlug string,
	bucketName string,
) (configOwner, *store.Bucket, error) {
	owner, err := resolveConfigPlanOwner(ctx, s, current, actor, requestedOwnerTeamSlug)
	if err != nil {
		return configOwner{}, nil, err
	}
	bucket, err := resolveAppBucket(ctx, s, bucketName)
	if err != nil {
		return configOwner{}, nil, err
	}
	return owner, bucket, nil
}

// resolveSDKSelections resolves sdk selections from immutable app scope before provider dispatch.
func resolveSDKSelections(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	registryClient sandbox.RegistryClient,
	apiKey string,

	doc sdkConfigDocument,
	previous sdkConfigDocument,
	bucket store.Bucket,
) ([]models.SDKSelection, []map[string]any, []sdkResolvedService, sdkConfigDocument, error) {
	doc = canonicalAppDocument(doc)
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
			AuthType:         appAuthType(serviceDoc.Auth),
			AuthName:         appAuthName(serviceDoc.Auth),
			ConnectScopes:    appConnectScopes(serviceDoc.Connect),
			Injections:       appInjections(serviceDoc.Injections),
		})
		// Applied state is keyed by Registry display name, while a hand-authored
		// config may use its stable slug. Compare through the resolved identity so
		// a second plan does not report every operation as newly added.
		summary = append(summary, sdkServiceSummary(activation.ServiceName, serviceDoc, previous.Services[activation.ServiceName].Operations))
		resolved = append(resolved, sdkResolvedService{
			ServiceID: activation.ServiceID, ServiceVersionID: resolvedServiceVersionID,
			Version: resolvedVersionStr, ServiceName: activation.ServiceName, PublicTarget: serviceName,
		})
		stateDoc.Services[activation.ServiceName] = serviceDoc
	}
	if err := validateResolvedSDKSelections(ctx, configStore, s, registryClient, apiKey, bucket, doc, resolved, selections); err != nil {
		return nil, nil, nil, sdkConfigDocument{}, err
	}

	return selections, summary, resolved, stateDoc, nil
}

// validateResolvedSDKSelections rejects malformed resolved sdk selections before it can cross the Unified operation boundary.
func validateResolvedSDKSelections(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, apiKey string, bucket store.Bucket, doc sdkConfigDocument, resolved []sdkResolvedService, selections []models.SDKSelection) error {
	// public target uniqueness becomes execution authority only when Unified
	// bindings can address those targets; ordinary SDK services keep legacy names.
	if len(doc.UnifiedOperations) > 0 {
		if err := validateUniqueUnifiedTargets(resolved); err != nil {
			return err
		}
	}
	if err := resolveAppAuthPolicies(ctx, registryClient, apiKey, resolved, selections); err != nil {
		return err
	}
	if err := validateAppBucketReadiness(ctx, s, bucket, selections, appReadinessServiceNames(resolved)); err != nil {
		return err
	}
	if err := validateWebhookAttachmentCoverage(ctx, configStore, doc); err != nil {
		return err
	}
	if registryClient == nil {
		return nil
	}
	if err := registryClient.ValidateSDKSelections(ctx, selections); err != nil {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	return nil
}

// canonicalAppDocument removes presentation-only ordering differences
// before an app is resolved, hashed, or persisted as immutable state.
func canonicalAppDocument(doc sdkConfigDocument) sdkConfigDocument {
	canonical := doc
	canonical.APIVersion = strings.TrimSpace(doc.APIVersion)
	canonical.Kind = strings.TrimSpace(doc.Kind)
	canonical.Name = strings.TrimSpace(doc.Name)
	canonical.Version = strings.TrimSpace(doc.Version)
	canonical.Language = strings.TrimSpace(doc.Language)
	canonical.Bucket = strings.TrimSpace(doc.Bucket)
	canonical.WebhookAttachment = strings.TrimSpace(doc.WebhookAttachment)
	canonical.UnifiedOperations = canonicalizeUnifiedOperations(doc.UnifiedOperations)
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
		service.Injections = canonicalAppInjections(service.Injections)
		canonical.Services[strings.TrimSpace(name)] = service
	}
	return canonical
}

// canonicalAppInjections gives server-variable bindings deterministic mode and
// casing while preserving order for existing request-transport injections.
func canonicalAppInjections(injections []InjectionConfig) []InjectionConfig {
	canonical := append([]InjectionConfig(nil), injections...)
	// Order remains meaningful for legacy transport targets, so normalization
	// changes fields in place instead of sorting the slice.
	for index := range canonical {
		// Existing transport targets remain byte-for-byte unchanged because their
		// names and values may be case- or whitespace-sensitive.
		if !strings.EqualFold(strings.TrimSpace(canonical[index].Location), appServerVariableLocation) {
			continue
		}
		canonical[index].Location = strings.ToLower(strings.TrimSpace(canonical[index].Location))
		canonical[index].Name = strings.TrimSpace(canonical[index].Name)
		canonical[index].Value = strings.TrimSpace(canonical[index].Value)
		canonical[index].Mode = strings.ToLower(strings.TrimSpace(canonical[index].Mode))
		// A server variable is app-owned routing input, so omission means the same
		// forced binding documented for this target rather than no action.
		if canonical[index].Mode == "" {
			canonical[index].Mode = "force"
		}
	}
	return canonical
}

// canonicalAppState serializes the canonical desired state so identity
// comparisons ignore YAML/JSON ordering and harmless whitespace changes.
func canonicalAppState(doc sdkConfigDocument) ([]byte, error) {
	return json.Marshal(canonicalAppDocument(doc))
}

// appAuthType normalizes only the public type selector; exact scheme
// resolution remains Registry-owned.
func appAuthType(auth *sdkAppAuthDoc) string {
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Type))
}

// appAuthName preserves an explicit scheme name for same-type
// disambiguation without treating it as credential material.
func appAuthName(auth *sdkAppAuthDoc) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.Name)
}

// appConnectScopes canonicalizes the consent ceiling so source order and
// duplicate values cannot alter app identity at runtime.
func appConnectScopes(connect *sdkAppConnectDoc) []string {
	if connect == nil {
		return nil
	}
	return sortedUniqueStrings(connect.Scopes)
}

type sdkExecutionAuthContractFetcher interface {
	FetchServiceVersionExecutionAuthContracts(context.Context, []sandbox.ServiceVersionExecutionAuthSelection, string) ([]sandbox.ServiceVersionExecutionAuthContract, error)
}

type sdkAuthResolutionTelemetry struct {
	anonymousOnly int
	securedOnly   int
	mixed         int
	webhookOnly   int
	explicit      int
	inferred      int
	none          int
	required      int
	multiScheme   int
}

// resolveAppAuthPolicies turns human-facing auth selectors into the
// exact Registry scheme used at dispatch. Doing this once during planning
// keeps agents and SDK consumers from guessing provider-specific auth names.
func resolveAppAuthPolicies(ctx context.Context, registryClient sandbox.RegistryClient, apiKey string, services []sdkResolvedService, selections []models.SDKSelection) error {
	fetcher, ok := registryClient.(sdkExecutionAuthContractFetcher)
	if !ok {
		err := validateNoExecutionAuthContract(selections)
		outcome := "success"
		if err != nil {
			outcome = "unavailable"
		}
		recordSDKAuthResolution(ctx, unavailableAuthTelemetry(selections), outcome)
		return err
	}
	requests, err := sdkExecutionAuthContractSelections(services, selections)
	if err != nil {
		return err
	}
	contracts, err := fetcher.FetchServiceVersionExecutionAuthContracts(ctx, requests, apiKey)
	if err != nil {
		recordSDKAuthResolution(ctx, sdkAuthResolutionTelemetry{}, "registry_error")
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "failed to resolve service auth policies"}
	}
	bySelection := make(map[string]sandbox.ServiceVersionExecutionAuthContract, len(contracts))
	for _, contract := range contracts {
		bySelection[executionAuthContractKey(contract.ServiceID, contract.Version, contract.OperationNames, contract.SelectAll)] = contract
	}
	telemetry := sdkAuthResolutionTelemetry{}
	for index := range selections {
		contract, exists := bySelection[executionAuthContractKey(requests[index].ServiceID, requests[index].Version, requests[index].OperationNames, requests[index].SelectAll)]
		if !exists {
			recordSDKAuthResolution(ctx, telemetry, "invalid_selection")
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("service %s version auth contract was not found", selections[index].ServiceID)}
		}
		if err := validateSelectedOperations(requests[index], contract.Operations); err != nil {
			recordSDKAuthResolution(ctx, telemetry, "invalid_selection")
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
		}
		if err := resolveSelectionAuthPolicy(&selections[index], contract, &telemetry); err != nil {
			recordSDKAuthResolution(ctx, telemetry, "invalid_selection")
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
		}
	}
	recordSDKAuthResolution(ctx, telemetry, "success")
	return nil
}

func unavailableAuthTelemetry(selections []models.SDKSelection) sdkAuthResolutionTelemetry {
	telemetry := sdkAuthResolutionTelemetry{}
	for _, selection := range selections {
		if !selection.SelectAll && len(selection.OperationNames) == 0 {
			telemetry.webhookOnly++
			telemetry.none++
		}
	}
	return telemetry
}

func validateNoExecutionAuthContract(selections []models.SDKSelection) error {
	for _, selection := range selections {
		if selection.SelectAll || len(selection.OperationNames) > 0 || selection.AuthType != "" || selection.AuthName != "" || len(selection.ConnectScopes) > 0 || len(selection.RequiredAuth) > 0 {
			return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "registry auth policy resolution is unavailable"}
		}
	}
	return nil
}

func sdkExecutionAuthContractSelections(services []sdkResolvedService, selections []models.SDKSelection) ([]sandbox.ServiceVersionExecutionAuthSelection, error) {
	if len(services) != len(selections) {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "resolved service selection mismatch"}
	}
	out := make([]sandbox.ServiceVersionExecutionAuthSelection, len(selections))
	for index, selection := range selections {
		out[index] = sandbox.ServiceVersionExecutionAuthSelection{
			ServiceID: selection.ServiceID, Version: services[index].Version,
			OperationNames: selection.OperationNames, SelectAll: selection.SelectAll,
		}
	}
	return out, nil
}

func executionAuthContractKey(serviceID uuid.UUID, version string, operationNames []string, selectAll bool) string {
	return serviceID.String() + "\x00" + version + "\x00" + strings.Join(sortedUniqueStrings(operationNames), "\x00") + fmt.Sprintf("\x00%t", selectAll)
}

func validateSelectedOperations(selection sandbox.ServiceVersionExecutionAuthSelection, operations []sandbox.OperationSecuritySummary) error {
	if selection.SelectAll {
		return nil
	}
	returned := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		returned[operation.Name] = struct{}{}
	}
	for _, name := range selection.OperationNames {
		if _, exists := returned[name]; !exists {
			return fmt.Errorf("service %s selected operation %q was not found", selection.ServiceID, name)
		}
	}
	return nil
}

// resolveSelectionAuthPolicy pins a scheme only when at least one selected
// operation cannot run anonymously. This keeps webhook-only and anonymous SDK
// plans independent from provider credential readiness.
func resolveSelectionAuthPolicy(selection *models.SDKSelection, contract sandbox.ServiceVersionExecutionAuthContract, telemetry *sdkAuthResolutionTelemetry) error {
	secured := securedOperationSummaries(contract.Operations)
	recordSelectionSecurityShape(telemetry, *selection, len(contract.Operations), len(secured))
	if len(secured) == 0 {
		selection.AuthType, selection.AuthName, selection.ConnectScopes, selection.RequiredAuth = "", "", nil, nil
		telemetry.none++
		return nil
	}
	preferred, explicit, err := preferredAppAuth(*selection, contract.AuthConfigs, secured)
	if err != nil {
		return err
	}
	alternatives, err := selectOperationAuthAlternatives(selection.ServiceID, secured, preferred)
	if err != nil {
		return err
	}
	required, err := requiredSDKAuth(contract.AuthConfigs, alternatives)
	if err != nil {
		return fmt.Errorf("service %s auth contract is invalid: %w", selection.ServiceID, err)
	}
	selection.RequiredAuth = required
	telemetry.required += len(required)
	if len(required) > 1 {
		telemetry.multiScheme++
	}
	selected := preferred
	if selected == nil {
		selected = commonAlternativeAuth(contract.AuthConfigs, alternatives)
	}
	selection.AuthType, selection.AuthName = "", ""
	if selected != nil {
		selection.AuthType = sandbox.CanonicalFusedAuthType(*selected)
		selection.AuthName = sandbox.AuthCredentialName(*selected)
	}
	if explicit {
		telemetry.explicit++
	} else {
		telemetry.inferred++
	}
	if selected == nil {
		return nil
	}
	return validateAppScopes(selection, declaredOAuth2Scopes(*selected))
}

func preferredAppAuth(selection models.SDKSelection, auths fusedobject.AuthConfigs, operations []sandbox.OperationSecuritySummary) (*fusedobject.AuthConfig, bool, error) {
	explicit := selection.AuthType != "" || selection.AuthName != ""
	if !explicit && len(selection.ConnectScopes) == 0 {
		return nil, false, nil
	}
	matches := compatibleAppAuths(auths, operations, selection.AuthType, selection.AuthName)
	scopeMatches := appAuthsAcceptingScopes(matches, selection.ConnectScopes)
	if len(scopeMatches) == 0 && len(matches) > 0 && len(selection.ConnectScopes) > 0 {
		candidate := selection
		candidate.AuthType = sandbox.CanonicalFusedAuthType(matches[0])
		candidate.AuthName = sandbox.AuthCredentialName(matches[0])
		return nil, explicit, validateAppScopes(&candidate, declaredOAuth2Scopes(matches[0]))
	}
	if len(scopeMatches) == 0 {
		return nil, explicit, fmt.Errorf("service %s has no authentication scheme compatible with every secured selected operation", selection.ServiceID)
	}
	if explicit && len(scopeMatches) > 1 {
		return nil, true, fmt.Errorf("service %s auth selection is ambiguous; set auth.name", selection.ServiceID)
	}
	selected := scopeMatches[0]
	return &selected, explicit, nil
}

func selectOperationAuthAlternatives(serviceID uuid.UUID, operations []sandbox.OperationSecuritySummary, preferred *fusedobject.AuthConfig) ([]authrouting.Alternative, error) {
	selected := make([]authrouting.Alternative, 0, len(operations))
	preferredName := ""
	if preferred != nil {
		preferredName = preferred.Name
	}
	for _, operation := range operations {
		alternative, ok := firstOperationAuthAlternative(operation.SecurityRequirements, preferredName)
		if !ok {
			return nil, fmt.Errorf("service %s auth selection cannot satisfy operation %q", serviceID, operation.Name)
		}
		selected = append(selected, alternative)
	}
	return selected, nil
}

func firstOperationAuthAlternative(requirements authrouting.Requirements, preferredName string) (authrouting.Alternative, bool) {
	for _, alternative := range requirements {
		if len(alternative.Schemes) == 0 {
			continue
		}
		if preferredName == "" || alternativeContainsAuth(alternative, preferredName) {
			return alternative, true
		}
	}
	return authrouting.Alternative{}, false
}

func alternativeContainsAuth(alternative authrouting.Alternative, authName string) bool {
	for _, requirement := range alternative.Schemes {
		if requirement.Scheme == authName {
			return true
		}
	}
	return false
}

func requiredSDKAuth(auths fusedobject.AuthConfigs, alternatives []authrouting.Alternative) ([]models.SDKRequiredAuth, error) {
	definitions, err := sdkAuthDefinitions(auths)
	if err != nil {
		return nil, err
	}
	required := make(map[string]models.SDKRequiredAuth)
	for _, alternative := range alternatives {
		for _, requirement := range alternative.Schemes {
			auth, exists := definitions[requirement.Scheme]
			if !exists {
				return nil, fmt.Errorf("unknown auth scheme %q", requirement.Scheme)
			}
			required[auth.Name] = sdkRequiredAuth(auth)
		}
	}
	return sortedRequiredSDKAuth(required), nil
}

func sdkAuthDefinitions(auths fusedobject.AuthConfigs) (map[string]fusedobject.AuthConfig, error) {
	definitions := make(map[string]fusedobject.AuthConfig, len(auths))
	for _, auth := range auths {
		if auth.Name == "" || definitions[auth.Name].Name != "" {
			return nil, errors.New("auth definitions require unique names")
		}
		definitions[auth.Name] = auth
	}
	return definitions, nil
}

func sdkRequiredAuth(auth fusedobject.AuthConfig) models.SDKRequiredAuth {
	return models.SDKRequiredAuth{
		AuthType: sandbox.CanonicalFusedAuthType(auth), AuthName: sandbox.AuthCredentialName(auth),
		BasicPasswordMode: auth.BasicPasswordMode,
	}
}

func sortedRequiredSDKAuth(required map[string]models.SDKRequiredAuth) []models.SDKRequiredAuth {
	result := make([]models.SDKRequiredAuth, 0, len(required))
	for _, auth := range required {
		result = append(result, auth)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AuthName == result[j].AuthName {
			return result[i].AuthType < result[j].AuthType
		}
		return result[i].AuthName < result[j].AuthName
	})
	return result
}

func commonAlternativeAuth(auths fusedobject.AuthConfigs, alternatives []authrouting.Alternative) *fusedobject.AuthConfig {
	if len(alternatives) == 0 {
		return nil
	}
	definitions, err := sdkAuthDefinitions(auths)
	if err != nil {
		return nil
	}
	for _, requirement := range alternatives[0].Schemes {
		if authAppearsInEveryAlternative(requirement.Scheme, alternatives) {
			auth := definitions[requirement.Scheme]
			return &auth
		}
	}
	return nil
}

func authAppearsInEveryAlternative(authName string, alternatives []authrouting.Alternative) bool {
	for _, alternative := range alternatives {
		if !alternativeContainsAuth(alternative, authName) {
			return false
		}
	}
	return true
}

func appAuthsAcceptingScopes(auths fusedobject.AuthConfigs, scopes []string) fusedobject.AuthConfigs {
	if len(scopes) == 0 {
		return auths
	}
	matches := make(fusedobject.AuthConfigs, 0, len(auths))
	for _, auth := range auths {
		authType := sandbox.CanonicalFusedAuthType(auth)
		if (authType == "oauth" || authType == "oidc") && scopesAllowed(scopes, declaredOAuth2Scopes(auth)) {
			matches = append(matches, auth)
		}
	}
	return matches
}

func declaredOAuth2Scopes(auth fusedobject.AuthConfig) []string {
	values := make(map[string]struct{})
	for _, flow := range auth.OAuth2Flows {
		for scope := range flow.Scopes {
			values[scope] = struct{}{}
		}
	}
	result := make([]string, 0, len(values))
	for scope := range values {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result
}

func scopesAllowed(requested, allowed []string) bool {
	allowedSet := stringSet(allowed)
	for _, scope := range requested {
		if !allowedSet[scope] {
			return false
		}
	}
	return true
}

func securedOperationSummaries(operations []sandbox.OperationSecuritySummary) []sandbox.OperationSecuritySummary {
	secured := make([]sandbox.OperationSecuritySummary, 0, len(operations))
	for _, operation := range operations {
		if !operationPermitsAnonymous(operation) {
			secured = append(secured, operation)
		}
	}
	return secured
}

func operationPermitsAnonymous(operation sandbox.OperationSecuritySummary) bool {
	for _, alternative := range operation.SecurityRequirements {
		if len(alternative.Schemes) == 0 {
			return true
		}
	}
	return false
}

func compatibleAppAuths(auths fusedobject.AuthConfigs, operations []sandbox.OperationSecuritySummary, authType, authName string) fusedobject.AuthConfigs {
	candidates := append(fusedobject.AuthConfigs(nil), auths...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Name == candidates[j].Name {
			return sandbox.CanonicalFusedAuthType(candidates[i]) < sandbox.CanonicalFusedAuthType(candidates[j])
		}
		return candidates[i].Name < candidates[j].Name
	})
	matches := make(fusedobject.AuthConfigs, 0, len(candidates))
	for _, auth := range candidates {
		if authName != "" && auth.Name != authName {
			continue
		}
		if authType != "" && sandbox.CanonicalFusedAuthType(auth) != authType {
			continue
		}
		if authSupportsEveryOperation(auth, operations) {
			matches = append(matches, auth)
		}
	}
	return matches
}

func authSupportsEveryOperation(auth fusedobject.AuthConfig, operations []sandbox.OperationSecuritySummary) bool {
	for _, operation := range operations {
		if !operationSupportsAuth(operation, auth.Name) {
			return false
		}
	}
	return true
}

func operationSupportsAuth(operation sandbox.OperationSecuritySummary, authName string) bool {
	for _, alternative := range operation.SecurityRequirements {
		for _, requirement := range alternative.Schemes {
			if requirement.Scheme == authName {
				return true
			}
		}
	}
	return false
}

func recordSelectionSecurityShape(telemetry *sdkAuthResolutionTelemetry, selection models.SDKSelection, operationCount, securedCount int) {
	if !selection.SelectAll && len(selection.OperationNames) == 0 {
		telemetry.webhookOnly++
		return
	}
	if securedCount == 0 {
		telemetry.anonymousOnly++
		return
	}
	if securedCount == operationCount {
		telemetry.securedOnly++
		return
	}
	telemetry.mixed++
}

func recordSDKAuthResolution(ctx context.Context, telemetry sdkAuthResolutionTelemetry, outcome string) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("sdk.auth.anonymous_only_count", telemetry.anonymousOnly),
		attribute.Int("sdk.auth.secured_only_count", telemetry.securedOnly),
		attribute.Int("sdk.auth.mixed_count", telemetry.mixed),
		attribute.Int("sdk.auth.webhook_only_count", telemetry.webhookOnly),
		attribute.Int("sdk.auth.explicit_count", telemetry.explicit),
		attribute.Int("sdk.auth.inferred_count", telemetry.inferred),
		attribute.Int("sdk.auth.none_count", telemetry.none),
		attribute.Int("sdk.auth.required_scheme_count", telemetry.required),
		attribute.Int("sdk.auth.multi_scheme_selection_count", telemetry.multiScheme),
		attribute.String("sdk.auth.decision_source", authDecisionSource(telemetry)),
		attribute.String("sdk.auth.decision_outcome", outcome),
	)
}

func authDecisionSource(telemetry sdkAuthResolutionTelemetry) string {
	if telemetry.explicit > 0 && telemetry.inferred > 0 {
		return "mixed"
	}
	if telemetry.explicit > 0 {
		return "explicit"
	}
	if telemetry.inferred > 0 {
		return "inferred"
	}
	return "none"
}

// validateAppScopes lets an app narrow OAuth/OIDC permissions while
// preventing config from requesting scopes absent from the provider contract.
func validateAppScopes(selection *models.SDKSelection, allowed []string) error {
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
		inbox.Items = append(inbox.Items, filterSDKEngineNotifications(notifications, call.request.ConfigKey)...)
	}
	if registryClient == nil {
		return inbox
	}
	if !entitlement.LiveEntitlement.Load().DriftMonitoringEnabled {
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
) []workspaceNotificationInboxItem {
	var items []workspaceNotificationInboxItem
	for _, notification := range notifications {
		if sdkNotificationMatches(notification, configKey) {
			items = append(items, workspaceNotificationInboxItems([]store.WorkspaceNotification{notification})...)
		}
	}
	return items
}

func sdkNotificationMatches(notification store.WorkspaceNotification, configKey string) bool {
	for _, target := range strings.Split(notification.ConfigKey, ",") {
		if strings.TrimSpace(target) == configKey {
			return true
		}
	}
	return false
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
// service (the shape resolveSDKSelections uses).
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

func activationVersionExistsByUUID(versions []store.WorkspaceServiceVersion, serviceVersionID uuid.UUID) bool {
	for _, allowed := range versions {
		if allowed.ServiceVersionID == serviceVersionID {
			return true
		}
	}
	return false
}

func previousSDKDocument(state *store.ConfigState) sdkConfigDocument {
	var previous sdkConfigDocument
	if state != nil && len(state.DesiredState) > 0 {
		_ = json.Unmarshal(state.DesiredState, &previous)
	}
	return previous
}

func sdkGenerateRequest(doc sdkConfigDocument, selections []models.SDKSelection, bindings []sdkContractBinding, defaultEngineURL string) GenerateSDKRequest {
	return GenerateSDKRequest{
		Name:             doc.Name,
		Description:      fmt.Sprintf("GitOps managed SDK %s", doc.Name),
		Version:          doc.Version,
		Selections:       selections,
		IncludeMCP:       false,
		TargetType:       store.AppKindSDK.String(),
		TargetLanguage:   doc.Language,
		DefaultEngineURL: strings.TrimSpace(defaultEngineURL),
		SkipPackaging:    doc.Generate != nil && !*doc.Generate,
		ContractBindings: bindings,
	}
}

// resolvedSDKPayload packages compiled private definitions and public descriptors into the signed plan payload.
func resolvedSDKPayload(request GenerateSDKRequest, bucketID, appID uuid.UUID, noop bool) appResolvedPayload {
	return appResolvedPayload{
		AppID: appID, Noop: noop, BucketID: bucketID, Name: request.Name, Description: request.Description, Version: request.Version,
		Selections: request.Selections, IncludeMCP: request.IncludeMCP, TargetType: request.TargetType,
		TargetLanguage: request.TargetLanguage, DefaultEngineURL: request.DefaultEngineURL,
		SkipSandbox: request.SkipSandbox, SkipPackaging: request.SkipPackaging, ContractBindings: request.ContractBindings,
		UnifiedOperations: request.UnifiedOperations,
	}
}

func sdkPlanSummary(create, update bool, services []map[string]any) map[string]any {
	return map[string]any{
		"create_sdk": create,
		"update_sdk": update,
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

// executeSDKConfigApply routes admitted work through the canonical Unified operation boundary and accounting path.
func executeSDKConfigApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	proxy Forwarder,
	registryClient sandbox.RegistryClient,
	call sdkApplyCall,
) (sdkGenerationResult, error) {
	// A plan has one deterministic Registry package. Serializing that identity
	// prevents a losing first apply from deleting the app committed by a
	// concurrent winner after both initially observed an empty local state.
	unlockApp := sdkGenerationApplies.lock(stableAppIDForPlan(call.planID))
	defer unlockApp()
	plan, err := loadAuthorizedSDKAppPlanForApply(ctx, configStore, s, call)
	if err != nil {
		return sdkGenerationResult{}, err
	}
	lease, err := configStore.ReserveConfigPlanApply(ctx, call.planID, call.planRevision)
	if err != nil {
		return sdkGenerationResult{}, configPlanApplyReservationHTTPError(err)
	}
	leaseGuard := workspaceApplyLeaseGuard{configStore: configStore, planID: call.planID, revision: call.planRevision, leaseID: lease.ID, releasable: true}
	defer leaseGuard.release()
	applyCtx, stopLease := workspaceApplyLeaseContextWithTimeout(ctx, configStore, call.planID, call.planRevision, lease.ID, sdkGenerationApplyTimeout+time.Minute)
	defer stopLease()
	call.applyLeaseID = lease.ID
	if payload, noop := noopSDKPayload(plan.ResolvedPayload); noop {
		result, applyErr := applyNoopSDKPlan(applyCtx, configStore, s, call, plan, payload)
		// No external system was contacted, so a failed local transaction is
		// always safe to release and retry immediately.
		leaseGuard.releasable = true
		return result, applyErr
	}
	// From the first Registry call onward, release requires either a committed
	// local transaction or positively confirmed compensation. Unknown external
	// outcomes remain fenced until the database lease expires.
	leaseGuard.releasable = false

	plan, result, err := generateSDKForApply(applyCtx, configStore, s, proxy, registryClient, call)
	if err != nil {
		leaseGuard.releasable = sdkGenerationFailureReleasable(applyCtx, proxy, result)
		return sdkGenerationResult{}, err
	}
	applyCtx, scopeSpan := otel.Tracer("engine").Start(applyCtx, "engine.sdk_scope.persist")
	defer scopeSpan.End()
	scopeSpan.SetAttributes(
		attribute.String("app.id", result.AppID.String()),
		attribute.String("sdk_generation_status", result.Status),
		attribute.Int("scope_schema_version", result.ScopeSchemaVersion),
	)
	if err := validateSDKGenerationResult(plan.ResolvedPayload, call, result.SDKGenerationResult); err != nil {
		scopeSpan.SetStatus(codes.Error, err.Error())
		scopeSpan.SetAttributes(attribute.String("outcome", "validation_failed"))
		leaseGuard.releasable = compensateNewRegistryPackage(applyCtx, proxy, result)
		return sdkGenerationResult{}, err
	}
	if err := revalidateSDKContractsAfterGeneration(applyCtx, registryClient, call.apiKey, plan.ResolvedPayload); err != nil {
		scopeSpan.SetStatus(codes.Error, "contract_revalidation_failed")
		scopeSpan.SetAttributes(attribute.String("outcome", "contract_revalidation_failed"))
		leaseGuard.releasable = compensateNewRegistryPackage(applyCtx, proxy, result)
		return sdkGenerationResult{}, err
	}
	token, familyID, appID, _, err := applyGeneratedAppRuntime(applyCtx, configStore, s, call, plan, result)
	if err != nil {
		scopeSpan.SetStatus(codes.Error, err.Error())
		scopeSpan.SetAttributes(attribute.String("outcome", "scope_persist_failed"))
		leaseGuard.releasable = compensateRejectedSDKPackage(applyCtx, configStore, plan.ConfigKey, proxy, result)
		return sdkGenerationResult{}, err
	}
	leaseGuard.releasable = true
	result.AppFamilyID = familyID
	result.AppID = appID
	result.ExecutionToken = token
	scopeSpan.SetAttributes(attribute.String("outcome", "success"))
	return result, nil
}

func compensateRejectedSDKPackage(ctx context.Context, configStore store.ConfigRepository, configKey string, proxy Forwarder, result sdkGenerationResult) bool {
	state, err := configStore.GetConfigState(ctx, configKey)
	if err == nil && state != nil && state.LatestResourceID != nil && *state.LatestResourceID == result.AppID {
		// A concurrent apply may have committed this exact immutable app. Its
		// package is now referenced and must not be removed by the losing caller.
		return true
	}
	return compensateNewRegistryPackage(ctx, proxy, result)
}

func noopSDKPayload(raw json.RawMessage) (appResolvedPayload, bool) {
	payload, err := appPayloadFromJSON(raw)
	return payload, err == nil && payload.Noop
}

func sdkGenerationFailureReleasable(ctx context.Context, proxy Forwarder, result sdkGenerationResult) bool {
	if result.createdForPlan {
		return compensateNewRegistryPackage(ctx, proxy, result)
	}
	return !result.registryGenerationAttempted || result.registryGenerationOutcomeConfirmed
}

func applyNoopSDKPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, plan *store.ConfigPlan, payload appResolvedPayload) (sdkGenerationResult, error) {
	current, err := configStore.GetConfigState(ctx, plan.ConfigKey)
	if err != nil || current == nil || current.LatestResourceID == nil || *current.LatestResourceID != payload.AppID {
		return sdkGenerationResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale_or_mismatched"}
	}
	_, err = configStore.ApplyConfigPlan(ctx, store.ApplyConfigPlanParams{
		State: store.UpsertConfigStateParams{
			ConfigKey: plan.ConfigKey, ConfigType: plan.ConfigType, SourceHash: plan.SourceHash,
			DesiredState: plan.DesiredState, ManagedResources: current.ManagedResources,
			LatestResourceID: current.LatestResourceID, UpdatedBy: call.accountID,
		},
		PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: call.planRevision, ApplyLeaseID: call.applyLeaseID,
	})
	if err != nil {
		return sdkGenerationResult{}, appApplyPersistenceError(ctx, err, payload.AppID)
	}
	app, err := s.GetApp(ctx, payload.AppID)
	if err != nil {
		return sdkGenerationResult{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "app_scope_not_found"}
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("sdk.apply_mode", "noop"), attribute.String("outcome", "success"))
	span.AddEvent("sdk configuration apply skipped registry generation")
	// immutable no-op convergence is a meaningful mutation outcome, while
	// the response and plan payload are intentionally excluded from durable audit.
	accesscontrol.MarkMutationAuditUnchanged(ctx)
	return sdkGenerationResult{SDKGenerationResult: models.SDKGenerationResult{
		AppFamilyID: app.AppFamilyID,
		AppID:       app.AppID,
		Status:      models.SDKGenerationStatusComplete,
	}}, nil
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
	if err := validateRegistryAppIdentity(input.payload, result.AppID); err != nil {
		// Preserve the unexpected Registry identity so the caller retains the
		// lease instead of treating this external outcome as safely untouched.
		return input.plan, result, err
	}
	// Ownership is established as soon as Registry creates the deterministic
	// identity, so stream failures can compensate it as well as DB failures.
	result.createdForPlan = input.existingConfigResourceID == uuid.Nil
	completed, err := awaitSDKGenerationCompletion(ctx, proxy, call.apiKey, result)
	if err != nil {
		return input.plan, result, err
	}
	return input.plan, completed, nil
}

type sdkGenerationApplyInput struct {
	plan                     *store.ConfigPlan
	existingConfigResourceID uuid.UUID
	payload                  json.RawMessage
}

// prepareSDKGenerationForApply assembles sdk generation for apply without starting persistence, accounting, or provider work.
func prepareSDKGenerationForApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	registryClient sandbox.RegistryClient,
	call sdkApplyCall,
) (sdkGenerationApplyInput, error) {
	plan, err := loadAuthorizedSDKAppPlanForApply(ctx, configStore, s, call)
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
	doc, resolvedPayload, err := decodeAppApplyPlan(ctx, configStore, s, plan, store.AppKindSDK.String())
	if err != nil {
		return sdkGenerationApplyInput{}, err
	}
	if err := normalizeAndValidateResolvedUnifiedPayload(&resolvedPayload); err != nil {
		return sdkGenerationApplyInput{}, err
	}
	familyID, appID, existing, err := reserveSDKGenerationIdentity(ctx, s, call, plan, doc, resolvedPayload)
	if err != nil {
		return sdkGenerationApplyInput{}, err
	}
	generationPayload, err := sdkGenerationPayloadForPlan(plan.ResolvedPayload, call, familyID, appID, plan.SourceHash)
	if err != nil {
		return sdkGenerationApplyInput{}, err
	}
	return sdkGenerationApplyInput{plan: plan, existingConfigResourceID: existing, payload: generationPayload}, nil
}

// reserveSDKGenerationIdentity persists Unified operation identity atomically while preserving immutability checks.
func reserveSDKGenerationIdentity(ctx context.Context, s store.Store, call sdkApplyCall, plan *store.ConfigPlan, doc sdkConfigDocument, resolved appResolvedPayload) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.sdk_config.reserve_identity")
	defer span.End()

	canonicalName, displayName, err := canonical.AppName(doc.Name)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid_app_name"}
	}
	if err := checkSDKFamilyCapacity(ctx, s, span, call.accountID, canonicalName); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}

	family, _, err := applifecycle.New(s).CreateOrGetFamily(ctx, applifecycle.CreateFamilyParams{
		AccountID: call.accountID, Kind: store.AppKindSDK, CanonicalName: canonicalName,
		DisplayName: displayName, TargetLanguage: doc.Language,
		OwnerSubjectID: planOwnerSubjectID(plan), OwnerTeamID: planOwnerTeamID(plan),
	})
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app_family_conflict"}
	}
	appID, existingID, err := reserveSDKVersionIdentityWithUnified(ctx, s, call.planID, family.AppFamilyID, doc.Version, plan.SourceHash, resolved)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	span.SetAttributes(
		attribute.String("app.family_id", family.AppFamilyID.String()),
		attribute.String("app.id", appID.String()),
		attribute.Bool("app.version_existing", existingID != uuid.Nil),
		attribute.String("outcome", "success"),
	)
	return family.AppFamilyID, appID, existingID, nil
}

func checkSDKFamilyCapacity(ctx context.Context, s store.Store, span trace.Span, accountID uuid.UUID, canonicalName string) error {
	_, familyErr := s.GetAppFamilyByIdentity(ctx, accountID, store.AppKindSDK.String(), canonicalName)
	if !errors.Is(familyErr, store.ErrAppFamilyNotFound) {
		return nil
	}

	// Existing families consume no additional entitlement, so only the absent
	// identity path needs the aggregate count query.
	currentFamilies, err := s.CountAppFamilies(ctx, accountID, store.AppKindSDK.String())
	if err != nil {
		span.RecordError(err)
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed_to_count_sdk_families"}
	}
	if limitErr := entitlement.CheckLimit(span, "sdk_families", currentFamilies, entitlement.LiveEntitlement.Load().MaxSDKFamilies); limitErr != nil {
		span.SetAttributes(attribute.String("outcome", "sdk_family_limit_exceeded"))
		return workspaceConfigHTTPError{status: http.StatusForbidden, message: limitErr.Error()}
	}
	return nil
}

// reserveSDKVersionIdentity persists Unified operation identity atomically while preserving immutability checks.
func reserveSDKVersionIdentity(ctx context.Context, s store.Store, planID, familyID uuid.UUID, version, sourceHash string) (uuid.UUID, uuid.UUID, error) {
	return reserveSDKVersionIdentityWithUnified(ctx, s, planID, familyID, version, sourceHash, appResolvedPayload{
		UnifiedDefinitionSchemaVersion: unified.DefinitionSchemaVersion,
		UnifiedDefinitions:             json.RawMessage("[]"),
		UnifiedDefinitionHash:          store.EmptyUnifiedSetHash,
		UnifiedCodegenDescriptorHash:   store.EmptyUnifiedSetHash,
	})
}

// reserveSDKVersionIdentityWithUnified persists Unified operation identity atomically while preserving immutability checks.
func reserveSDKVersionIdentityWithUnified(ctx context.Context, s store.Store, planID, familyID uuid.UUID, version, sourceHash string, resolved appResolvedPayload) (uuid.UUID, uuid.UUID, error) {
	tombstoned, err := s.AppTombstoneExists(ctx, familyID, version)
	if err != nil {
		return uuid.Nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed_to_check_app_version"}
	}
	if tombstoned {
		return uuid.Nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app_version_deactivated"}
	}
	existing, err := s.GetAppByFamilyAndVersion(ctx, familyID, version)
	if errors.Is(err, store.ErrAppNotFound) {
		return stableAppIDForPlan(planID), uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed_to_check_app_version"}
	}
	if existing.SourceHash != sourceHash || !existingAppMatchesResolvedUnified(existing, resolved) {
		return uuid.Nil, uuid.Nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app_version_immutable"}
	}
	return existing.AppID, existing.AppID, nil
}

// existingAppMatchesResolvedUnified treats legacy empty fields as the canonical empty set before immutability comparison.
func existingAppMatchesResolvedUnified(existing *store.App, resolved appResolvedPayload) bool {
	schemaVersion := existing.UnifiedDefinitionSchemaVersion
	definitionHash := existing.UnifiedDefinitionHash
	descriptorHash := existing.UnifiedCodegenDescriptorHash
	// versions published before Unified fields existed represent the same
	// immutable empty definition set, rather than a distinct invalid contract.
	if schemaVersion == 0 {
		schemaVersion = unified.DefinitionSchemaVersion
	}
	if definitionHash == "" {
		definitionHash = store.EmptyUnifiedSetHash
	}
	if descriptorHash == "" {
		descriptorHash = store.EmptyUnifiedSetHash
	}
	return schemaVersion == resolved.UnifiedDefinitionSchemaVersion &&
		definitionHash == resolved.UnifiedDefinitionHash &&
		descriptorHash == resolved.UnifiedCodegenDescriptorHash
}

func runTrackedSDKGeneration(ctx context.Context, proxy Forwarder, apiKey string, payload json.RawMessage) (sdkGenerationResult, error) {
	result := sdkGenerationResult{registryGenerationAttempted: true}
	for attempt := 0; ; attempt++ {
		generated, err := runSDKGeneration(ctx, proxy, apiKey, payload)
		if err == nil {
			generated.registryGenerationAttempted = true
			return generated, nil
		}
		var proxyErr sdkProxyError
		if errors.As(err, &proxyErr) && proxyErr.status >= 400 && proxyErr.status < 500 {
			result.registryGenerationOutcomeConfirmed = true
			return result, err
		}
		if attempt >= len(sdkGenerationRequestRetryDelays) || !waitForSDKGenerationRetry(ctx, sdkGenerationRequestRetryDelays[attempt]) {
			return result, err
		}
	}
}

// Registry generation is durably idempotent on the plan ID and deterministic
// app identity embedded in payload. Replaying the exact request while this
// apply still owns its lease resolves transient proxy/5xx ambiguity without
// allowing a second package or app version to be created.
var sdkGenerationRequestRetryDelays = [...]time.Duration{100 * time.Millisecond, 500 * time.Millisecond}

func waitForSDKGenerationRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func compensateNewRegistryPackage(ctx context.Context, proxy Forwarder, result sdkGenerationResult) bool {
	if !result.createdForPlan || result.AppID == uuid.Nil {
		return true
	}
	// Cleanup must outlive a disconnected caller, but remains bounded so a
	// Registry outage cannot strand an Engine request indefinitely.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cleanupCtx, span := otel.Tracer("engine").Start(cleanupCtx, "engine.sdk_generation.compensate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", result.AppID.String()))
	if err := deleteRegistryPackage(cleanupCtx, proxy, result.AppID); err != nil {
		// The plan remains pending and keeps the same deterministic app ID,
		// so a retry reclaims the same Registry record and retries compensation.
		slog.ErrorContext(cleanupCtx, "failed to compensate rejected Registry package", slog.String("app_id", result.AppID.String()))
		span.SetStatus(codes.Error, "registry package compensation failed")
		span.SetAttributes(attribute.String("outcome", "failed"))
		return false
	}
	span.SetAttributes(attribute.String("outcome", "deleted"))
	return true
}

func validateRegistryAppIdentity(payload json.RawMessage, returnedID uuid.UUID) error {
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil || request.AppID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_requested_app_id_invalid"}
	}
	// Registry may update bytes and job state, but it cannot replace the stable
	// identity Engine authorized and placed in the generation request.
	if returnedID != request.AppID {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_app_id_mismatch"}
	}
	return nil
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
		attribute.String("app.id", initial.AppID.String()),
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

func appInjections(injections []InjectionConfig) []models.SDKInjectionConfig {
	if len(injections) == 0 {
		return nil
	}
	out := make([]models.SDKInjectionConfig, len(injections))
	for i, inj := range injections {
		out[i] = models.SDKInjectionConfig{
			Location: inj.Location,
			Name:     inj.Name,
			Value:    inj.Value,
			Mode:     inj.Mode,
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

func sdkGenerationPayloadForPlan(payload json.RawMessage, call sdkApplyCall, appFamilyID, appID uuid.UUID, sourceHash string) (json.RawMessage, error) {
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid sdk generation payload"}
	}
	request.IdempotencyKey = call.planID.String()
	request.AppFamilyID = appFamilyID
	request.AppID = appID
	request.SourceHash = sourceHash
	request.GeneratorVersion = models.SDKGeneratorVersion
	out, _ := json.Marshal(request)
	return out, nil
}

// stableAppIDForPlan derives a deterministic App ID from planID alone --
// Engine is mono-workspace, so there is no separate workspace dimension left
// to fold into the hash for cross-workspace disambiguation.
func stableAppIDForPlan(planID uuid.UUID) uuid.UUID {
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

// finalizeAppSelections pins every selection to the current selection schema and exact service revision.
func finalizeAppSelections(selections []models.SDKSelection, bindings []sdkContractBinding) []models.SDKSelection {
	byService := sdkBindingMap(bindings)
	for i := range selections {
		// Consumers must validate schema identity rather than infer it from a missing value.
		selections[i].SchemaVersion = models.AppSelectionSchemaVersion
		// A resolved binding is the only authoritative source for an immutable service version ID.
		if binding, ok := byService[selections[i].ServiceID]; ok && binding.ServiceVersionID != uuid.Nil {
			selections[i].ServiceVersionID = binding.ServiceVersionID
		}
	}
	return selections
}

func sdkContractBindingsFromPayload(payload json.RawMessage) ([]sdkContractBinding, error) {
	resolved, err := appPayloadFromJSON(payload)
	if err != nil {
		return nil, err
	}
	return resolved.ContractBindings, nil
}

// appPayloadFromJSON strictly decodes the Registry plan payload before post-generation contract checks.
func appPayloadFromJSON(payload json.RawMessage) (appResolvedPayload, error) {
	var resolved appResolvedPayload
	if err := json.Unmarshal(payload, &resolved); err != nil {
		return appResolvedPayload{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid app resolved payload"}
	}
	if err := normalizeAndValidateResolvedUnifiedPayload(&resolved); err != nil {
		return appResolvedPayload{}, err
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

// revalidateSDKContractsAfterGeneration rechecks every generated forward and rollback endpoint against the current Registry snapshot.
func revalidateSDKContractsAfterGeneration(ctx context.Context, registryClient sandbox.RegistryClient, apiKey string, raw json.RawMessage) error {
	bindings, err := sdkContractBindingsFromPayload(raw)
	if err != nil {
		return err
	}
	if err := ensureSDKContractBindingsCurrent(ctx, registryClient, apiKey, bindings); err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
	}
	return nil
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
	resolved, err := appPayloadFromJSON(payload)
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
	if result.AppID == uuid.Nil {
		return result, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "invalid sdk ID returned from generator"}
	}
	return result, nil
}

func resolveAppBucket(ctx context.Context, s store.Store, bucketName string) (*store.Bucket, error) {
	bucketName = strings.TrimSpace(bucketName)
	if bucketName == "" {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "app config requires exactly one bucket"}
	}
	b, err := s.GetBucketByName(ctx, bucketName)
	if err != nil || b == nil || b.ID == uuid.Nil {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bucket not found: " + bucketName}
	}
	return b, nil
}

func validateAppBucketIdentity(ctx context.Context, s store.Store, bucketName string, authorizedBucketID uuid.UUID) (*store.Bucket, error) {
	bucket, err := resolveAppBucket(ctx, s, bucketName)
	if err != nil || authorizedBucketID == uuid.Nil || bucket.ID != authorizedBucketID {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "app bucket identity changed; create a new plan"}
	}
	return bucket, nil
}

type appReadinessBucket struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type appMissingCredential struct {
	ServiceID         string                        `json:"service_id"`
	Service           string                        `json:"service,omitempty"`
	AuthType          string                        `json:"auth_type"`
	AuthName          string                        `json:"auth_name"`
	BasicPasswordMode authrouting.BasicPasswordMode `json:"basic_password_mode,omitempty"`
	RequiredFields    []appMissingCredentialField   `json:"required_fields"`
}

type appMissingCredentialField struct {
	Name      string `json:"name"`
	SecretKey string `json:"secret_key,omitempty"`
}

// validateAppBucketReadiness checks the one bucket selected by an app before a
// plan is persisted and again during apply. The planner's immutable chosen
// alternatives let this pass validate every AND member from metadata without
// decrypting values. One bucket-scoped pass reports all missing material, and
// its typed response contains prompt labels and secret-key names only so a CLI
// can remediate without parsing prose or exposing credential values.
func validateAppBucketReadiness(ctx context.Context, s store.Store, bucket store.Bucket, selections []models.SDKSelection, serviceNames map[uuid.UUID]string) error {
	if bucket.ID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusBadRequest, message: "app config requires exactly one bucket"}
	}
	if !appSelectionsRequireAuth(selections) {
		return nil
	}
	ready, secretKeys, err := loadAppBucketMaterial(ctx, s, bucket.ID, selections)
	if err != nil {
		return err
	}
	missing := make([]appMissingCredential, 0)
	for _, selection := range selections {
		missing = append(missing, missingAppBucketMaterial(selection, serviceNames[selection.ServiceID], ready, secretKeys)...)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return appMissingCredentialKey(missing[i]) < appMissingCredentialKey(missing[j]) })
	return workspaceConfigHTTPError{
		status:   http.StatusBadRequest,
		code:     "bucket_credentials_missing",
		message:  "The selected credential set is missing required authentication material.",
		category: "validation",
		details: map[string]any{
			"bucket":              appReadinessBucket{ID: bucket.ID.String(), Name: bucket.Name},
			"missing_credentials": missing,
		},
		remediation: "Add the required credentials to the credential set and create the plan again.",
	}
}

func appMissingCredentialKey(missing appMissingCredential) string {
	return missing.ServiceID + "\x00" + missing.AuthType + "\x00" + missing.AuthName
}

// appReadinessServiceNames reuses the bounded plan resolution result for
// friendlier prompts. Apply intentionally passes no names because the persisted
// execution scope is ID-authoritative and readiness must never add a lookup.
func appReadinessServiceNames(resolved []sdkResolvedService) map[uuid.UUID]string {
	names := make(map[uuid.UUID]string, len(resolved))
	for _, service := range resolved {
		names[service.ServiceID] = service.ServiceName
	}
	return names
}

func appSelectionsRequireAuth(selections []models.SDKSelection) bool {
	for _, selection := range selections {
		if len(selection.RequiredAuth) > 0 {
			return true
		}
	}
	return false
}

func loadAppBucketMaterial(ctx context.Context, s store.Store, bucketID uuid.UUID, selections []models.SDKSelection) (map[string]bool, map[string]bool, error) {
	repository, ok := s.(store.AppBucketReadinessStore)
	if !ok {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to read bucket readiness"}
	}
	requirements := appBucketCredentialRequirements(selections)
	presence, err := repository.GetAppBucketCredentialPresence(ctx, bucketID, requirements)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to read bucket readiness"}
	}
	ready := make(map[string]bool, len(presence))
	secretKeys := make(map[string]bool)
	for _, item := range presence {
		if item.Connected {
			ready[appConnectReadinessKey(item.ServiceID, item.AuthType, item.AuthName)] = true
		}
		for _, key := range item.SecretKeys {
			secretKeys[item.ServiceID.String()+"\x00"+key] = true
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("bucket_credential_requirement_count", len(requirements)),
		attribute.Int("bucket_credential_presence_count", len(presence)),
	)
	return ready, secretKeys, nil
}

func appBucketCredentialRequirements(selections []models.SDKSelection) []store.AppCredentialRequirement {
	requirements := make([]store.AppCredentialRequirement, 0)
	for _, selection := range selections {
		for _, required := range selection.RequiredAuth {
			keys := make([]string, 0)
			for _, field := range appRequiredSecretFields(required) {
				if field.SecretKey != "" {
					keys = append(keys, field.SecretKey)
				}
			}
			requirements = append(requirements, store.AppCredentialRequirement{
				ServiceID: selection.ServiceID, AuthType: canonicalWorkspaceStaticAuthType(required.AuthType),
				AuthName: strings.TrimSpace(required.AuthName), SecretKeys: keys,
			})
		}
	}
	return requirements
}

func missingAppBucketMaterial(selection models.SDKSelection, serviceName string, ready, secretKeys map[string]bool) []appMissingCredential {
	missing := make([]appMissingCredential, 0)
	for _, required := range selection.RequiredAuth {
		if required.AuthType == "oauth" || required.AuthType == "oidc" {
			if !ready[appConnectReadinessKey(selection.ServiceID, required.AuthType, required.AuthName)] {
				missing = append(missing, newAppMissingCredential(selection.ServiceID, serviceName, required, []appMissingCredentialField{{Name: "connection"}}))
			}
			continue
		}
		fields := missingAppSecretFields(selection.ServiceID, required, secretKeys)
		if len(fields) > 0 {
			missing = append(missing, newAppMissingCredential(selection.ServiceID, serviceName, required, fields))
		}
	}
	return missing
}

func newAppMissingCredential(serviceID uuid.UUID, serviceName string, required models.SDKRequiredAuth, fields []appMissingCredentialField) appMissingCredential {
	return appMissingCredential{
		ServiceID: serviceID.String(), Service: serviceName,
		AuthType: canonicalWorkspaceStaticAuthType(required.AuthType), AuthName: strings.TrimSpace(required.AuthName), BasicPasswordMode: required.BasicPasswordMode,
		RequiredFields: fields,
	}
}

func missingAppSecretFields(serviceID uuid.UUID, required models.SDKRequiredAuth, secretKeys map[string]bool) []appMissingCredentialField {
	fields := make([]appMissingCredentialField, 0)
	for _, field := range appRequiredSecretFields(required) {
		if !secretKeys[serviceID.String()+"\x00"+field.SecretKey] {
			fields = append(fields, field)
		}
	}
	return fields
}

func appConnectReadinessKey(serviceID uuid.UUID, authType, authName string) string {
	return serviceID.String() + "\x00" + canonicalWorkspaceStaticAuthType(authType) + "\x00" + strings.TrimSpace(authName)
}

// appRequiredSecretFields keeps prompt-safe field labels beside the exact
// Engine secret keys. Exact scheme identity and Basic mode were pinned during
// Registry policy resolution, so readiness needs neither provider rules nor
// decrypted values.
func appRequiredSecretFields(required models.SDKRequiredAuth) []appMissingCredentialField {
	name := strings.TrimSpace(required.AuthName)
	if name == "" {
		return []appMissingCredentialField{{Name: "credential_name", SecretKey: "<credential-name>"}}
	}
	switch required.AuthType {
	case "basic":
		switch required.BasicPasswordMode {
		case authrouting.BasicPasswordRequired:
			return []appMissingCredentialField{{Name: "username", SecretKey: name + "_username"}, {Name: "password", SecretKey: name + "_password"}}
		case authrouting.BasicPasswordOptional, authrouting.BasicPasswordEmpty:
			return []appMissingCredentialField{{Name: "username", SecretKey: name + "_username"}}
		default:
			return []appMissingCredentialField{{Name: "invalid_basic_password_mode", SecretKey: "<invalid-basic-password-mode>"}}
		}
	case "mtls":
		return []appMissingCredentialField{{Name: "certificate", SecretKey: name + "_cert"}, {Name: "private_key", SecretKey: name + "_key"}}
	case "api_key":
		return []appMissingCredentialField{{Name: "api_key", SecretKey: name}}
	case "bearer":
		return []appMissingCredentialField{{Name: "token", SecretKey: name}}
	case "oauth", "oidc":
		return nil
	default:
		return []appMissingCredentialField{{Name: "invalid_auth_type", SecretKey: "<invalid-auth-type>"}}
	}
}

// persistAppRuntimeParams carries the exact app version plus its family-level
// bucket and owner inputs into the atomic config apply transaction.
type persistAppRuntimeParams struct {
	accountID          uuid.UUID
	appID              uuid.UUID
	ownerSubjectID     uuid.UUID
	ownerTeamID        uuid.UUID
	bucketID           uuid.UUID
	bucketName         string
	selections         []models.SDKSelection
	scopeSchemaVersion int
	// Kind and name label the exact version for the app catalogue.
	kind                           store.AppKind
	name                           string
	version                        string
	configKey                      string
	description                    string
	targetLanguage                 string
	sourceHash                     string
	generatorVersion               string
	unifiedDefinitionSchemaVersion int
	unifiedDefinitions             json.RawMessage
	unifiedDefinitionHash          string
	unifiedCodegenDescriptorHash   string
}

// appRuntimeForApply copies compiled private definitions and hashes into the immutable runtime record.
func appRuntimeForApply(p persistAppRuntimeParams) (store.AppRuntime, error) {
	if p.bucketID == uuid.Nil || strings.TrimSpace(p.bucketName) == "" {
		return store.AppRuntime{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "app bucket identity unavailable"}
	}
	if err := validateAppRuntimeSelections(p.scopeSchemaVersion, p.selections); err != nil {
		return store.AppRuntime{}, err
	}
	selections, err := json.Marshal(p.selections)
	if err != nil {
		return store.AppRuntime{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "app selections are invalid"}
	}
	return store.AppRuntime{
		AccountID:                      p.accountID,
		AppID:                          p.appID,
		OwnerSubjectID:                 p.ownerSubjectID,
		OwnerTeamID:                    p.ownerTeamID,
		BucketID:                       p.bucketID,
		Selections:                     selections,
		ScopeSchemaVersion:             p.scopeSchemaVersion,
		UnifiedDefinitionSchemaVersion: p.unifiedDefinitionSchemaVersion,
		UnifiedDefinitions:             append([]byte(nil), p.unifiedDefinitions...),
		UnifiedDefinitionHash:          p.unifiedDefinitionHash,
		UnifiedCodegenDescriptorHash:   p.unifiedCodegenDescriptorHash,
		Kind:                           p.kind,
		Name:                           p.name,
		Version:                        p.version,
		ConfigKey:                      p.configKey,
	}, nil
}

// validateAppRuntimeSelections keeps an incomplete plan or Registry response
// from becoming durable app state that SDK and MCP detail readers cannot use.
func validateAppRuntimeSelections(scopeSchemaVersion int, selections []models.SDKSelection) error {
	if err := models.ValidateAppSelections(scopeSchemaVersion, selections); err != nil {
		return appSelectionSchemaMismatchError()
	}
	return nil
}

func appSelectionSchemaMismatchError() error {
	return workspaceConfigHTTPError{status: http.StatusConflict, message: "app_selection_schema_version_mismatch"}
}

// applyGeneratedAppRuntime persists Unified operation identity atomically while preserving immutability checks.
func applyGeneratedAppRuntime(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	call sdkApplyCall,
	plan *store.ConfigPlan,
	result sdkGenerationResult,
) (string, uuid.UUID, uuid.UUID, bool, error) {
	// DesiredState is an sdkConfigDocument, not the Registry generation payload.
	// sdkConfigDocument (stateDoc, built in resolveSDKSelections), not the
	// GenerateSDKRequest shape plan.ResolvedPayload.
	var doc sdkConfigDocument
	_ = json.Unmarshal(plan.DesiredState, &doc)
	payload, err := appPayloadFromJSON(plan.ResolvedPayload)
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, err
	}
	// Registry resolves concrete operation IDs, while Engine remains the owner
	// of schema identity and immutable service-version bindings.
	selections := finalizeAppSelections(result.Selections, payload.ContractBindings)
	return applyAppConfigPlan(ctx, configStore, s, call, plan, persistAppRuntimeParams{
		accountID:                      call.accountID,
		appID:                          result.AppID,
		ownerSubjectID:                 planOwnerSubjectID(plan),
		ownerTeamID:                    planOwnerTeamID(plan),
		bucketID:                       payload.BucketID,
		bucketName:                     doc.Bucket,
		selections:                     selections,
		scopeSchemaVersion:             result.ScopeSchemaVersion,
		kind:                           store.AppKindSDK,
		name:                           doc.Name,
		version:                        doc.Version,
		configKey:                      fmt.Sprintf("sdk:%s:%s", doc.Name, doc.Version),
		description:                    payload.Description,
		targetLanguage:                 payload.TargetLanguage,
		sourceHash:                     plan.SourceHash,
		generatorVersion:               result.GeneratorVersion,
		unifiedDefinitionSchemaVersion: payload.UnifiedDefinitionSchemaVersion,
		unifiedDefinitions:             payload.UnifiedDefinitions,
		unifiedDefinitionHash:          payload.UnifiedDefinitionHash,
		unifiedCodegenDescriptorHash:   payload.UnifiedCodegenDescriptorHash,
	})
}

func applyAppConfigPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, plan *store.ConfigPlan, params persistAppRuntimeParams) (string, uuid.UUID, uuid.UUID, bool, error) {
	scope, err := appRuntimeForApply(params)
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, err
	}
	return applyAppConfigRuntime(ctx, configStore, s, call, plan, scope, params.bucketName, params.targetLanguage, params.generatorVersion)
}

func applyAppConfigRuntime(ctx context.Context, configStore store.ConfigRepository, s store.Store, call sdkApplyCall, plan *store.ConfigPlan, scope store.AppRuntime, authorizedBucketName, targetLanguage, generatorVersion string) (string, uuid.UUID, uuid.UUID, bool, error) {
	rawToken, tokenHash, err := applifecycle.NewExecutionToken()
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to issue sdk execution credential"}
	}
	result, err := applifecycle.New(s).ApplyConfigPlan(ctx, configStore, store.ApplyAppConfigPlanParams{
		Plan: store.ApplyConfigPlanParams{
			State: store.UpsertConfigStateParams{
				ConfigKey: plan.ConfigKey, ConfigType: plan.ConfigType, SourceHash: plan.SourceHash,
				DesiredState: plan.DesiredState, ManagedResources: []byte("{}"), LatestResourceID: &scope.AppID, UpdatedBy: call.accountID,
			},
			PlanID: call.planID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: call.planRevision,
			ApplyLeaseID: call.applyLeaseID,
		},
		Scope: scope, AuthorizedBucketName: authorizedBucketName,
		TokenHash: tokenHash, TokenName: "default", TokenPolicy: applifecycle.FullAccessTokenPolicy(),
		TokenIssuedBySubjectID: optionalActorID(call.actor.SubjectID), TokenIssuedByCredentialID: optionalActorID(call.actor.CredentialID),
		TargetLanguage:   targetLanguage,
		GeneratorVersion: generatorVersion,
	})
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, appApplyPersistenceError(ctx, err, scope.AppID)
	}
	notifyAppRuntimeChanged(ctx, s, scope.AppID)
	if !result.TokenCreated {
		return "", result.AppFamilyID, result.AppID, result.VersionCreated, nil
	}
	return rawToken, result.AppFamilyID, result.AppID, result.VersionCreated, nil
}

type appRuntimeChangeNotifier interface {
	NotifyAppRuntimeChanged(context.Context, uuid.UUID)
}

func notifyAppRuntimeChanged(ctx context.Context, s store.Store, appID uuid.UUID) {
	if notifier, ok := s.(appRuntimeChangeNotifier); ok {
		notifier.NotifyAppRuntimeChanged(ctx, appID)
	}
}

func appApplyPersistenceError(ctx context.Context, err error, appID uuid.UUID) error {
	slog.ErrorContext(ctx, "app config apply persistence failed", slog.Any("error", err), slog.String("app.id", appID.String()))
	if errors.Is(err, store.ErrSDKBucketImmutable) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "app bucket assignment is immutable"}
	}
	if errors.Is(err, store.ErrConfigPlanNotFound) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale_or_mismatched"}
	}
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply app config"}
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

// validateSDKGenerationResult rejects malformed sdk generation result before it can cross the Unified operation boundary.
func validateSDKGenerationResult(payload json.RawMessage, call sdkApplyCall, result models.SDKGenerationResult) error {
	if err := validateSDKGenerationResultEnvelope(call, result); err != nil {
		return err
	}
	var request GenerateSDKRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid sdk generation payload"}
	}
	// A skipped result is only trustworthy when this Engine actually asked for
	// one. Registry silently dropping packaging for a request that wanted a
	// package would otherwise publish a version with nothing to download.
	if result.Status == models.SDKGenerationStatusSkipped && !request.SkipPackaging {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_unexpectedly_skipped"}
	}
	if err := validateGeneratedScopeSelections(request.Selections, result.Selections); err != nil {
		return err
	}
	return validateGeneratedUnifiedTargets(request.UnifiedOperations, result.Selections)
}

// validateSDKGenerationResultEnvelope rejects malformed sdk generation result envelope before it can cross the Unified operation boundary.
func validateSDKGenerationResultEnvelope(call sdkApplyCall, result models.SDKGenerationResult) error {
	if result.AppID == uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "app_id_required"}
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
	// Skipped is terminal and legitimate: the request asked for the resolved
	// contract without a package, and Registry closed the job out rather than
	// scheduling codegen. The scope assertions below still apply to it.
	if result.Status != models.SDKGenerationStatusPending && result.Status != models.SDKGenerationStatusComplete &&
		result.Status != models.SDKGenerationStatusSkipped {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_status_invalid"}
	}
	if result.ScopeSchemaVersion != models.AppScopeSchemaVersion {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_schema_version_mismatch"}
	}
	return nil
}

// validateGeneratedUnifiedTargets proves Registry returned every exact forward
// and rollback endpoint compiled into the immutable Unified descriptor.
func validateGeneratedUnifiedTargets(descriptors *models.SDKUnifiedOperationDescriptors, selections []models.SDKSelection) error {
	if descriptors == nil {
		return nil
	}
	returned, err := concreteEndpointIDsByServiceVersion(selections)
	if err != nil {
		return err
	}
	for _, operation := range descriptors.Operations {
		for _, target := range operation.Targets {
			if !generatedUnifiedEndpointReturned(returned, target.ServiceID, target.ServiceVersionID, target.EndpointID) {
				return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_unified_target_mismatch"}
			}
			// compensation can execute after generation, so missing rollback
			// scope must fail before the package becomes downloadable.
			if target.Rollback != nil && !generatedUnifiedEndpointReturned(returned, target.Rollback.ServiceID, target.Rollback.ServiceVersionID, target.Rollback.EndpointID) {
				return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_unified_target_mismatch"}
			}
		}
	}
	return nil
}

// generatedUnifiedEndpointReturned checks one exact endpoint in the bounded
// service-version selection map built from Registry output.
func generatedUnifiedEndpointReturned(returned map[[2]uuid.UUID]map[uuid.UUID]bool, serviceID, versionID, endpointID uuid.UUID) bool {
	return returned[[2]uuid.UUID{serviceID, versionID}][endpointID]
}

// concreteEndpointIDsByServiceVersion indexes returned Registry scope by exact service/version/endpoint identity.
func concreteEndpointIDsByServiceVersion(selections []models.SDKSelection) (map[[2]uuid.UUID]map[uuid.UUID]bool, error) {
	result := make(map[[2]uuid.UUID]map[uuid.UUID]bool, len(selections))
	for _, selection := range selections {
		key := [2]uuid.UUID{selection.ServiceID, selection.ServiceVersionID}
		if _, exists := result[key]; exists {
			return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
		}
		ids := make(map[uuid.UUID]bool, len(selection.EndpointIDs))
		for _, id := range selection.EndpointIDs {
			ids[id] = true
		}
		result[key] = ids
	}
	return result, nil
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
	if invalidConcreteReturnedShape(returned) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	if explicitEndpointSelectionChanged(planned, returned) {
		return endpointSelectionMismatch(planned, returned)
	}
	if explicitWebhookSelectionChanged(planned, returned) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	if !sameReturnedSelectionPolicy(planned, returned) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_scope_selection_mismatch"}
	}
	return nil
}

func invalidConcreteReturnedShape(selection models.SDKSelection) bool {
	if selection.SelectAll || selection.WebhookSelectAll {
		return true
	}
	if len(selection.EndpointIDs)+len(selection.WebhookIDs) == 0 {
		return true
	}
	return hasDuplicateUUID(selection.EndpointIDs) || hasDuplicateUUID(selection.WebhookIDs)
}

func explicitEndpointSelectionChanged(planned, returned models.SDKSelection) bool {
	return len(planned.EndpointIDs) > 0 && !sameUUIDSet(planned.EndpointIDs, returned.EndpointIDs)
}

func explicitWebhookSelectionChanged(planned, returned models.SDKSelection) bool {
	return len(planned.WebhookIDs) > 0 && !sameUUIDSet(planned.WebhookIDs, returned.WebhookIDs)
}

func sameReturnedSelectionPolicy(planned, returned models.SDKSelection) bool {
	if len(planned.OperationNames) > 0 && !sameStrings(planned.OperationNames, returned.OperationNames) {
		return false
	}
	if len(planned.WebhookNames) > 0 && !sameStrings(planned.WebhookNames, returned.WebhookNames) {
		return false
	}
	return planned.AuthType == returned.AuthType && planned.AuthName == returned.AuthName &&
		sameRequiredAuth(planned.RequiredAuth, returned.RequiredAuth) &&
		sameStrings(planned.ConnectScopes, returned.ConnectScopes) && sameInjections(planned.Injections, returned.Injections)
}

func sameRequiredAuth(expected, actual []models.SDKRequiredAuth) bool {
	want, _ := json.Marshal(expected)
	got, _ := json.Marshal(actual)
	return bytes.Equal(want, got)
}

func sameStrings(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	want := stringSet(expected)
	for _, value := range actual {
		if !want[value] {
			return false
		}
		delete(want, value)
	}
	return len(want) == 0
}

func sameInjections(expected, actual []models.SDKInjectionConfig) bool {
	want, _ := json.Marshal(expected)
	got, _ := json.Marshal(actual)
	return bytes.Equal(want, got)
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

func writeSDKConfigError(w http.ResponseWriter, err error, contexts ...context.Context) {
	if isConfigAuthorizationError(err) {
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
			details:   map[string]any{"stage": "registry_generation", "http_status": proxyErr.status},
		}, contexts...)
		return
	}
	writeWorkspaceConfigError(w, err, contexts...)
}
