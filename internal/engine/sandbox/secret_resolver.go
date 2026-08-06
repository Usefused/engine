package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/requestbinding"
	"github.com/Usefused/engine/internal/engine/store"
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
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	OperationID      string
	AuthType         string
	Auths            fusedobject.AuthConfigs
	Passthrough      map[string]any
}

type secretResolver struct {
	db        store.Store
	masterKey []byte
}

func NewSecretResolver(db store.Store, masterKey []byte) SecretResolver {
	return &secretResolver{
		db:        db,
		masterKey: masterKey,
	}
}

// ResolveExecutionCredentials uses the full dispatch identity so secret and
// binding queries remain pinned to one service version, operation, and auth
// scheme rather than loading a service-wide credential set.
func (r *secretResolver) ResolveExecutionCredentials(ctx context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "ResolveCredentials")
	defer span.End()

	scope, err := r.db.GetAppRuntime(ctx, request.AppID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load SDK scope for secrets: %w", err)
	}
	bindings, err := r.loadBucketBindings(ctx, scope.BucketID, request)
	if err != nil {
		return nil, nil, err
	}

	bindings = appendInjectionBindings(bindings, scope.Selections, request.ServiceID)

	finalCreds := copyPassthroughCredentials(request.Passthrough)
	if requestbinding.HasDynamicSource(bindings) {
		finalCreds["fused_resource_required"] = "true"
	}
	if err := r.mergeStoredSecrets(ctx, scope.BucketID, request.ServiceID, finalCreds, request.Auths); err != nil {
		return nil, nil, err
	}
	if err := r.resolveConnectedAuth(ctx, scope.BucketID, request.ServiceID, request.Auths, finalCreds); err != nil {
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

func appendInjectionBindings(bindings []store.WorkspaceConnectionBinding, selectionsRaw []byte, serviceID uuid.UUID) []store.WorkspaceConnectionBinding {
	var selections []models.SDKSelection
	if err := json.Unmarshal(selectionsRaw, &selections); err == nil {
		for _, sel := range selections {
			if sel.ServiceID == serviceID {
				for _, inj := range sel.Injections {
					injVal := inj.Value
					bindings = append(bindings, store.WorkspaceConnectionBinding{
						TargetLocation: inj.Location,
						TargetName:     inj.Name,
						SourceKind:     "literal",
						LiteralValue:   &injVal,
					})
				}
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
func (r *secretResolver) mergeStoredSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, credentials map[string]any, auths fusedobject.AuthConfigs) error {
	keys := missingCredentialKeys(credentials, requiredStaticSecretKeys(auths, credentials))
	if len(keys) == 0 {
		return nil
	}
	secrets, err := r.db.GetSecrets(ctx, bucketID, serviceID, keys)
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

// missingCredentialKeys keeps per-call overrides DB-free and lets basic/mTLS
// style multi-part auth resolve through one exact-key batch query.
func missingCredentialKeys(credentials map[string]any, keys []string) []string {
	missing := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, exists := credentials[key]; !exists {
			missing = append(missing, key)
		}
	}
	return missing
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
func (r *secretResolver) resolveConnectedAuth(ctx context.Context, bucketID, serviceID uuid.UUID, auths fusedobject.AuthConfigs, credentials map[string]any) error {
	endUserRef := connectedEndUserRef(credentials)
	if endUserRef == "" {
		return nil
	}
	if selector := requestedAuthType(credentials); selector != "" && !isConnectedAuthSelector(selector) {
		// A selected static scheme, such as basic or api_key, is satisfied from
		// bucket secrets; endUserRef should remain harmless metadata in that path.
		return nil
	}
	authName, err := selectedConnectedAuthName(credentials, auths)
	if err != nil {
		return err
	}
	if authName == "" {
		// The user reference alone identifies who connected; the auth name tells
		// the dispatcher which provider slot receives the decrypted access token.
		return errors.New("connected auth requires fused_auth_name or fused_auth_type")
	}
	conn, err := r.usableAuthConnection(ctx, bucketID, serviceID, endUserRef, authName, auths)
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
		if err := connectionResourceSelectionError(resourceID, activeCount); err != nil {
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
func connectionResourceSelectionError(requested *uuid.UUID, activeCount int) error {
	if requested != nil {
		return errors.New("connection resource not found for connected user")
	}
	if activeCount > 1 {
		return errors.New("resource_selection_required: pass fused.resourceId")
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
func selectedConnectedAuthName(credentials map[string]any, auths fusedobject.AuthConfigs) (string, error) {
	if authName := credentialString(credentials, "fused_auth_name"); authName != "" {
		return authName, nil
	}
	selector := requestedAuthType(credentials)
	if selector == "" {
		return defaultConnectedAuthName(auths), nil
	}
	if !isConnectedAuthSelector(selector) {
		// Static auth can share request builders with connected auth. Selecting a
		// static type must not trigger a user-connection lookup just because an
		// endUserRef is present.
		return "", nil
	}
	authName := connectedAuthNameForType(auths, selector)
	if authName == "" {
		return "", fmt.Errorf("connected auth type %q is not configured for this service", selector)
	}
	return authName, nil
}

// usableAuthConnection refreshes near-expiry tokens before dispatch while
// still allowing a currently-valid token through if the provider is flaky.
func (r *secretResolver) usableAuthConnection(ctx context.Context, bucketID, serviceID uuid.UUID, endUserRef, authName string, auths fusedobject.AuthConfigs) (*store.AuthConnection, error) {
	conn, err := r.db.GetAuthConnection(ctx, bucketID, serviceID, endUserRef)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch auth connection: %w", err)
	}
	if conn == nil {
		return nil, store.ErrAuthConnectionNotFound
	}
	// Once Engine has classified the grant as permanently unusable, later SDK
	// calls must receive the same action instead of dispatching a stale token.
	if conn.RefreshState == reconnectRequiredCode {
		return nil, newReconnectRequiredError(conn, "stored_grant_unusable")
	}
	now := time.Now().UTC()
	if refreshed, err := r.refreshIfDue(ctx, conn, authName, auths, now); err != nil {
		return nil, err
	} else if refreshed != nil {
		return refreshed, nil
	}
	return conn, nil
}

// refreshIfDue starts refresh inside the proactive window; permanent grant
// failures block immediately while transient failures wait for access expiry.
func (r *secretResolver) refreshIfDue(ctx context.Context, conn *store.AuthConnection, authName string, auths fusedobject.AuthConfigs, now time.Time) (*store.AuthConnection, error) {
	if !authConnectionNeedsRefresh(conn, now) {
		return nil, nil
	}
	// A provider-declared refresh expiry is permanent, so waiting for a network
	// request would only obscure the action the customer application must take.
	if authConnectionRefreshTokenExpired(conn, now) {
		return nil, r.reconnectRequiredError(ctx, conn, "refresh_token_expired")
	}
	// Access-only grants remain usable until access expiry; forcing consent in
	// the proactive window would throw away provider-authorized token lifetime.
	if conn.EncryptedRefreshToken == "" {
		return nil, r.expiredConnectionError(ctx, conn, now)
	}
	refreshed, err := r.refreshAuthConnection(ctx, conn, authName, auths)
	if err == nil {
		return refreshed, nil
	}
	// invalid_grant means retries cannot repair this user's revoked or replaced
	// grant, even when the old access token has a few minutes left to live.
	if connectauth.IsReconnectRequiredRefreshError(err) {
		return nil, r.reconnectRequiredError(ctx, conn, "refresh_token_rejected")
	}
	// Transient refresh failures block only after access expiry, preserving
	// availability while never dispatching an already-expired credential.
	if authConnectionExpired(conn, now) {
		if markErr := r.markAuthConnectionRefreshState(ctx, conn, "failed", "refresh_failed_after_expiry"); markErr != nil {
			return nil, fmt.Errorf("mark failed auth refresh: %w", markErr)
		}
		return nil, fmt.Errorf("refresh connected auth: %w", err)
	}
	return nil, nil
}

// refreshAuthConnection performs the provider refresh and persists the updated
// encrypted token material back to the bucket-owned connection row.
func (r *secretResolver) refreshAuthConnection(ctx context.Context, conn *store.AuthConnection, authName string, auths fusedobject.AuthConfigs) (*store.AuthConnection, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.auth_connection.refresh")
	defer span.End()
	span.SetAttributes(authConnectionSpanAttrs(conn)...)
	auth, err := connectedRefreshAuth(authName, auths)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cfg, err := r.db.GetConnectConfig(ctx, conn.BucketID, conn.ServiceID)
	if err != nil {
		span.SetStatus(codes.Error, "load connect config for refresh")
		return nil, fmt.Errorf("load connect config for refresh: %w", err)
	}
	if cfg == nil || !cfg.Enabled {
		span.SetStatus(codes.Error, "connect config not found for refresh")
		return nil, errors.New("connect config not found for refresh")
	}
	creds, err := connectauth.DecryptClientCredentials(cfg, r.masterKey)
	if err != nil {
		span.SetStatus(codes.Error, "decrypt connect config for refresh")
		return nil, fmt.Errorf("decrypt connect config for refresh: %w", err)
	}
	refreshToken, err := decryptAuthConnectionToken(r.masterKey, conn.EncryptedDEK, conn.EncryptedRefreshToken)
	if err != nil {
		span.SetStatus(codes.Error, "decrypt refresh token")
		return nil, fmt.Errorf("decrypt refresh token: %w", err)
	}
	token, err := connectauth.RefreshAccessToken(ctx, http.DefaultClient, auth, creds, refreshToken)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	updated, err := r.connectionFromRefreshToken(conn, auth, token)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	saved, err := r.db.UpsertAuthConnection(ctx, updated)
	if err != nil {
		span.SetStatus(codes.Error, "persist refreshed auth connection")
		return nil, err
	}
	span.SetStatus(codes.Ok, "auth connection refreshed")
	return saved, nil
}

// connectionFromRefreshToken reuses the existing connection identity and DEK so
// refresh updates only the provider token material, not bucket ownership.
func (r *secretResolver) connectionFromRefreshToken(conn *store.AuthConnection, auth fusedobject.AuthConfig, token connectauth.TokenResponse) (store.AuthConnection, error) {
	dek, err := store.UnwrapDEK(r.masterKey, conn.EncryptedDEK)
	if err != nil {
		return store.AuthConnection{}, err
	}
	access, err := store.EncryptWithDEK(dek, token.AccessToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	refresh, err := refreshedOptionalToken(dek, token.RefreshToken, conn.EncryptedRefreshToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	idToken, err := refreshedOptionalToken(dek, token.IDToken, conn.EncryptedIDToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	claims := refreshIdentityClaims(token, conn)
	updated := *conn
	updated.EncryptedAccessToken = access
	updated.EncryptedRefreshToken = refresh
	updated.EncryptedIDToken = idToken
	updated.TokenType = connectauth.DefaultTokenType(token.TokenType)
	// Refresh responses commonly omit scope; preserving the connection's last
	// known set avoids reporting the service's broader catalogue as granted.
	scopeSet := connectauth.TokenScopeMetadata(token, conn.Scopes, conn.ScopeSource)
	updated.Scopes = scopeSet.Scopes
	updated.ScopeSource = scopeSet.Source
	updated.Issuer = connectauth.ClaimString(claims, "iss")
	updated.Subject = connectauth.ClaimString(claims, "sub")
	updated.IdentityClaims = connectauth.ClaimBytes(claims)
	updated.ExpiresAt = connectauth.TokenExpiresAt(token.ExpiresIn)
	if refreshExpiresAt := connectauth.RefreshTokenExpiresAt(token.RefreshTokenExpiresIn); refreshExpiresAt != nil {
		// Providers that omit refresh-token TTL on refresh are saying "no new
		// information"; keep the previous reconnect deadline instead of
		// accidentally making long-lived refresh material look unbounded.
		updated.RefreshTokenExpiresAt = refreshExpiresAt
	}
	updated.RefreshState = "ok"
	// A successful refresh is authoritative evidence that any prior diagnostic
	// is stale, so the connection returns to a clean operator-visible state.
	updated.LastFailureCode = ""
	updated.LastFailureAt = nil
	updated.LastFailureTraceID = ""
	return updated, nil
}

// refreshIdentityClaims preserves existing OIDC metadata when a provider
// refreshes only the access token and omits a new id_token.
func refreshIdentityClaims(token connectauth.TokenResponse, conn *store.AuthConnection) map[string]any {
	if strings.TrimSpace(token.IDToken) != "" {
		return connectauth.OIDCClaims(token.IDToken)
	}
	var claims map[string]any
	if len(conn.IdentityClaims) == 0 {
		return nil
	}
	if err := json.Unmarshal(conn.IdentityClaims, &claims); err != nil {
		return nil
	}
	return claims
}

// connectedRefreshAuth selects by the dispatcher auth name first so refresh
// uses the same provider config that will receive the access token.
func connectedRefreshAuth(authName string, auths fusedobject.AuthConfigs) (fusedobject.AuthConfig, error) {
	for _, auth := range auths {
		if authCredentialName(auth) == authName && isRefreshableAuth(auth) {
			return validateRefreshableAuth(auth)
		}
	}
	return fusedobject.AuthConfig{}, errors.New("refreshable auth config not found")
}

// validateRefreshableAuth fails before network I/O when registry metadata
// cannot support OAuth refresh.
func validateRefreshableAuth(auth fusedobject.AuthConfig) (fusedobject.AuthConfig, error) {
	if strings.TrimSpace(auth.TokenURL) == "" {
		return fusedobject.AuthConfig{}, errors.New("refresh token_url is required")
	}
	return auth, nil
}

// isRefreshableAuth mirrors Engine-owned connect support so static/API-key
// credentials never enter OAuth refresh paths.
func isRefreshableAuth(auth fusedobject.AuthConfig) bool {
	return auth.Type == "oauth2" || auth.Type == "openIdConnect" || auth.Type == "oidc"
}

// authConnectionNeedsRefresh keeps refresh work scoped to tokens with known
// expiry that are already inside the proactive window.
func authConnectionNeedsRefresh(conn *store.AuthConnection, now time.Time) bool {
	return conn.ExpiresAt != nil && !conn.ExpiresAt.After(now.Add(connectedAuthRefreshWindow))
}

// authConnectionExpired is separate from needs-refresh because failed proactive
// refresh should not block a still-valid request.
func authConnectionExpired(conn *store.AuthConnection, now time.Time) bool {
	return conn.ExpiresAt != nil && !conn.ExpiresAt.After(now)
}

// authConnectionRefreshTokenExpired trusts a known provider TTL while treating
// an omitted TTL as unknown rather than inventing an early reconnect deadline.
func authConnectionRefreshTokenExpired(conn *store.AuthConnection, now time.Time) bool {
	return conn.RefreshTokenExpiresAt != nil && !conn.RefreshTokenExpiresAt.After(now)
}

// expiredConnectionError marks permanently unusable connections for admin
// visibility, then returns the fail-closed execution error.
func (r *secretResolver) expiredConnectionError(ctx context.Context, conn *store.AuthConnection, now time.Time) error {
	if !authConnectionExpired(conn, now) {
		return nil
	}
	return r.reconnectRequiredError(ctx, conn, "refresh_token_missing")
}

// reconnectRequiredError persists the durable operator-visible state before
// telling an SDK to begin consent, preventing execution and UI views diverging.
func (r *secretResolver) reconnectRequiredError(ctx context.Context, conn *store.AuthConnection, reason string) error {
	if err := r.markAuthConnectionRefreshState(ctx, conn, reconnectRequiredCode, reason); err != nil {
		return fmt.Errorf("mark reconnect-required auth connection: %w", err)
	}
	return newReconnectRequiredError(conn, reason)
}

// markAuthConnectionRefreshState reuses the existing encrypted token material
// so state transitions do not accidentally clear credentials.
func (r *secretResolver) markAuthConnectionRefreshState(ctx context.Context, conn *store.AuthConnection, state, reason string) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.auth_connection.refresh_state")
	defer span.End()
	span.SetAttributes(append(authConnectionSpanAttrs(conn), attribute.String("refresh_state", state))...)
	// The reason is low-cardinality decision metadata and contains no provider
	// response text, making it safe for audit/debug telemetry.
	if reason != "" {
		span.SetAttributes(attribute.String("refresh_state_reason", reason))
	}
	updated := *conn
	updated.RefreshState = state
	// Only stable Engine decision codes are persisted; raw provider errors could
	// contain user data or credential-adjacent response details.
	updated.LastFailureCode = reason
	failedAt := time.Now().UTC()
	updated.LastFailureAt = &failedAt
	updated.LastFailureTraceID = executionTraceID(ctx)
	_, err := r.db.UpsertAuthConnection(ctx, updated)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "auth connection refresh state updated")
	return err
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

// refreshedOptionalToken preserves the existing encrypted token when providers
// omit optional fields during refresh, which avoids bricking rotating-token
// providers that return a refresh token only on some responses.
func refreshedOptionalToken(dek []byte, value, fallbackEncrypted string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return fallbackEncrypted, nil
	}
	return store.EncryptWithDEK(dek, value)
}

// decryptAuthConnectionToken decrypts token fields by explicit ciphertext so
// access and refresh token paths cannot accidentally swap stored fields.
func decryptAuthConnectionToken(masterKey []byte, encryptedDEK, encryptedValue string) (string, error) {
	dek, err := store.UnwrapDEK(masterKey, encryptedDEK)
	if err != nil {
		return "", err
	}
	return store.DecryptWithDEK(dek, encryptedValue)
}

// WithDefaultConnectedAuthName lets generated SDKs avoid service-specific auth
// field names while still targeting the correct dispatcher credential slot.
func WithDefaultConnectedAuthName(credentials map[string]any, auths fusedobject.AuthConfigs) map[string]any {
	authName := defaultConnectedAuthNameForCredentials(credentials, auths)
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
func defaultConnectedAuthName(auths fusedobject.AuthConfigs) string {
	for _, auth := range auths {
		if isConnectedAuthSelector(canonicalFusedAuthType(auth)) {
			return authCredentialName(auth)
		}
	}
	return ""
}

// defaultConnectedAuthNameForCredentials lets authType select the connected
// credential slot while preserving the old first-OAuth/OIDC default.
func defaultConnectedAuthNameForCredentials(credentials map[string]any, auths fusedobject.AuthConfigs) string {
	selector := requestedAuthType(credentials)
	if selector == "" {
		return defaultConnectedAuthName(auths)
	}
	return connectedAuthNameForType(auths, selector)
}

func authCredentialName(auth fusedobject.AuthConfig) string {
	name := strings.TrimSpace(auth.Name)
	if name != "" {
		return name
	}
	if canonicalFusedAuthType(auth) == "api_key" && strings.TrimSpace(auth.KeyName) != "" {
		// Unnamed API-key schemes still have a concrete provider key name; using
		// it here keeps bucket storage and applyAPIKey lookup aligned.
		return strings.TrimSpace(auth.KeyName)
	}
	if auth.Type == "oauth2" || auth.Type == "openIdConnect" || auth.Type == "oidc" ||
		auth.Type == "http" && strings.EqualFold(auth.Scheme, "bearer") {
		// Some imported OpenAPI auth schemes omit a friendly auth name even
		// though the generated SDK and dispatcher both use Authorization for
		// bearer-style credentials. Normalising here keeps token lookup and
		// outbound header application on the same non-secret key.
		return "Authorization"
	}
	if canonicalFusedAuthType(auth) == "mtls" {
		// mTLS has no header name to fall back to; use a stable pair prefix so
		// cert/key bucket secrets can still be resolved for unnamed imports.
		return "mtls"
	}
	return ""
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
func ResolveConnectionAccessToken(ctx context.Context, db store.Store, masterKey []byte, conn *store.AuthConnection, authName string, auths fusedobject.AuthConfigs) (string, error) {
	resolver := &secretResolver{db: db, masterKey: masterKey}
	usable, err := resolver.usableAuthConnection(ctx, conn.BucketID, conn.ServiceID, conn.EndUserRef, authName, auths)
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
