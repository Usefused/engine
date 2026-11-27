package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

const (
	// RegistryTargetHeader allows CLI/UI callers to opt into 'cloud' (or 'local') when Hybrid mode is configured.
	RegistryTargetHeader = "X-Fused-Registry-Target"
	// RegistrySourceHeader reports whether 'local' or 'cloud' Registry fulfilled a proxied request.
	RegistrySourceHeader = "X-Fused-Registry-Source"

	maxHybridGraphQLBodyBytes = 4 << 20  // 4 MiB ceiling for proxied GraphQL queries
	maxHybridSDKBodyBytes     = 2 << 20  // 2 MiB ceiling for incoming SDK generation requests before snapshot enrichment
	maxHybridImportBodyBytes  = 32 << 20 // 32 MiB ceiling for spec import payloads inspected for --cloud visibility
)

// HybridRegistryProxy routes Engine proxy requests Local-first with strict permission-gated Cloud fallback.
type HybridRegistryProxy struct {
	cloud *RegistryProxy
	local *RegistryProxy
	store store.Store
}

// NewHybridRegistryProxy binds Local and Cloud Registry proxies behind the shared Forwarder contract.
func NewHybridRegistryProxy(cloud, local *RegistryProxy, s store.Store) *HybridRegistryProxy {
	return &HybridRegistryProxy{cloud: cloud, local: local, store: s}
}

// Forward dispatches GraphQL and REST requests according to Enterprise entitlement, pinned service origin, and Cloud permissions.
func (p *HybridRegistryProxy) Forward(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	// Degrade cleanly to Cloud-only forwarding when no Local Registry proxy is configured.
	if p.local == nil {
		p.cloud.Forward(w, r, stripPrefix)
		return
	}
	// Account, billing, leads, and legacy credit routes always belong to Fused Cloud Registry.
	if isCloudOnlyProxyPath(r.URL.Path) {
		w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
		p.cloud.Forward(w, stripTargetHeader(r), stripPrefix)
		return
	}
	if r.URL.Path == "/graphql" {
		p.forwardHybridGraphQL(w, r, stripPrefix)
		return
	}
	if isImportOrDiscoveryPath(r.URL.Path) {
		p.forwardHybridImport(w, r, stripPrefix)
		return
	}
	// Route SDK generation to Cloud only when every selected service is Cloud-pinned; otherwise compile on Local Registry.
	if isSDKGeneratePath(r.Method, r.URL.Path) {
		p.forwardHybridSDKGenerate(w, r, stripPrefix)
		return
	}
	p.forwardHybridServiceREST(w, r, stripPrefix)
}

// ForwardAndInspect enforces Hybrid import/delete routing while preserving post-2xx response inspection.
func (p *HybridRegistryProxy) ForwardAndInspect(w http.ResponseWriter, r *http.Request, stripPrefix string, onSuccess func(response *http.Response, body []byte)) {
	if p.local == nil {
		p.cloud.ForwardAndInspect(w, r, stripPrefix, onSuccess)
		return
	}
	target, ok := p.admitImportOrMutationTarget(w, r)
	if !ok {
		return
	}
	cleaned := stripTargetHeader(r.WithContext(sandbox.WithRegistryTarget(r.Context(), target)))
	w.Header().Set(RegistrySourceHeader, target)
	if target == sandbox.RegistrySourceCloud {
		p.cloud.ForwardAndInspect(w, cleaned, stripPrefix, onSuccess)
		return
	}
	p.local.ForwardAndInspect(w, cleaned, stripPrefix, onSuccess)
}

// AdmitImportApplyTarget validates Enterprise Local Registry or --cloud permission before import preflight runs.
func AdmitImportApplyTarget(proxy Forwarder, w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	hybrid, ok := proxy.(*HybridRegistryProxy)
	// Non-hybrid Engines always target Fused Cloud Registry without extra target headers.
	if !ok || hybrid.local == nil {
		return r, true
	}
	target, admitted := hybrid.admitImportTarget(w, r, false)
	if !admitted {
		return nil, false
	}
	return r.WithContext(sandbox.WithRegistryTarget(r.Context(), target)), true
}

