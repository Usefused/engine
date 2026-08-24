package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/requestbinding"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/secretref"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrCredentialExpired means a stored secret's expires_at has passed. The
// dispatch is aborted before ever reaching the vendor -- a confusing 401 from
// the vendor (or worse, a signature-verification failure with no obvious
// cause) is a much harder failure to diagnose than failing fast here with the
// key name and expiry time attached.
var ErrCredentialExpired = errors.New("credential expired")

const connectedAuthRefreshWindow = 5 * time.Minute

type SecretResolver interface {
	ResolveExecutionCredentials(ctx context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error)
	// GetWebhookSecret resolves one signing secret through the immutable
	// bucket ID captured at apply. secretRef supplies only the validated key;
	// its bucket name is never trusted for runtime lookup.
	GetWebhookSecret(ctx context.Context, accountID, bucketID uuid.UUID, secretRef string) (string, error)
}

// connectedAuthFailureRecorder is intentionally narrower than SecretResolver;
// dispatch only needs a safe diagnostic write after a provider response.
type connectedAuthFailureRecorder interface {
	recordConnectedAuthFailure(ctx context.Context, credentials map[string]any, code string) (bool, error)
}

type CredentialRequest struct {
	AccountID        uuid.UUID
	AppID            uuid.UUID
	TokenID          uuid.UUID
	BindingMode      store.AppTokenBindingMode
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	OperationID      string
	AuthType         string
	Auths            fusedobject.AuthConfigs
	Requirements     authrouting.Requirements
	Passthrough      map[string]any
}

type secretResolver struct {
	db                 store.Store
	masterKey          []byte
	refreshCoordinator *AuthRefreshCoordinator
}

// NewSecretResolver creates the request credential resolver and its shared
// lease-aware connected-auth refresh coordinator.
func NewSecretResolver(db store.Store, masterKey []byte) SecretResolver {
	return &secretResolver{
		db:                 db,
		masterKey:          masterKey,
		refreshCoordinator: NewAuthRefreshCoordinator(db, masterKey),
	}
}

// ResolveExecutionCredentials uses the full dispatch identity so secret and
// binding queries remain pinned to one service version, operation, and auth
// scheme rather than loading a service-wide credential set.
func (r *secretResolver) ResolveExecutionCredentials(ctx context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "ResolveCredentials")
	defer span.End()

	scope, selections, err := r.loadExecutionScope(ctx, request.AppID)
	if err != nil {
		return nil, nil, err
	}
	bindings, err := r.loadBucketBindings(ctx, scope.BucketID, request)
	if err != nil {
		return nil, nil, err
	}

	bindings = appendInjectionBindings(bindings, selections, request.ServiceID)

	finalCreds := copyPassthroughCredentials(request.Passthrough)
	// Flow selection is configuration, not a per-call credential. Removing the
	// caller value prevents an SDK request from bypassing the reviewed profile.
	delete(finalCreds, "fused_oauth2_flow")
	if err := r.applyWorkspaceOAuthProfile(ctx, request, finalCreds); err != nil {
		return nil, nil, err
	}
	if err := r.applyAppTokenBinding(ctx, request, finalCreds); err != nil {
		return nil, nil, err
	}
	// The dispatcher needs the exact resolved bucket only to derive the fallback
	// connection scope; this internal routing value is stripped before telemetry.
	finalCreds["fused_bucket_id"] = scope.BucketID.String()
	if requestbinding.HasDynamicSource(bindings) {
		finalCreds["fused_resource_required"] = "true"
	}
	if err := r.mergeStoredSecrets(ctx, scope.BucketID, request.ServiceID, finalCreds, request.Auths, request.Requirements); err != nil {
		return nil, nil, err
	}
	if err := r.resolveConnectedAuth(ctx, scope.BucketID, request.ServiceID, request.ServiceVersionID, request.Auths, request.Requirements, finalCreds); err != nil {
		return nil, nil, err
	}
	values, err := resolveRequestBindings(bindings, finalCreds, scope.BucketID)
	if err != nil {
		return nil, nil, err
	}

	if err := r.resolveDynamicBucketValues(ctx, scope.BucketID, request.ServiceID, values); err != nil {
		return nil, nil, err
	}

	return finalCreds, values, nil
}

// loadExecutionScope keeps the persisted contract check ahead of bucket and
// secret reads while emitting only a bounded outcome on the execution span.
func (r *secretResolver) loadExecutionScope(ctx context.Context, appID uuid.UUID) (*store.AppRuntime, []models.SDKSelection, error) {
	scope, err := r.db.GetAppRuntime(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load SDK scope for secrets: %w", err)
	}
	selections, err := models.DecodeAppSelections(scope.ScopeSchemaVersion, scope.Selections)
	if err != nil {
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attribute.String("app.selection_schema.outcome", "rejected"))
		span.SetStatus(codes.Error, "unsupported app selection schema")
		return nil, nil, fmt.Errorf("failed to load SDK scope for secrets: unsupported app selection schema")
	}
	return scope, selections, nil
}

