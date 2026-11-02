package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type runtimeOAuthProvider interface {
	OAuthProviderService
	AuthorizeRuntime(context.Context, accesscontrol.Actor, oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error)
	ConsentRuntime(context.Context, accesscontrol.Actor, oauthprovider.ConsentRequest, store.OAuthRuntimeGrant) (string, error)
	TokenRuntime(context.Context, oauthprovider.TokenRequest) (oauthprovider.RuntimeTokenResponse, error)
	RevokeRuntime(context.Context, oauthprovider.RevokeRequest) error
}

type runtimeAccessManifest struct {
	SchemaVersion int                               `json:"schema_version"`
	Name          string                            `json:"name"`
	Version       string                            `json:"version"`
	Bucket        string                            `json:"bucket,omitempty"`
	Services      map[string]runtimeServiceManifest `json:"services"`
}

type runtimeServiceManifest struct {
	Version    string   `json:"version"`
	Operations []string `json:"operations"`
}

type runtimeOAuthHandler struct {
	service   runtimeOAuthProvider
	store     store.Store
	configs   store.ConfigRepository
	registry  sandbox.RegistryClient
	publicURL string
}

type runtimeConsentPage struct {
	Management  bool
	Name        string
	Version     string
	Digest      string
	Services    map[string]runtimeServiceManifest
	Buckets     []store.BucketSummary
	FixedBucket *store.BucketSummary
}

// MountRuntimeOAuthRoutes reuses browser-session security while provisioning ordinary API apps.
func MountRuntimeOAuthRoutes(router chi.Router, provider OAuthProviderService, s store.Store, configs store.ConfigRepository, registry sandbox.RegistryClient, sessions BrowserSessionService, cookies *browserauth.CookieManager, publicURL string) {
	service, ok := provider.(runtimeOAuthProvider)
	// Unsupported providers cannot expose a partially functioning provisioning surface.
	if !ok || service == nil {
		return
	}
	h := &runtimeOAuthHandler{service: service, store: s, configs: configs, registry: registry, publicURL: strings.TrimRight(publicURL, "/")}
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).Post("/oauth/runtime/register", h.register)
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).Get("/oauth/runtime/authorize", oauthAuthorizeHandler(h, s, sessions, cookies, "/login"))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).Post("/oauth/runtime/authorize/consent", oauthConsentHandler(h, s, sessions, cookies, maxOAuthFormBytes))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(60, 600, time.Minute))).Post("/oauth/runtime/token", h.token)
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(60, 600, time.Minute))).Post("/oauth/runtime/revoke", h.revoke)
	// This endpoint authenticates its own family-bound deployment token, never a generic management credential.
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).Post("/oauth/runtime/deploy", h.deploy)
}

// parseRuntimeManifest accepts bounded, explicit requirements and reuses API-app declaration validation.
func parseRuntimeManifest(raw []byte) (runtimeAccessManifest, error) {
	var manifest runtimeAccessManifest
	// A compiler manifest cannot carry arbitrary SDK config, credentials, or select-all permissions.
	if len(raw) == 0 || len(raw) > 64<<10 || decodeOneStrictJSON(bytes.NewReader(raw), &manifest) != nil || manifest.SchemaVersion != 1 || len(manifest.Services) == 0 || len(manifest.Services) > 64 {
		return manifest, oauthprovider.ErrInvalidRequest
	}
	// Operation names must remain unambiguous on the existing endpoint-name protocol.
	if err := validateRuntimeOperations(manifest.Services); err != nil {
		return manifest, err
	}
	// Caller-selected routing is descriptive until resolved through the consenting actor’s bucket-use scope.
	if len(manifest.Bucket) > 255 || strings.TrimSpace(manifest.Bucket) != manifest.Bucket || strings.ContainsAny(manifest.Bucket, "\x00\r\n\t") || manifest.Bucket == "*" {
		return manifest, oauthprovider.ErrInvalidRequest
	}
	doc := manifest.document("validation-only", nil)
	// Existing SDK/API validation remains the single authority for names, versions, and service selections.
	if err := validateSDKConfigDocument(doc); err != nil {
		return manifest, oauthprovider.ErrInvalidRequest
	}
	return manifest, nil
}