// forwardHybridGraphQL executes Local-first GraphQL reads with strict permission-gated Cloud fallback on misses.
func (p *HybridRegistryProxy) forwardHybridGraphQL(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	explicitTarget := normalizedRegistryTarget(r.Header.Get(RegistryTargetHeader))
	if explicitTarget == sandbox.RegistrySourceCloud {
		if !requireCloudPermission(w, r.Context(), accesscontrol.PermissionCatalogueCloudRead) {
			return
		}
		w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
		p.cloud.Forward(w, stripTargetHeader(r), stripPrefix)
		return
	}
	if !requireLocalRegistryEntitled(w, r.Context()) {
		return
	}
	body, ok := readBoundedProxyBody(w, r, maxHybridGraphQLBodyBytes, "invalid_graphql_request_body", "The GraphQL request body could not be read.")
	if !ok {
		return
	}

	localReq := cloneRequestWithBody(stripTargetHeader(r), body)
	localRec := newBufferedResponseWriter()
	p.local.Forward(localRec, localReq, stripPrefix)

	// Return immediately when Local Registry succeeded with non-empty data or when Cloud fallback is disallowed.
	if localRec.status != http.StatusOK || !graphqlResponseIsMiss(localRec.body.Bytes()) || explicitTarget == sandbox.RegistrySourceLocal || !ActorHasCloudReadPermission(r.Context()) {
		localRec.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceLocal)
		localRec.flushTo(w)
		return
	}

	cloudReq := cloneRequestWithBody(stripTargetHeader(r), body)
	w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
	p.cloud.Forward(w, cloudReq, stripPrefix)
}

// forwardHybridImport routes spec imports to Local Registry by default, allowing --cloud only with permission and public visibility.
func (p *HybridRegistryProxy) forwardHybridImport(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	enforcePublicOnCloud := r.Method == http.MethodPost && (r.URL.Path == "/integrations/import/plan" || r.URL.Path == "/integrations/start")
	target, ok := p.admitImportTarget(w, r, enforcePublicOnCloud)
	if !ok {
		return
	}
	cleaned := stripTargetHeader(r)
	w.Header().Set(RegistrySourceHeader, target)
	if target == sandbox.RegistrySourceCloud {
		p.cloud.Forward(w, cleaned, stripPrefix)
		return
	}
	p.local.Forward(w, cleaned, stripPrefix)
}

// admitImportTarget checks either Enterprise Local Registry entitlement or Cloud publish permission + public visibility.
func (p *HybridRegistryProxy) admitImportTarget(w http.ResponseWriter, r *http.Request, enforcePublicOnCloud bool) (string, bool) {
	target := normalizedRegistryTarget(r.Header.Get(RegistryTargetHeader))
	if target == sandbox.RegistrySourceCloud {
		if !requireCloudPermission(w, r.Context(), accesscontrol.PermissionCatalogueCloudPublish) {
			return "", false
		}
		if enforcePublicOnCloud && !ensurePublicCloudImportBody(w, r) {
			return "", false
		}
		return sandbox.RegistrySourceCloud, true
	}
	if !requireLocalRegistryEntitled(w, r.Context()) {
		return "", false
	}
	return sandbox.RegistrySourceLocal, true
}

// admitImportOrMutationTarget determines whether a mutating ForwardAndInspect request targets Local or Cloud Registry.
func (p *HybridRegistryProxy) admitImportOrMutationTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	if isImportOrDiscoveryPath(r.URL.Path) {
		return p.admitImportTarget(w, r, false)
	}
	serviceID := extractIntegrationServiceID(r.URL.Path)
	if serviceID != uuid.Nil && p.lookupServiceSource(r.Context(), serviceID) == sandbox.RegistrySourceCloud {
		if !requireCloudPermission(w, r.Context(), accesscontrol.PermissionCatalogueCloudPublish) {
			return "", false
		}
		return sandbox.RegistrySourceCloud, true
	}
	if !requireLocalRegistryEntitled(w, r.Context()) {
		return "", false
	}
	return sandbox.RegistrySourceLocal, true
}