func (r *secretResolver) applyAppTokenBinding(ctx context.Context, request CredentialRequest, credentials map[string]any) error {
	if request.BindingMode != store.AppTokenBindingFixed {
		return nil
	}
	authName, err := selectedConnectedAuthName(credentials, request.Auths, request.Requirements)
	if err != nil {
		return err
	}
	if authName == "" {
		return nil
	}
	binding, err := r.db.GetAppTokenBinding(ctx, request.TokenID, request.ServiceID, authName)
	if err != nil {
		return fmt.Errorf("resolve fixed app token binding: %w", err)
	}
	if binding == nil {
		return store.ErrAppTokenBindingInvalid
	}
	// Fixed tokens ignore caller selectors by construction; only opaque IDs
	// resolved and validated at issuance may reach connected-auth lookup.
	delete(credentials, "fused_end_user_ref")
	delete(credentials, "end_user_ref")
	delete(credentials, "fused_resource_id")
	credentials["fused_auth_name"] = binding.AuthName
	credentials["fused_connection_id"] = binding.AuthConnectionID.String()
	if binding.ResourceID != nil {
		credentials["fused_resource_id"] = binding.ResourceID.String()
	}
	return nil
}

func (r *secretResolver) applyWorkspaceOAuthProfile(ctx context.Context, request CredentialRequest, credentials map[string]any) error {
	authType, required := workspaceOAuthProfileLookup(request)
	if !required {
		return nil
	}
	profileStore, ok := r.db.(store.WorkspaceProfileStore)
	if !ok {
		return errors.New("workspace OAuth flow selection is unavailable")
	}
	stored, err := profileStore.GetEffectiveWorkspaceProfile(ctx, request.ServiceID, request.ServiceVersionID, authType)
	if err != nil {
		return fmt.Errorf("failed to load workspace OAuth flow selection: %w", err)
	}
	if stored == nil {
		return nil
	}
	var profile connectionprofile.Profile
	if err := json.Unmarshal(stored.ProfileSnapshot, &profile); err != nil {
		return errors.New("workspace OAuth profile snapshot is invalid")
	}
	if profile.AuthName != "" {
		credentials[credentialKeyFusedAuthName] = profile.AuthName
	}
	if profile.OAuth2Flow != "" {
		credentials["fused_oauth2_flow"] = profile.OAuth2Flow
	}
	return nil
}

func workspaceOAuthProfileLookup(request CredentialRequest) (string, bool) {
	selector := canonicalAuthSelector(request.AuthType)
	for _, auth := range request.Auths {
		canonical := canonicalFusedAuthType(auth)
		if (selector == "" || selector == canonical) && canonical == "oauth" && len(auth.OAuth2Flows) > 1 {
			return "oauth", true
		}
	}
	return "", false
}

func appendInjectionBindings(bindings []store.WorkspaceConnectionBinding, selections []models.SDKSelection, serviceID uuid.UUID) []store.WorkspaceConnectionBinding {
	for _, sel := range selections {
		if sel.ServiceID == serviceID {
			for _, inj := range sel.Injections {
				injVal := inj.Value
				bindings = append(bindings, store.WorkspaceConnectionBinding{
					TargetLocation: inj.Location,
					TargetName:     inj.Name,
					SourceKind:     "literal",
					LiteralValue:   &injVal,
					Mode:           inj.Mode,
				})
			}
		}
	}
	return bindings
}

func (r *secretResolver) resolveDynamicBucketValues(ctx context.Context, bucketID, serviceID uuid.UUID, values []store.BucketValue) error {
	bucketKeys, secretKeys, err := extractDynamicKeys(values)
	if err != nil {
		return err
	}
	if len(bucketKeys) == 0 && len(secretKeys) == 0 {
		return nil
	}

	_, span := otel.Tracer("engine").Start(ctx, "engine.execution.dynamic_bucket_values_resolved")
	defer span.End()

	if bucketID == uuid.Nil {
		return errors.New("cannot resolve bucket values without a valid bucket assigned")
	}

	secretMap, err := r.fetchDynamicSecrets(ctx, bucketID, serviceID, bucketKeys, secretKeys)
	if err != nil {
		return err
	}

	return interpolateValues(values, secretMap)
}