// validateRuntimeOperations enforces one bounded, unambiguous operation set across every selected service.
func validateRuntimeOperations(services map[string]runtimeServiceManifest) error {
	all := map[string]bool{}
	// Compare endpoints across services because the execution protocol uses one endpoint namespace.
	for _, service := range services {
		// Every selected operation contributes to the exact, globally bounded set.
		for _, operation := range service.Operations {
			// An endpoint must be exact and unique, rather than relying on service iteration order.
			if !validRuntimeOperation(operation) || all[operation] {
				return oauthprovider.ErrInvalidRequest
			}
			all[operation] = true
		}
	}
	// Empty and unbounded scopes cannot create a consent request.
	if len(all) == 0 || len(all) > 1000 {
		return oauthprovider.ErrInvalidRequest
	}
	return nil
}

// validRuntimeOperation rejects wildcard and ambiguous public endpoint spellings.
func validRuntimeOperation(operation string) bool {
	return operation != "" && operation != "*" && len(operation) <= 512 && strings.TrimSpace(operation) == operation
}

// document selects existing API delivery without introducing an app kind or invoking package generation.
func (m runtimeAccessManifest) document(bucket string, runtimeBuckets []string) sdkConfigDocument {
	generate := false
	services := make(map[string]sdkConfigServiceDoc, len(m.Services))
	// Preserve compiler selections exactly when adapting to the existing app declaration.
	for name, service := range m.Services {
		services[name] = sdkConfigServiceDoc{Version: service.Version, Operations: service.Operations}
	}
	return sdkConfigDocument{APIVersion: "fused/v1", Kind: "sdk", Name: m.Name, Version: m.Version, Language: "typescript", Generate: &generate, Bucket: bucket, RuntimeBuckets: runtimeBuckets, Services: services}
}

// register uses the shared registration body bounds, secret headers, and protocol errors.
func (h *runtimeOAuthHandler) register(w http.ResponseWriter, r *http.Request) {
	oauthRegistrationHandler(128<<10, h.registrationRequest)(w, r)
}

// registrationRequest binds exact compiler bytes to a provisional client without granting management access.
func (h *runtimeOAuthHandler) registrationRequest(r *http.Request) (any, error) {
	var input struct {
		Name        string `json:"name"`
		RedirectURI string `json:"redirect_uri"`
		Manifest    string `json:"manifest"`
		Management  bool   `json:"management,omitempty"`
	}
	// Unknown fields and trailing JSON cannot widen the registered requirement set.
	if decodeOneStrictJSON(r.Body, &input) != nil {
		return nil, oauthprovider.ErrInvalidRequest
	}
	manifest, err := parseRuntimeManifest([]byte(input.Manifest))
	// Invalid manifests cannot consume registration quota or receive a credential pair.
	if err != nil {
		return nil, err
	}
	// An omitted label inherits the actual application identity displayed during approval.
	if input.Name == "" {
		input.Name = manifest.Name
	}
	scope := store.OAuthRuntimeScope
	// Durable app management must be explicitly requested and reviewed rather than inferred from execution consent.
	if input.Management {
		scope = store.OAuthDeploymentScope
	}
	result, err := h.service.RegisterClient(store.ContextWithRuntimeManifest(r.Context(), []byte(input.Manifest)), oauthprovider.RegisterClientRequest{Name: input.Name, RedirectURI: input.RedirectURI, Scopes: []string{scope}})
	// Failed registration cannot expose a partial client secret.
	if err != nil {
		return nil, err
	}
	return map[string]any{"client_id": result.ClientID, "client_secret": result.ClientSecret, "client_id_expires_at": result.ClientIDExpiresAt, "manifest_digest": store.RuntimeManifestDigest([]byte(input.Manifest))}, nil
}