// forwardHybridServiceREST routes service-scoped and SDK REST paths to the owning Registry or Local-first with Cloud fallback.
func (p *HybridRegistryProxy) forwardHybridServiceREST(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	if normalizedRegistryTarget(r.Header.Get(RegistryTargetHeader)) == sandbox.RegistrySourceCloud {
		if !requireCloudPermission(w, r.Context(), cloudPermissionForMethod(r.Method)) {
			return
		}
		w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
		p.cloud.Forward(w, stripTargetHeader(r), stripPrefix)
		return
	}
	if !requireLocalRegistryEntitled(w, r.Context()) {
		return
	}
	serviceID := extractIntegrationServiceID(r.URL.Path)
	if serviceID != uuid.Nil && p.lookupServiceSource(r.Context(), serviceID) == sandbox.RegistrySourceCloud {
		// Enforce catalogue.cloud.publish on mutating requests against Cloud-pinned services and catalogue.cloud.read on reads.
		if !requireCloudPermission(w, r.Context(), cloudPermissionForMethod(r.Method)) {
			return
		}
		w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
		p.cloud.Forward(w, stripTargetHeader(r), stripPrefix)
		return
	}
	// Read-only unpinned GETs try Local first and fall back to Cloud on 404 when permitted.
	if !isMutatingMethod(r.Method) {
		p.forwardHybridGETWithFallback(w, r, stripPrefix)
		return
	}
	// Package compensation deletes try Local first and fall back to Cloud on 404 when permitted.
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sdk-packages/") {
		p.forwardHybridGETWithFallback(w, r, stripPrefix)
		return
	}
	w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceLocal)
	p.local.Forward(w, stripTargetHeader(r), stripPrefix)
}

// cloudPermissionForMethod maps HTTP methods to catalogue.cloud.publish for mutations and catalogue.cloud.read for reads.
func cloudPermissionForMethod(method string) accesscontrol.Permission {
	// Mutating requests against Fused Cloud Registry require explicit publish permission.
	if isMutatingMethod(method) {
		return accesscontrol.PermissionCatalogueCloudPublish
	}
	return accesscontrol.PermissionCatalogueCloudRead
}

// forwardHybridSDKGenerate keeps internal service contracts private by compiling mixed or Local SDKs on Local Registry.
func (p *HybridRegistryProxy) forwardHybridSDKGenerate(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	if !requireLocalRegistryEntitled(w, r.Context()) {
		return
	}
	body, ok := readBoundedProxyBody(w, r, maxHybridSDKBodyBytes, "invalid_sdk_generate_request", "The SDK generation request body could not be read.")
	// Reject unreadable or oversized SDK generation payloads before dispatching upstream.
	if !ok {
		return
	}
	allCloud, cloudRefs := p.classifySDKGenerateSelections(r.Context(), body)
	// Purely Cloud-pinned SDKs compile on Fused Cloud Registry when the caller has Cloud read permission.
	if allCloud {
		if !requireCloudPermission(w, r.Context(), accesscontrol.PermissionCatalogueCloudRead) {
			return
		}
		sanitized := stripCallerInlineContracts(body)
		w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
		p.cloud.Forward(w, cloneRequestWithBody(stripTargetHeader(r), sanitized), stripPrefix)
		return
	}
	enriched, enrichErr := p.enrichSDKGenerateBodyWithCloudSnapshots(r.Context(), body, cloudRefs)
	// Fail closed if a mixed SDK references a Cloud service whose local snapshot is unavailable.
	if enrichErr != nil {
		writeControlAPIError(w, r.Context(), http.StatusConflict, "sdk_cloud_snapshot_unavailable", "A Cloud-sourced service snapshot required for mixed SDK generation is unavailable.", "Refresh workspace services and retry SDK generation.")
		return
	}
	w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceLocal)
	p.local.Forward(w, cloneRequestWithBody(stripTargetHeader(r), enriched), stripPrefix)
}