// extractDynamicKeys deduplicates and sorts required variable keys from
// binding values. Returns an error for any bucket.* tag that isn't one of
// the recognized ambient forms -- most importantly a named-bucket reference
// like ${bucket.prod.secret.key} (the kind: webhook grammar, see
// internal/shared/secretref), which has no meaning here since an injection
// value always resolves against this app's own dispatch-scoped bucket.
// Surfacing that here, at classification time, gives a specific reason
// instead of letting it fall through unrecognized and surface later as
// interpolateValues' generic "missing required bucket value" error.
func extractDynamicKeys(values []store.BucketValue) ([]string, []string, error) {
	var bucketKeys []string
	var secretKeys []string
	extractedSet := make(map[string]bool)

	for _, val := range values {
		for _, k := range requestbinding.ExtractVariables(val.Value) {
			if extractedSet[k] {
				continue
			}
			extractedSet[k] = true
			key, err := classifyDynamicKey(k)
			if err != nil {
				return nil, nil, err
			}
			switch key.store {
			case dynamicKeyBucket:
				bucketKeys = append(bucketKeys, key.name)
			case dynamicKeySecret:
				secretKeys = append(secretKeys, key.name)
			}
		}
	}
	return bucketKeys, secretKeys, nil
}

// dynamicKeyStore is which fetch path a recognized ambient tag resolves
// through -- kept distinct from secretref.Kind since that's the named-bucket
// (webhook) grammar's own classification, not this ambient one's.
type dynamicKeyStore int

const (
	dynamicKeyIgnore dynamicKeyStore = iota // not a bucket.* tag at all -- not this function's concern
	dynamicKeyBucket
	dynamicKeySecret
)

type dynamicKey struct {
	store dynamicKeyStore
	name  string
}

// classifyDynamicKey accepts only the three ambient forms SDK/MCP injections
// support (no bucket name segment -- always this app's own dispatch
// bucket) and rejects any other bucket.* shape by name instead of silently
// dropping it.
func classifyDynamicKey(k string) (dynamicKey, error) {
	switch {
	case strings.HasPrefix(k, "bucket.env."):
		return dynamicKey{store: dynamicKeyBucket, name: strings.TrimPrefix(k, "bucket.env.")}, nil
	case strings.HasPrefix(k, "bucket.values."):
		return dynamicKey{store: dynamicKeyBucket, name: strings.TrimPrefix(k, "bucket.values.")}, nil
	case strings.HasPrefix(k, "bucket.secrets."):
		return dynamicKey{store: dynamicKeySecret, name: strings.TrimPrefix(k, "bucket.secrets.")}, nil
	case strings.HasPrefix(k, "bucket."):
		return dynamicKey{}, fmt.Errorf(
			"injection value references %q, which is not a supported bucket variable here: "+
				"SDK/MCP injections resolve only against this app's own bucket via "+
				"${bucket.env.*}, ${bucket.values.*}, or ${bucket.secrets.*} -- a named bucket "+
				"like ${bucket.<name>.secret.<key>} is valid only in a kind: webhook secret", k)
	default:
		return dynamicKey{}, nil
	}
}

// IsExactNonSecretBucketReference lets app validation reuse the same key
// classifier that owns runtime's targeted bucket ingestion.
func IsExactNonSecretBucketReference(input string) bool {
	keys := requestbinding.ExtractVariables(input)
	// URL routing accepts one complete token so literals cannot surround it.
	if len(keys) != 1 || input != "${"+keys[0]+"}" {
		return false
	}
	key, err := classifyDynamicKey(keys[0])
	return err == nil && key.store == dynamicKeyBucket && key.name != ""
}

// fetchDynamicSecrets retrieves requested environment variables and secrets via batch DB calls.
func (r *secretResolver) fetchDynamicSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, bucketKeys, secretKeys []string) (map[string]string, error) {
	secretMap := make(map[string]string)

	if len(bucketKeys) > 0 {
		bucketValues, err := r.db.GetBucketValues(ctx, bucketID, serviceID, bucketKeys)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch dynamic bucket values: %w", err)
		}
		for _, bv := range bucketValues {
			// Re-add both prefixes to the map so the parser can resolve both formats
			secretMap["bucket.env."+bv.KeyName] = bv.Value
			secretMap["bucket.values."+bv.KeyName] = bv.Value
		}
	}

	if len(secretKeys) > 0 {
		secrets, err := r.db.GetSecrets(ctx, bucketID, serviceID, secretKeys)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch dynamic bucket secrets: %w", err)
		}
		for _, sec := range secrets {
			val, err := r.decryptStoredSecret(serviceID, sec)
			if err != nil {
				return nil, err
			}
			secretMap["bucket.secrets."+sec.KeyName] = val
		}
	}

	return secretMap, nil
}

// interpolateValues replaces variable tokens with their mapped secrets, failing on missing dependencies.
func interpolateValues(values []store.BucketValue, secretMap map[string]string) error {
	for i, val := range values {
		if len(requestbinding.ExtractVariables(val.Value)) > 0 {
			interpolated := requestbinding.Interpolate(val.Value, secretMap)
			// verify no unresolved bucket tags exist
			unresolved := requestbinding.ExtractVariables(interpolated)
			for _, unk := range unresolved {
				if strings.HasPrefix(unk, "bucket.") {
					return fmt.Errorf("missing required bucket value for injection: %s", unk)
				}
			}
			values[i].Value = interpolated
		}
	}
	return nil
}