// authorizeRequest verifies client/callback/PKCE before reading the registered requirements.
func (h *runtimeOAuthHandler) authorizeRequest(r *http.Request, actor accesscontrol.Actor, req oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error) {
	return h.service.AuthorizeRuntime(r.Context(), actor, req)
}

// manifest loads only the registered compiler output; browser submissions never replace it.
func (h *runtimeOAuthHandler) manifest(ctx context.Context, client string) (runtimeAccessManifest, string, error) {
	repository, ok := h.store.(store.OAuthRuntimeManifestStore)
	// Missing storage cannot produce a permissive empty request.
	if !ok {
		return runtimeAccessManifest{}, "", errors.New("runtime storage unavailable")
	}
	raw, err := repository.OAuthRuntimeManifest(ctx, client)
	// Revoked or expired clients cannot inspect or authorize stale requests.
	if err != nil {
		return runtimeAccessManifest{}, "", err
	}
	manifest, err := parseRuntimeManifest(raw)
	return manifest, store.RuntimeManifestDigest(raw), err
}

// renderAuthorization shares the existing consent shell and exposes only buckets this actor may use.
func (h *runtimeOAuthHandler) renderAuthorization(w http.ResponseWriter, r *http.Request, actor accesscontrol.Actor, result oauthprovider.AuthorizeResult, req oauthprovider.AuthorizeRequest) {
	manifest, digest, err := h.manifest(r.Context(), req.ClientID)
	// An unreadable manifest must not render an incomplete permission prompt.
	if err != nil {
		renderOAuthErrorPage(r, h.store, w, 400, "The compiled access request is unavailable.")
		return
	}
	scope, err := (accesscontrol.SnapshotAuthorizer{}).Scope(r.Context(), actor, accesscontrol.PermissionBucketUse, accesscontrol.ResourceBucket)
	// Bucket names are disclosed only within the browser actor's authorized scope.
	if err != nil {
		renderOAuthErrorPage(r, h.store, w, 403, "You need permission to use a credential bucket.")
		return
	}
	buckets, total, err := h.store.ListAuthorizedBucketSummaries(r.Context(), scope, 1000, 0)
	// A bounded picker cannot silently hide some available choices.
	if err != nil || total > 1000 {
		renderOAuthErrorPage(r, h.store, w, 400, "The available buckets could not be loaded.")
		return
	}
	fixed, err := requestedRuntimeBucket(manifest.Bucket, buckets)
	// A caller-selected unavailable bucket must never fall back to a picker or another credential set.
	if err != nil {
		renderOAuthErrorPage(r, h.store, w, 403, "The requested credential bucket is unavailable or you do not have permission to use it.")
		return
	}
	page := runtimeConsentPage{FixedBucket: fixed, Management: len(req.Scope) == 1 && req.Scope[0] == store.OAuthDeploymentScope, Name: manifest.Name, Version: manifest.Version, Digest: digest, Services: manifest.Services, Buckets: buckets}
	renderOAuthConsentPage(w, r, h.store, result, req, nil, &page)
}

// selectedBuckets resolves explicit IDs through current authorization before generating an API app plan.
func (h *runtimeOAuthHandler) selectedBuckets(r *http.Request, actor accesscontrol.Actor, manifest runtimeAccessManifest) ([]store.Bucket, error) {
	ids, err := runtimeBucketIDs(r.PostForm.Get("bucket"), r.PostForm["runtime_bucket"])
	// Parse and deduplicate selections before performing any authorized metadata read.
	if err != nil {
		return nil, err
	}
	buckets := make([]store.Bucket, 0, len(ids))
	// Each approved route requires current bucket-use authority independently.
	for _, id := range ids {
		err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(r.Context(), actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: id}})
		// Permission is re-evaluated on submission rather than trusted from the rendered form.
		if err != nil {
			return nil, err
		}
		bucket, err := h.store.GetBucket(r.Context(), id)
		// Deleted identities cannot be rebound to a replacement with the same name.
		if err != nil || bucket == nil {
			return nil, oauthprovider.ErrInvalidRequest
		}
		buckets = append(buckets, *bucket)
	}

	// A fixed request grants only its selected bucket, regardless of forged or added form fields.
	if manifest.Bucket != "" && (len(buckets) != 1 || !runtimeBucketMatches(manifest.Bucket, buckets[0].ID, buckets[0].Name)) {
		return nil, oauthprovider.ErrInvalidRequest
	}
	return buckets, nil
}