// classifySDKGenerateSelections inspects SDK selections to detect pure-Cloud vs mixed Local+Cloud builds.
func (p *HybridRegistryProxy) classifySDKGenerateSelections(ctx context.Context, body []byte) (bool, []store.ServiceContractMetadataRef) {
	var req models.SDKGenerationRequest
	// Leave malformed or empty selection lists for Local Registry's own request validator.
	if json.Unmarshal(body, &req) != nil || len(req.Selections) == 0 {
		return false, nil
	}
	regStore, ok := p.store.(store.WorkspaceServiceRegistryStore)
	// Without a provenance store, keep SDK compilation strictly on Local Registry.
	if !ok {
		return false, nil
	}
	ids := make([]uuid.UUID, 0, len(req.Selections))
	for _, sel := range req.Selections {
		ids = append(ids, sel.ServiceID)
	}
	sources, err := regStore.GetWorkspaceServiceRegistrySources(ctx, ids)
	// Treat lookup errors as Local-owned to prevent leaking internal identifiers to Cloud.
	if err != nil {
		return false, nil
	}
	var cloudRefs []store.ServiceContractMetadataRef
	for _, sel := range req.Selections {
		if sources[sel.ServiceID] == sandbox.RegistrySourceCloud {
			cloudRefs = append(cloudRefs, store.ServiceContractMetadataRef{
				ServiceID:        sel.ServiceID,
				ServiceVersionID: sel.ServiceVersionID,
			})
		}
	}
	return len(cloudRefs) == len(req.Selections), cloudRefs
}

// enrichSDKGenerateBodyWithCloudSnapshots strips any untrusted client inline_contracts and embeds Engine-admitted Cloud snapshots.
func (p *HybridRegistryProxy) enrichSDKGenerateBodyWithCloudSnapshots(ctx context.Context, body []byte, cloudRefs []store.ServiceContractMetadataRef) ([]byte, error) {
	var req models.SDKGenerationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}
	hadCallerInline := len(req.InlineContracts) > 0
	// Never trust caller-supplied inline_contracts; Engine solely authors inline contracts from admitted local snapshots.
	req.InlineContracts = nil
	// Purely Local SDK builds require no inline snapshots, rewriting only if untrusted inline_contracts were stripped.
	if len(cloudRefs) == 0 {
		if hadCallerInline {
			return json.Marshal(req)
		}
		return body, nil
	}
	inlineStore, ok := p.store.(store.InlineSDKContractStore)
	// Mixed SDK builds must fail closed when the store cannot export admitted Cloud snapshots.
	if !ok {
		return nil, errors.New("inline SDK contract snapshot store unavailable")
	}
	contracts, err := inlineStore.BuildInlineSDKServiceContracts(ctx, cloudRefs)
	if err != nil {
		return nil, err
	}
	req.InlineContracts = contracts
	return json.Marshal(req)
}

// forwardHybridGETWithFallback serves Local 2xx/non-404 responses directly and falls back to Cloud on 404 if permitted.
func (p *HybridRegistryProxy) forwardHybridGETWithFallback(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	localRec := newBufferedResponseWriter()
	p.local.Forward(localRec, stripTargetHeader(r), stripPrefix)
	if localRec.status != http.StatusNotFound || !ActorHasCloudReadPermission(r.Context()) {
		localRec.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceLocal)
		localRec.flushTo(w)
		return
	}
	w.Header().Set(RegistrySourceHeader, sandbox.RegistrySourceCloud)
	p.cloud.Forward(w, stripTargetHeader(r), stripPrefix)
}