// copyPassthroughCredentials protects request-scoped auth maps from resolver
// mutations, which matters for retries and audit-safe tests.
func copyPassthroughCredentials(passthrough map[string]any) map[string]any {
	// Runtime credentials are request-scoped; copy before merging stored
	// bucket material so retries/tests cannot observe resolver side effects.
	finalCreds := make(map[string]any, len(passthrough))
	for k, v := range passthrough {
		finalCreds[k] = v
	}
	return finalCreds
}

// loadBucketBindings requires the rollout capability explicitly so runtime can
// never regress to loading a whole bucket and filtering it in process.
// bucketID resolves the dispatching workspace via a join inside the store
// (see WorkspaceProfileStore.ListWorkspaceBindingsForExecution) -- the
// binding rows themselves are workspace-scoped, not bucket-scoped, so every
// bucket in the workspace resolves the same effective bindings for a given
// service version and auth type.
func (r *secretResolver) loadBucketBindings(ctx context.Context, bucketID uuid.UUID, request CredentialRequest) ([]store.WorkspaceConnectionBinding, error) {
	bindingStore, ok := r.db.(interface {
		ListWorkspaceBindingsForExecution(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) ([]store.WorkspaceConnectionBinding, error)
	})
	if !ok {
		return nil, errors.New("targeted workspace binding store is unavailable")
	}
	bindings, err := bindingStore.ListWorkspaceBindingsForExecution(ctx, bucketID, request.ServiceID, request.ServiceVersionID, request.AuthType, request.OperationID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workspace bindings from store: %w", err)
	}
	return bindings, nil
}

func resolveRequestBindings(bindings []store.WorkspaceConnectionBinding, credentials map[string]any, bucketID uuid.UUID) ([]store.BucketValue, error) {
	resource := requestbinding.Resource{
		ProviderResourceID: credentialString(credentials, "fused_resource_provider_id"),
		BaseURL:            credentialString(credentials, "fused_resource_base_url"),
	}
	if raw, ok := credentials["fused_resource_metadata"].([]byte); ok {
		resource.MetadataJSON = raw
	}
	return requestbinding.Resolve(bindings, resource, bucketID)
}

// mergeStoredSecrets preserves per-call overrides while reading only the auth
// keys this execution can apply.
func (r *secretResolver) mergeStoredSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, credentials map[string]any, auths fusedobject.AuthConfigs, requirements authrouting.Requirements) error {
	alternatives, err := orderedStaticSecretAlternatives(auths, requirements, credentials)
	if err != nil {
		return err
	}
	if len(alternatives) == 0 || secretAlternativeNeedsNoStore(alternatives[0]) {
		return nil
	}
	secrets, err := r.db.GetFirstCompleteSecretSet(ctx, bucketID, serviceID, alternatives)
	if err != nil {
		return fmt.Errorf("failed to fetch secrets from store: %w", err)
	}
	for _, sec := range secrets {
		val, err := r.decryptStoredSecret(serviceID, sec)
		if err != nil {
			return err
		}
		// Per-call passthrough is intentionally allowed to shadow the bucket:
		// generated SDKs can support one-off overrides without mutating the
		// team's stored credential state.
		credentials[sec.KeyName] = val
	}
	return nil
}

func secretAlternativeNeedsNoStore(alternative store.SecretKeyAlternative) bool {
	return len(alternative.Required) == 0 && len(alternative.Optional) == 0
}

// decryptStoredSecret owns expiry enforcement for stored bucket secrets so
// callers cannot accidentally dispatch stale static credentials.
func (r *secretResolver) decryptStoredSecret(serviceID uuid.UUID, sec store.WorkspaceSecret) (string, error) {
	if sec.ExpiresAt != nil && sec.ExpiresAt.Before(time.Now().UTC()) {
		return "", fmt.Errorf("%w: %q for service %s expired at %s", ErrCredentialExpired, sec.KeyName, serviceID, sec.ExpiresAt.Format(time.RFC3339))
	}
	plain, err := store.UnwrapDEK(r.masterKey, sec.EncryptedDEK)
	if err != nil {
		return "", fmt.Errorf("failed to unwrap DEK for secret %s: %w", sec.ID, err)
	}
	val, err := store.DecryptWithDEK(plain, sec.EncryptedValue)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt secret %s: %w", sec.ID, err)
	}
	return val, nil
}