// runtimeBucketMatches treats canonical UUID references as identities and other references as exact names.
func runtimeBucketMatches(reference string, id uuid.UUID, name string) bool {
	// UUID-shaped references never alias a different bucket whose name happens to look like an ID.
	if parsed, err := uuid.Parse(reference); err == nil {
		return parsed == id && parsed.String() == reference
	}
	return reference == name
}

// requestedRuntimeBucket resolves only metadata already filtered by current bucket-use authorization.
func requestedRuntimeBucket(reference string, available []store.BucketSummary) (*store.BucketSummary, error) {
	// Omission deliberately retains the generic consent picker.
	if reference == "" {
		return nil, nil
	}
	for _, bucket := range available {
		// A selected name or UUID must identify one currently authorized bucket.
		if runtimeBucketMatches(reference, bucket.ID, bucket.Name) {
			return &bucket, nil
		}
	}
	return nil, oauthprovider.ErrInvalidRequest
}

// runtimeBucketIDs validates the finite browser selection without silently choosing a default.
func runtimeBucketIDs(primary string, additional []string) ([]uuid.UUID, error) {
	raw := append([]string{primary}, additional...)
	// A duplicate default selection is permitted, while the submitted form stays bounded.
	if len(raw) > 33 {
		return nil, oauthprovider.ErrInvalidRequest
	}
	seen := map[uuid.UUID]bool{}
	result := make([]uuid.UUID, 0, len(raw))
	// Preserve the default first while collapsing repeated approved identities.
	for _, value := range raw {
		id, err := uuid.Parse(value)
		// Omitted, invalid, or noncanonical identities never fall back to a bucket name.
		if err != nil || id == uuid.Nil || id.String() != value {
			return nil, oauthprovider.ErrInvalidRequest
		}
		// A checked default is equivalent to selecting it only in the default dropdown.
		if !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	// The app planner admits no more than 32 approved bucket identities.
	if len(result) > 32 {
		return nil, oauthprovider.ErrInvalidRequest
	}
	return result, nil
}

// consentRedirect validates proof again, publishes the reviewed ordinary API app, then issues a bound code.
func (h *runtimeOAuthHandler) consentRedirect(r *http.Request, actor accesscontrol.Actor, req oauthprovider.ConsentRequest) (string, error) {
	_, err := h.service.AuthorizeRuntime(r.Context(), actor, oauthprovider.AuthorizeRequest{ClientID: req.ClientID, RedirectURI: req.RedirectURI, ResponseType: "code", Scope: req.Scope, State: req.State, CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod})
	// Even denials must validate the registered callback before redirecting.
	if err != nil {
		return "", err
	}
	// Cancellation performs no app publication and issues no runtime credential.
	if r.PostForm.Get("decision") == "deny" {
		return oauthDeniedRedirect(req)
	}
	// Unknown decisions cannot be mistaken for owner approval.
	if r.PostForm.Get("decision") != "allow" {
		return "", oauthprovider.ErrInvalidRequest
	}
	manifest, digest, err := h.manifest(r.Context(), req.ClientID)
	// A digest from the rendered page fences accidental use of another compiled request.
	if err != nil || r.PostForm.Get("manifest_digest") != digest {
		return "", oauthprovider.ErrInvalidRequest
	}
	buckets, err := h.selectedBuckets(r, actor, manifest)
	// No plan may be created with unavailable or unauthorized credential routing.
	if err != nil {
		return "", err
	}
	grant, err := h.publish(r.Context(), actor, manifest, digest, buckets)
	// Publication failure cannot produce a callback claiming a usable runtime.
	if err != nil {
		return "", err
	}
	return h.service.ConsentRuntime(r.Context(), actor, req, grant)
}

// publish reuses immutable API app planning/apply, dependency checks, ownership and quota enforcement.
func (h *runtimeOAuthHandler) publish(ctx context.Context, actor accesscontrol.Actor, manifest runtimeAccessManifest, digest string, buckets []store.Bucket) (store.OAuthRuntimeGrant, error) {
	return h.publishWithCeiling(ctx, actor, manifest, digest, buckets, nil)
}

// publishWithCeiling applies the original immutable provider ceiling before committing any later release.
func (h *runtimeOAuthHandler) publishWithCeiling(ctx context.Context, actor accesscontrol.Actor, manifest runtimeAccessManifest, digest string, buckets []store.Bucket, ceiling *store.OAuthRuntimeGrant) (store.OAuthRuntimeGrant, error) {
	names := make([]string, 0, len(buckets))
	// The planner resolves the complete approved set through its existing bucket-name bindings.
	for _, bucket := range buckets {
		names = append(names, bucket.Name)
	}
	sort.Strings(names)
	doc := manifest.document(buckets[0].Name, names)
	// Existing families require delegation authority before any app or plan is committed.
	if err := h.preflightTokenDelegation(ctx, actor, doc.Name); err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	raw, err := json.Marshal(doc)
	// Serialization failure cannot become an empty desired state.
	if err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	request := SDKConfigPlanRequest{ConfigKey: "sdk:" + doc.Name + ":" + doc.Version, SourceHash: store.RuntimeManifestDigest(raw), Config: raw}
	ctx = accesscontrol.ContextWithActor(ctx, actor)
	call := sdkPlanCall{accountID: actor.AccountID, actor: actor, request: request, document: doc, defaultEngineURL: h.publicURL}
	// Updating an approved family requires manage on that family, never workspace app creation.
	if ceiling != nil {
		call.authorizationAppID = ceiling.AppID
	}
	plan, err := createSDKConfigPlan(ctx, h.configs, h.store, h.registry, call)
	// Existing app lifecycle and provider snapshot failures must retain their normal behavior.
	if err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	// The plan must preserve the exact bucket identities selected in the browser.
	if err := validateRuntimePlannedBuckets(plan.plan.ResolvedPayload, buckets); err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	payload, err := appPayloadFromJSON(plan.plan.ResolvedPayload)
	// Retain original provider hash bindings and reject label reuse before immutable app publication.
	if err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	if err := validateDeploymentPlan(ctx, h.store, actor.AccountID, payload, ceiling); err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	result, err := executeSDKConfigApply(ctx, h.configs, h.store, nil, h.registry, sdkApplyCall{kind: store.ConfigTypeSDK, accountID: actor.AccountID, actor: actor, planID: plan.plan.ID, planRevision: plan.plan.Revision, sourceHash: request.SourceHash, skipToken: true})
	// Failed apply cannot leave a separately minted runtime secret behind.
	if err != nil {
		return store.OAuthRuntimeGrant{}, err
	}
	choices := make([]store.OAuthRuntimeBucket, 0, len(buckets))
	// Return only public routing metadata for the approved identities.
	for _, bucket := range buckets {
		choices = append(choices, store.OAuthRuntimeBucket{ID: bucket.ID, Name: bucket.Name})
	}
	return store.OAuthRuntimeGrant{ContractBindings: allAppContractBindings(payload), AppID: result.AppID, AppFamilyID: result.AppFamilyID, ManifestDigest: digest, DefaultBucketID: buckets[0].ID, Buckets: choices}, nil
}

// preflightTokenDelegation follows initial-token create authority and requires explicit delegation for existing families.
func (h *runtimeOAuthHandler) preflightTokenDelegation(ctx context.Context, actor accesscontrol.Actor, name string) error {
	canonicalName, _, err := canonical.AppName(name)
	// Invalid names must never reach a family lookup or fall back to creation authority.
	if err != nil {
		return oauthprovider.ErrInvalidRequest
	}
	family, err := h.store.GetAppFamilyByIdentity(ctx, actor.AccountID, store.AppKindSDK.String(), canonicalName)
	// A fresh personal family obtains its first credential under create authority, as ordinary apply does.
	// Apply's immutable owner check also fences a concurrent claim by another subject.
	if errors.Is(err, store.ErrAppFamilyNotFound) {
		return (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppAPICreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
	}
	// Lookup failures cannot be interpreted as permission to create an unowned credential.
	if err != nil {
		return err
	}
	return (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppAPITokensManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: family.AppFamilyID}})
}