// lookupServiceSource reads the workspace's pinned Registry source ('local' or 'cloud') for serviceID.
func (p *HybridRegistryProxy) lookupServiceSource(ctx context.Context, serviceID uuid.UUID) string {
	regStore, ok := p.store.(store.WorkspaceServiceRegistryStore)
	if !ok || serviceID == uuid.Nil {
		return ""
	}
	sources, err := regStore.GetWorkspaceServiceRegistrySources(ctx, []uuid.UUID{serviceID})
	if err != nil {
		return ""
	}
	return sources[serviceID]
}

// ActorHasCloudReadPermission reports whether ctx is either a background worker or an actor holding catalogue.cloud.read.
func ActorHasCloudReadPermission(ctx context.Context) bool {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	// Internal Engine background workers carry no control actor and remain authorized for already-activated Cloud services.
	if !ok {
		return true
	}
	req := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionCatalogueCloudRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	return accesscontrol.AuthorizeAll(ctx, accesscontrol.SnapshotAuthorizer{}, req) == nil
}

// requireCloudPermission enforces a Cloud Registry workspace permission and writes a structured 403 on denial.
func requireCloudPermission(w http.ResponseWriter, ctx context.Context, permission accesscontrol.Permission) bool {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		writeControlAPIError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required.", "Log in or provide a valid Fused credential.")
		return false
	}
	req := accesscontrol.Requirement{
		Permission: permission,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
	}
	accesscontrol.CaptureRequiredPermissions(ctx, []accesscontrol.Requirement{req})
	if err := accesscontrol.AuthorizeAll(ctx, accesscontrol.SnapshotAuthorizer{}, req); err != nil {
		accesscontrol.CaptureMissingPermissions(ctx, accesscontrol.MissingRequirements(err))
		accesscontrol.WriteAuthorizationError(w, err, ctx)
		return false
	}
	return true
}

// requireLocalRegistryEntitled enforces live Enterprise license entitlement before dispatching to Local Registry.
func requireLocalRegistryEntitled(w http.ResponseWriter, ctx context.Context) bool {
	if entitlement.LiveEntitlement.Load().LocalRegistryEnabled {
		return true
	}
	writeControlAPIError(w, ctx, http.StatusForbidden, "local_registry_enterprise_required", "Local Registry requires an active Enterprise license.", "Contact Fused to enable Enterprise Local Registry access on your license.")
	return false
}

// ensurePublicCloudImportBody rejects --cloud import plan/start payloads unless is_public is explicitly true.
func ensurePublicCloudImportBody(w http.ResponseWriter, r *http.Request) bool {
	body, ok := readBoundedProxyBody(w, r, maxHybridImportBodyBytes, "invalid_import_request", "The import request body could not be read.")
	if !ok {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var payload struct {
		IsPublic *bool `json:"is_public"`
	}
	// Require explicit is_public=true so private internal specs are never uploaded to Fused Cloud via --cloud.
	if json.Unmarshal(body, &payload) != nil || payload.IsPublic == nil || !*payload.IsPublic {
		writeControlAPIError(w, r.Context(), http.StatusBadRequest, "cloud_import_requires_public", "Publishing to Fused Cloud Registry with --cloud requires public visibility (--public).", "Omit --cloud to keep internal specifications in your Local Registry, or pass --public to publish publicly.")
		return false
	}
	return true
}

// readBoundedProxyBody reads r.Body up to maxBytes, returning HTTP 413 on overflow and HTTP 400 on read failure.
func readBoundedProxyBody(w http.ResponseWriter, r *http.Request, maxBytes int64, badRequestCode, badRequestMsg string) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		// Reject oversized payloads explicitly with 413 instead of buffering arbitrary request bodies in memory.
		if errors.As(err, &maxErr) {
			writeControlAPIError(w, r.Context(), http.StatusRequestEntityTooLarge, "request_body_too_large", "The request body exceeds the allowed size limit.", "Reduce the payload size and retry.")
			return nil, false
		}
		writeControlAPIError(w, r.Context(), http.StatusBadRequest, badRequestCode, badRequestMsg, "Send a valid JSON payload.")
		return nil, false
	}
	return body, true
}