// resolveConnectedAuth turns a stable end-user reference into the actual
// provider credential only inside Engine's execution path.
func (r *secretResolver) resolveConnectedAuth(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, auths fusedobject.AuthConfigs, requirements authrouting.Requirements, credentials map[string]any) error {
	endUserRef := connectedEndUserRef(credentials)
	if !connectedAuthResolutionRequired(endUserRef, credentials, requirements) {
		return nil
	}
	authName, err := selectedConnectedAuthName(credentials, auths, requirements)
	if err != nil {
		return err
	}
	if authName == "" {
		// The user reference alone identifies who connected; the auth name tells
		// the dispatcher which provider slot receives the decrypted access token.
		return errors.New("connected auth requires fused_auth_name or fused_auth_type")
	}
	conn, err := r.selectedUsableAuthConnection(ctx, bucketID, serviceID, serviceVersionID, endUserRef, authName, credentials)
	if err != nil {
		return err
	}
	// The opaque connection ID stays inside Engine and lets post-dispatch
	// diagnostics update exactly one row without repeating the user lookup.
	credentials["fused_connection_id"] = conn.ID.String()
	token, err := decryptAuthConnectionAccessToken(r.masterKey, conn)
	if err != nil {
		return fmt.Errorf("failed to decrypt auth connection: %w", err)
	}
	injectConnectedToken(credentials, authName, token)
	if err := r.maybeInjectConnectedResource(ctx, conn, credentials); err != nil {
		return err
	}
	if err := r.db.TouchAuthConnectionLastUsed(ctx, conn.ID, time.Now().UTC()); err != nil {
		return fmt.Errorf("failed to touch auth connection: %w", err)
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.auth_connection.used")
	span.SetAttributes(authConnectionSpanAttrs(conn)...)
	span.End()
	return nil
}

func connectedAuthResolutionRequired(endUserRef string, credentials map[string]any, requirements authrouting.Requirements) bool {
	if endUserRef == "" && credentialString(credentials, "fused_connection_id") == "" {
		return false
	}
	selectorType := requestedAuthType(credentials)
	selectorName := requestedAuthName(credentials)
	if selectorType == "" && selectorName == "" {
		// An end-user reference is identity context, not an instruction to turn
		// an explicitly anonymous operation into an authenticated provider call.
		return !requirementsPermitAnonymous(requirements)
	}
	// Static auth is satisfied from bucket secrets; carrying end-user context
	// alongside it must not trigger a connected-account lookup.
	return selectorType == "" || isConnectedAuthSelector(selectorType)
}

// maybeInjectConnectedResource avoids a resource-table query for ordinary
// OAuth services while keeping the main auth resolver below the audit ceiling.
func (r *secretResolver) maybeInjectConnectedResource(ctx context.Context, conn *store.AuthConnection, credentials map[string]any) error {
	if !connectedResourceRequested(credentials) {
		return nil
	}
	return r.injectConnectedResource(ctx, conn, credentials)
}

// connectedResourceRequested keeps legacy/non-resource OAuth requests DB-free
// while honoring either a service metadata marker or an explicit SDK choice.
func connectedResourceRequested(credentials map[string]any) bool {
	return credentialString(credentials, "fused_resource_required") == "true" || credentialString(credentials, "fused_resource_id") != ""
}

// injectConnectedResource resolves routing context with one connection-scoped
// query and keeps the trusted URL internal to the Engine credential envelope.
func (r *secretResolver) injectConnectedResource(ctx context.Context, conn *store.AuthConnection, credentials map[string]any) error {
	resourceID, err := optionalConnectionResourceID(credentials)
	if err != nil {
		return err
	}
	resource, activeCount, err := r.db.GetConnectionResourceForExecution(ctx, conn.ID, resourceID)
	if err != nil {
		return fmt.Errorf("resolve connection resource: %w", err)
	}
	if resource == nil {
		if err := connectionResourceSelectionError(conn, resourceID, activeCount); err != nil {
			return err
		}
		// Connections created for services without x-fused-connect legitimately
		// have no resource row and continue on the static service URL.
		return nil
	}
	credentials["fused_connection_id"] = conn.ID.String()
	credentials["fused_resource_id"] = resource.ID.String()
	credentials["fused_resource_base_url"] = resource.BaseURL
	credentials["fused_resource_type"] = resource.ResourceType
	credentials["fused_resource_provider_id"] = resource.ProviderResourceID
	credentials["fused_resource_metadata"] = append([]byte(nil), resource.MetadataJSON...)
	return nil
}

// optionalConnectionResourceID accepts only Engine-issued UUIDs; provider IDs
// are metadata and can never be used to reach another connection's resource.
func optionalConnectionResourceID(credentials map[string]any) (*uuid.UUID, error) {
	value := credentialString(credentials, "fused_resource_id")
	if value == "" {
		return nil, nil
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return nil, errors.New("fused resource id must be a valid UUID")
	}
	return &id, nil
}

// connectionResourceSelectionError distinguishes services without resource
// routing from an invalid explicit choice and a genuinely ambiguous tenant.
func connectionResourceSelectionError(conn *store.AuthConnection, requested *uuid.UUID, activeCount int) error {
	if requested != nil {
		return newResourceSelectionRequiredError(conn, "resource_not_found")
	}
	if activeCount > 1 {
		return newResourceSelectionRequiredError(conn, "multiple_resources")
	}
	return nil
}

// connectedEndUserRef accepts the generated fused_* key plus the older alias so
// CLI and SDK callers resolve the same bucket-owned connection row.
func connectedEndUserRef(credentials map[string]any) string {
	endUserRef := credentialString(credentials, "fused_end_user_ref")
	if endUserRef != "" {
		return endUserRef
	}
	return credentialString(credentials, "end_user_ref")
}

// injectConnectedToken keeps connected tokens engine-side until this final
// merge point; SDKs only send the stable end-user reference over the wire.
func injectConnectedToken(credentials map[string]any, authName, token string) {
	if _, exists := credentials[authName]; !exists {
		credentials[authName] = token
	}
}

// selectedConnectedAuthName keeps public selection at the auth-type level while
// returning the internal credential key applyAuth needs for token injection.
func selectedConnectedAuthName(credentials map[string]any, auths fusedobject.AuthConfigs, requirements authrouting.Requirements) (string, error) {
	if authName := credentialString(credentials, "fused_auth_name"); authName != "" {
		if requirementAuthNameConfigured(auths, requirements, authName, "") {
			return authName, nil
		}
		return "", fmt.Errorf("connected auth name %q is not configured for this operation", authName)
	}
	selector := requestedAuthType(credentials)
	if selector == "" {
		return defaultConnectedAuthName(auths, requirements), nil
	}
	if !isConnectedAuthSelector(selector) {
		// Static auth can share request builders with connected auth. Selecting a
		// static type must not trigger a user-connection lookup just because an
		// endUserRef is present.
		return "", nil
	}
	authName := connectedAuthNameForRequirements(auths, requirements, selector)
	if authName == "" {
		return "", fmt.Errorf("connected auth type %q is not configured for this service", selector)
	}
	return authName, nil
}

// usableAuthConnection refreshes near-expiry tokens before dispatch while
// still allowing a currently-valid token through if the provider is flaky.
func (r *secretResolver) usableAuthConnection(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, endUserRef, authName string) (*store.AuthConnection, error) {
	conn, err := r.db.GetAuthConnection(ctx, bucketID, serviceID, endUserRef, authName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch auth connection: %w", err)
	}
	if conn == nil {
		return nil, newConnectionRequiredError(bucketID.String(), serviceID.String(), endUserRef)
	}
	return r.ensureUsableAuthConnection(ctx, conn, serviceVersionID)
}

func (r *secretResolver) selectedUsableAuthConnection(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, endUserRef, authName string, credentials map[string]any) (*store.AuthConnection, error) {
	connectionID := credentialString(credentials, "fused_connection_id")
	if connectionID == "" {
		return r.usableAuthConnection(ctx, bucketID, serviceID, serviceVersionID, endUserRef, authName)
	}
	id, err := uuid.Parse(connectionID)
	if err != nil {
		return nil, errors.New("fixed app token connection identity is invalid")
	}
	conn, err := r.db.GetAuthConnectionByIDForBuckets(ctx, id, []uuid.UUID{bucketID})
	if err != nil {
		return nil, fmt.Errorf("load fixed app token connection: %w", err)
	}
	if conn == nil || conn.BucketID != bucketID || conn.ServiceID != serviceID || conn.AuthName != authName {
		return nil, store.ErrAppTokenBindingInvalid
	}
	return r.ensureUsableAuthConnection(ctx, conn, serviceVersionID)
}

func (r *secretResolver) ensureUsableAuthConnection(ctx context.Context, conn *store.AuthConnection, serviceVersionID uuid.UUID) (*store.AuthConnection, error) {
	// Once Engine has classified the grant as permanently unusable, later SDK
	// calls must receive the same action instead of dispatching a stale token.
	if conn.RefreshState == reconnectRequiredCode {
		return nil, newReconnectRequiredError(conn, "stored_grant_unusable")
	}
	coordinator := r.refreshCoordinator
	if coordinator == nil {
		// Focused legacy constructions may instantiate secretResolver directly;
		// building the same coordinator lazily keeps production and tests on one path.
		coordinator = NewAuthRefreshCoordinator(r.db, r.masterKey)
	}
	return coordinator.ensureForegroundConnectionFresh(ctx, conn, serviceVersionID)
}

// recordConnectedAuthFailure stores provider authorization diagnostics without
// changing refresh state or attempting token recovery for an unexpired grant.
func (r *secretResolver) recordConnectedAuthFailure(ctx context.Context, credentials map[string]any, code string) (bool, error) {
	connectionID := credentialString(credentials, "fused_connection_id")
	// Static credentials have no connected-user row and continue through their
	// existing provider error behavior without an unrelated database write.
	if connectionID == "" {
		return false, nil
	}
	id, err := uuid.Parse(connectionID)
	// Caller-supplied internal IDs are stripped before resolution, so an invalid
	// value here signals an Engine invariant failure rather than bad user input.
	if err != nil {
		return false, errors.New("resolved connection id is invalid")
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.auth_connection.failure_record")
	defer span.End()
	span.SetAttributes(attribute.String("connection_id", id.String()), attribute.String("failure_code", code))
	err = r.db.RecordAuthConnectionFailure(ctx, id, code, executionTraceID(ctx), time.Now().UTC())
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}
	span.SetStatus(codes.Ok, "auth connection failure recorded")
	return true, nil
}

// executionTraceID returns only a valid OTEL correlation identifier, avoiding
// placeholder values that would send users searching for a nonexistent trace.
func executionTraceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

// authConnectionSpanAttrs keeps refresh/usage spans free of token material
// while still linking audit events to the bucket-owned connection.
func authConnectionSpanAttrs(conn *store.AuthConnection) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("bucket_id", conn.BucketID.String()),
		attribute.String("service_id", conn.ServiceID.String()),
		attribute.String("connection_id", conn.ID.String()),
	}
}