// validateRuntimePlannedBuckets rejects concurrent name reuse and any omitted or added runtime scope.
func validateRuntimePlannedBuckets(raw json.RawMessage, buckets []store.Bucket) error {
	payload, err := appPayloadFromJSON(raw)
	// The ordinary planner must resolve the default and complete allowlist without substitutions.
	if err != nil || payload.BucketID != buckets[0].ID || len(payload.RuntimeBuckets) != len(buckets) {
		return oauthprovider.ErrInvalidRequest
	}
	approved := make(map[uuid.UUID]bool, len(buckets))
	// Compare resolved identities, since names may be reused after deletion.
	for _, bucket := range buckets {
		approved[bucket.ID] = true
	}
	// Every planned runtime route must belong to the browser-approved set.
	for _, ref := range payload.RuntimeBuckets {
		// An equal name never authorizes a newly created bucket identity.
		if !approved[ref.BucketID] {
			return oauthprovider.ErrInvalidRequest
		}
	}
	return nil
}

// token returns the runtime projection after shared client authentication and single-use PKCE exchange.
func (h *runtimeOAuthHandler) token(w http.ResponseWriter, r *http.Request) {
	setOAuthResponseHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
	// Malformed forms never reach the credential issuer.
	if r.ParseForm() != nil {
		writeOAuthTokenError(w, 400, "invalid_request")
		return
	}
	id, secret := oauthClientCredentials(r)
	result, err := h.service.TokenRuntime(r.Context(), oauthprovider.TokenRequest{GrantType: r.PostForm.Get("grant_type"), Code: r.PostForm.Get("code"), RedirectURI: r.PostForm.Get("redirect_uri"), CodeVerifier: r.PostForm.Get("code_verifier"), ClientID: id, ClientSecret: secret, RefreshToken: r.PostForm.Get("refresh_token")})
	// Failures are secret-free and never return the internally discarded OAuth credential pair.
	if err != nil {
		writeOAuthTokenError(w, oauthTokenErrorStatus(err), oauthTokenErrorCode(err))
		return
	}
	writeOAuthJSON(w, 200, result)
}

// revoke authenticates the registered client and retires only its own dedicated runtime token.
func (h *runtimeOAuthHandler) revoke(w http.ResponseWriter, r *http.Request) {
	setOAuthResponseHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
	// A malformed form cannot become an empty-token success response.
	if r.ParseForm() != nil {
		writeOAuthTokenError(w, 400, "invalid_request")
		return
	}
	id, secret := oauthClientCredentials(r)
	err := h.service.RevokeRuntime(r.Context(), oauthprovider.RevokeRequest{ClientID: id, ClientSecret: secret, Token: r.PostForm.Get("token")})
	// Client authentication failures retain standard OAuth error semantics.
	if err != nil {
		writeOAuthTokenError(w, oauthTokenErrorStatus(err), oauthTokenErrorCode(err))
		return
	}
	writeOAuthJSON(w, 200, map[string]any{})
}