// graphqlResponseIsMiss reports whether a 200 GraphQL response contains no Errors and only null or empty root results.
func graphqlResponseIsMiss(body []byte) bool {
	var envelope struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []json.RawMessage          `json:"errors"`
	}
	// Responses with GraphQL errors or missing data envelopes are authoritative and never fall back.
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Errors) > 0 || len(envelope.Data) == 0 {
		return false
	}
	checked := 0
	for field, raw := range envelope.Data {
		if field == "__typename" {
			continue
		}
		checked++
		if !jsonValueIsEmpty(raw) {
			return false
		}
	}
	return checked > 0
}

// stripCallerInlineContracts removes any client-supplied inline_contracts field before forwarding an SDK build request.
func stripCallerInlineContracts(body []byte) []byte {
	var raw map[string]json.RawMessage
	// Leave malformed JSON untouched so Registry's strict decoder rejects it cleanly.
	if json.Unmarshal(body, &raw) != nil {
		return body
	}
	// Skip re-encoding when the caller did not send an inline_contracts field.
	if _, exists := raw["inline_contracts"]; !exists {
		return body
	}
	delete(raw, "inline_contracts")
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return cleaned
}

// jsonValueIsEmpty recognizes null, empty string, empty array, and empty pagination objects (items:[] or data:[]).
func jsonValueIsEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) || bytes.Equal(trimmed, []byte("[]")) {
		return true
	}
	var page struct {
		Items *[]json.RawMessage `json:"items"`
		Data  *[]json.RawMessage `json:"data"`
	}
	// Paged GraphQL responses use {"items":[]} or {"data":[],"total":0} when no records match.
	if trimmed[0] == '{' && json.Unmarshal(trimmed, &page) == nil {
		if (page.Items != nil && len(*page.Items) == 0) || (page.Data != nil && len(*page.Data) == 0) {
			return true
		}
	}
	return false
}

// isCloudOnlyProxyPath identifies account, billing, lead, and credit routes owned exclusively by Fused Cloud Registry.
func isCloudOnlyProxyPath(path string) bool {
	return path == "/account" || strings.HasPrefix(path, "/account/") ||
		path == "/leads" || path == "/credits" || strings.HasPrefix(path, "/credits/")
}

// isImportOrDiscoveryPath identifies specification import and discovery session routes.
func isImportOrDiscoveryPath(path string) bool {
	return path == "/integrations/preview_openapi" ||
		strings.HasPrefix(path, "/integrations/import/") ||
		path == "/integrations/start" ||
		path == "/integrations/sessions/active" ||
		strings.HasPrefix(path, "/integrations/session/")
}

// extractIntegrationServiceID parses the UUID segment from /integrations/{service_id}(/...).
func extractIntegrationServiceID(path string) uuid.UUID {
	if !strings.HasPrefix(path, "/integrations/") {
		return uuid.Nil
	}
	rest := strings.TrimPrefix(path, "/integrations/")
	segment, _, _ := strings.Cut(rest, "/")
	id, err := uuid.Parse(segment)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// normalizedRegistryTarget canonicalizes X-Fused-Registry-Target header values to 'local', 'cloud', or ''.
func normalizedRegistryTarget(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case sandbox.RegistrySourceLocal:
		return sandbox.RegistrySourceLocal
	case sandbox.RegistrySourceCloud:
		return sandbox.RegistrySourceCloud
	default:
		return ""
	}
}

// stripTargetHeader clones r without Engine-local routing headers before forwarding upstream.
func stripTargetHeader(r *http.Request) *http.Request {
	cloned := r.Clone(r.Context())
	cloned.Header = r.Header.Clone()
	cloned.Header.Del(RegistryTargetHeader)
	return cloned
}

// cloneRequestWithBody creates an independent replayable HTTP request with a fresh body reader.
func cloneRequestWithBody(r *http.Request, body []byte) *http.Request {
	cloned := r.Clone(r.Context())
	cloned.Header = r.Header.Clone()
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.ContentLength = int64(len(body))
	return cloned
}