// WithDefaultConnectedAuthName lets generated SDKs avoid service-specific auth
// field names while still targeting the correct dispatcher credential slot.
func WithDefaultConnectedAuthName(credentials map[string]any, auths fusedobject.AuthConfigs, requirements authrouting.Requirements) map[string]any {
	authName := defaultConnectedAuthNameForCredentials(credentials, auths, requirements)
	if authName == "" || credentialString(credentials, "fused_auth_name") != "" {
		return credentials
	}
	// Most generated SDK calls only know "which end user"; deriving the auth
	// name here keeps service-specific credential field names out of app code.
	out := make(map[string]any, len(credentials)+1)
	for key, value := range credentials {
		out[key] = value
	}
	out["fused_auth_name"] = authName
	return out
}

// defaultConnectedAuthName keeps the auth-name inference deliberately narrow
// to OAuth/OIDC schemes that Engine-owned connect can satisfy.
func defaultConnectedAuthName(auths fusedobject.AuthConfigs, requirements authrouting.Requirements) string {
	return connectedAuthNameForRequirements(auths, requirements, "")
}

// defaultConnectedAuthNameForCredentials lets authType select the connected
// credential slot while preserving the old first-OAuth/OIDC default.
func defaultConnectedAuthNameForCredentials(credentials map[string]any, auths fusedobject.AuthConfigs, requirements authrouting.Requirements) string {
	selector := requestedAuthType(credentials)
	if selector == "" {
		return defaultConnectedAuthName(auths, requirements)
	}
	return connectedAuthNameForRequirements(auths, requirements, selector)
}

func connectedAuthNameForRequirements(auths fusedobject.AuthConfigs, requirements authrouting.Requirements, selector string) string {
	definitions, err := fusedAuthDefinitions(auths)
	if err != nil {
		return ""
	}
	for _, alternative := range requirements {
		for _, requirement := range alternative.Schemes {
			auth, ok := definitions[requirement.Scheme]
			if ok && isConnectedAuthSelector(canonicalFusedAuthType(auth)) && (selector == "" || canonicalFusedAuthType(auth) == selector) {
				return authCredentialName(auth)
			}
		}
	}
	return ""
}

func requirementAuthNameConfigured(auths fusedobject.AuthConfigs, requirements authrouting.Requirements, authName, selector string) bool {
	definitions, err := fusedAuthDefinitions(auths)
	if err != nil {
		return false
	}
	for _, alternative := range requirements {
		for _, requirement := range alternative.Schemes {
			auth, ok := definitions[requirement.Scheme]
			if ok && authCredentialName(auth) == authName && isConnectedAuthSelector(canonicalFusedAuthType(auth)) && (selector == "" || canonicalFusedAuthType(auth) == selector) {
				return true
			}
		}
	}
	return false
}

func authCredentialName(auth fusedobject.AuthConfig) string {
	return strings.TrimSpace(auth.Name)
}

func AuthCredentialName(auth fusedobject.AuthConfig) string {
	return authCredentialName(auth)
}

// decryptAuthConnectionAccessToken decrypts only the access token needed for
// dispatch so refresh/id token material stays unused unless a flow needs it.
func decryptAuthConnectionAccessToken(masterKey []byte, conn *store.AuthConnection) (string, error) {
	return decryptAuthConnectionToken(masterKey, conn.EncryptedDEK, conn.EncryptedAccessToken)
}

// ResolveConnectionAccessToken applies the same proactive refresh policy used
// by SDK dispatch before returning a token for an Engine-owned control action.
func ResolveConnectionAccessToken(ctx context.Context, db store.Store, masterKey []byte, conn *store.AuthConnection, serviceVersionID uuid.UUID, authName string) (string, error) {
	resolver := &secretResolver{db: db, masterKey: masterKey, refreshCoordinator: NewAuthRefreshCoordinator(db, masterKey)}
	usable, err := resolver.usableAuthConnection(ctx, conn.BucketID, conn.ServiceID, serviceVersionID, conn.EndUserRef, authName)
	if err != nil {
		return "", err
	}
	token, err := decryptAuthConnectionAccessToken(masterKey, usable)
	if err != nil {
		return "", errors.New("failed to decrypt auth connection")
	}
	return token, nil
}

// credentialString treats missing/non-string passthrough entries as absent so
// malformed SDK inputs fail through normal connected-auth validation.
func credentialString(credentials map[string]any, key string) string {
	value, _ := credentials[key].(string)
	return value
}

// GetWebhookSecret is a targeted point-lookup for a webhook signing secret.
// secretRef is a ${bucket.<name>.secret.<key>} reference (plan item 4,
// bracketed grammar shared with connectionprofile's ${resource.*}
// expressions -- see internal/shared/secretref) -- stored verbatim on the
// WorkspaceWebhook row alongside its resolved immutable bucket ID. The
// reference supplies the key only: its human-readable bucket name can never
// redirect runtime lookup. An empty ref and zero bucket ID mean no signing
// secret is configured; the caller decides what that means for verification.
func (r *secretResolver) GetWebhookSecret(ctx context.Context, accountID, bucketID uuid.UUID, secretRef string) (string, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "GetWebhookSecret")
	defer span.End()

	if strings.TrimSpace(secretRef) == "" && bucketID == uuid.Nil {
		return "", nil
	}
	if strings.TrimSpace(secretRef) == "" || bucketID == uuid.Nil {
		return "", errors.New("stored webhook secret binding is incomplete")
	}
	if err := r.db.VerifyWorkspaceOwner(ctx, accountID); err != nil {
		return "", fmt.Errorf("failed to get workspace ID: %w", err)
	}

	ref, err := parseWebhookSecretRef(secretRef)
	if err != nil {
		return "", err
	}
	// Audit trail for inbound webhook verification: which bucket a signing
	// secret resolved against is the first thing worth knowing when a
	// delivery fails signature checks for reasons that aren't obvious from
	// the provider's response alone. Key/value are never attached to spans.
	span.SetAttributes(attribute.String("bucket_id", bucketID.String()))

	// Bucket secrets are not service-scoped (see prepareWorkspaceBucketSecrets),
	// so the lookup key is service_id = uuid.Nil, exactly as they were stored.
	sec, err := r.db.GetSecret(ctx, bucketID, uuid.Nil, ref.SecretKeyName())
	if err != nil {
		return "", fmt.Errorf("failed to fetch webhook secret: %w", err)
	}
	if sec == nil {
		return "", nil // no secret configured — caller decides what to do
	}
	return r.decryptWebhookSecret(*sec, secretRef)
}

// parseWebhookSecretRef parses and validates that a stored webhook SecretRef
// resolves to the bucket's secret store. Split out of GetWebhookSecret so
// that function's own control flow stays a linear list of store round trips
// rather than growing a validation branch on top of each of them.
func parseWebhookSecretRef(secretRef string) (secretref.Ref, error) {
	ref, err := secretref.Parse(secretRef)
	if err != nil {
		return secretref.Ref{}, fmt.Errorf("stored webhook secret reference is invalid: %w", err)
	}
	// A signing secret can only ever come from the bucket's secret store --
	// an env-kind reference here means the stored row was corrupted or
	// authored against a field it doesn't apply to, and should fail closed
	// rather than silently look up the wrong store.
	if ref.Kind != secretref.KindSecret {
		return secretref.Ref{}, errors.New("webhook secret reference must resolve to a secret, not an env value")
	}
	return ref, nil
}

// decryptWebhookSecret enforces expiry and decrypts a resolved webhook
// signing secret row. secretRef is only used for the expiry error message.
func (r *secretResolver) decryptWebhookSecret(sec store.WorkspaceSecret, secretRef string) (string, error) {
	if sec.ExpiresAt != nil && sec.ExpiresAt.Before(time.Now().UTC()) {
		return "", fmt.Errorf("%w: webhook secret %q expired at %s", ErrCredentialExpired, secretRef, sec.ExpiresAt.Format(time.RFC3339))
	}
	dek, err := store.UnwrapDEK(r.masterKey, sec.EncryptedDEK)
	if err != nil {
		return "", fmt.Errorf("failed to unwrap DEK: %w", err)
	}
	return store.DecryptWithDEK(dek, sec.EncryptedValue)
}
