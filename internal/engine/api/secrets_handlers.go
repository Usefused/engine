package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"log/slog"

	"github.com/Usefused/engine/internal/engine/mtlsauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type SecretUpsertPayload struct {
	ServiceID      uuid.UUID `json:"service_id"`
	KeyName        string    `json:"key_name"`
	CredentialType string    `json:"credential_type"`
	BucketID       string    `json:"bucket_id"`
	Value          string    `json:"value"`
	// ExpiresAt is optional. Nil means the credential never expires. Set on
	// every upsert (never merged with a prior value) so clearing it just
	// means omitting it on a re-set, matching how every other field here
	// already replaces wholesale.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type SecretBulkUpsertPayload struct {
	BucketID string                `json:"bucket_id"`
	Secrets  []SecretUpsertPayload `json:"secrets"`
}

// UpsertSecretHandler stays limited to single-value credentials. mTLS uses
// the bulk endpoint so the API cannot persist only a certificate or only a key.
func UpsertSecretHandler(s store.Store, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.secrets.upsert")
		defer span.End()

		status, message, ok := secretAdminWorkspace(ctx, s, r)
		// Authentication and workspace failures are already reduced to safe local copy.
		if !ok {
			writeControlAPIMutationError(w, ctx, status, secretAdminErrorCode(status), message, secretAdminRemediation(status), "secret_upsert", "", "not_committed", "")
			return
		}

		var payload SecretUpsertPayload
		// Invalid JSON cannot safely identify credential metadata or secret value.
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_secret_request", "The secret request body is invalid.", "Check service_id, key_name, credential_type, bucket_id, and expires_at.", "secret_upsert", "", "not_committed", "")
			return
		}

		// Expired-at-write credentials can never become usable and are rejected early.
		if payload.ExpiresAt != nil && payload.ExpiresAt.Before(time.Now()) {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_secret_expiry", "expires_at must be in the future.", "Choose a future RFC3339 expiry or omit expires_at.", "secret_upsert", "", "not_committed", "")
			return
		}
		// Paired credentials must use the atomic bulk endpoint to avoid partial state.
		if credentialTypeRequiresPair(payload.CredentialType) {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "paired_credentials_required", "Paired credentials must be saved together.", "Use the paired credential fields in one secret-set command.", "secret_upsert", "", "not_committed", "")
			return
		}

		bucketID, status, err := resolveSecretBucketID(ctx, s, payload.BucketID)
		// Bucket resolution distinguishes invalid input from absent defaults.
		if err != nil {
			writeControlAPIMutationError(w, ctx, status, "secret_bucket_resolution_failed", err.Error(), "Provide an existing bucket ID or configure a default bucket.", "secret_upsert", "", "not_committed", "")
			return
		}
		// Exact lookup prevents secret writes to unknown buckets.
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketMutationLookupError(ctx, w, err, "secret_upsert")
			return
		}

		span.SetAttributes(
			attribute.String("service_id", payload.ServiceID.String()),
			attribute.String("credential_type", canonicalSecretCredentialType(payload.CredentialType)),
			attribute.String("bucket_id", bucketID.String()),
		)

		secret, err := buildEncryptedSecret(bucketID, payload, masterKey)
		// Encryption must complete before persistence receives any credential row.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "secret_encryption_failed", "The Engine could not encrypt the secret.", "Check Engine master-key configuration and retry.", "secret_upsert", "", "not_committed", "")
			return
		}

		// Unclassified store failures hide encrypted material and retain an unknown commit outcome.
		if err := s.UpsertSecret(ctx, secret); err != nil {
			slog.ErrorContext(ctx, "failed to upsert secret", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "secret_save_failed", "The Engine could not save the secret.", "Inspect current secret metadata before retrying, and use the request or trace ID to check Engine logs.", "secret_upsert", "", "unknown", "")
			return
		}
		span.SetAttributes(attribute.String("outcome", "upserted"))

		w.WriteHeader(http.StatusNoContent)
	}
}

// UpsertSecretsHandler is the admin boundary for paired and batch credential
// writes: validate once, encrypt once per value, then commit through Store.
func UpsertSecretsHandler(s store.Store, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.secrets.bulk_upsert")
		defer span.End()

		status, message, ok := secretAdminWorkspace(ctx, s, r)
		// Authentication and workspace failures are already reduced to safe local copy.
		if !ok {
			writeControlAPIMutationError(w, ctx, status, secretAdminErrorCode(status), message, secretAdminRemediation(status), "secret_bulk_upsert", "", "not_committed", "")
			return
		}

		payload, err := decodeSecretBulkUpsertPayload(r)
		// Strict decoding prevents misspelled fields from producing partial credentials.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_secret_request", err.Error(), "Check the paired credential fields and retry.", "secret_bulk_upsert", "", "not_committed", "")
			return
		}
		bucketID, status, err := resolveSecretBucketID(ctx, s, payload.BucketID)
		// Bucket resolution distinguishes invalid input from absent defaults.
		if err != nil {
			writeControlAPIMutationError(w, ctx, status, "secret_bucket_resolution_failed", err.Error(), "Provide an existing bucket ID or configure a default bucket.", "secret_bulk_upsert", "", "not_committed", "")
			return
		}
		// Exact lookup prevents secret writes to unknown buckets.
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketMutationLookupError(ctx, w, err, "secret_bulk_upsert")
			return
		}
		// Validate the complete set before encryption so pairs commit atomically.
		if err := validateSecretBulkPayload(payload.Secrets, time.Now()); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_secret_set", err.Error(), "Provide every required paired credential field in the same request.", "secret_bulk_upsert", "", "not_committed", "")
			return
		}

		secrets, err := buildEncryptedSecrets(bucketID, payload.Secrets, masterKey)
		// Any encryption failure aborts the whole pair before persistence.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "secret_encryption_failed", "The Engine could not encrypt the credential set.", "Check Engine master-key configuration and retry.", "secret_bulk_upsert", "", "not_committed", "")
			return
		}
		// Unclassified atomic-store failures retain an unknown commit outcome at the handler boundary.
		if err := s.UpsertSecrets(ctx, secrets); err != nil {
			slog.ErrorContext(ctx, "failed to upsert secrets", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "secret_save_failed", "The Engine could not save the credential set.", "Inspect current secret metadata before retrying, and use the request or trace ID to check Engine logs.", "secret_bulk_upsert", "", "unknown", "")
			return
		}

		span.SetAttributes(
			attribute.String("bucket_id", bucketID.String()),
			attribute.Int("secret_count", len(secrets)),
			attribute.Int("mtls_pair_count", countMTLSPairs(payload.Secrets)),
			attribute.String("outcome", "upserted"),
		)
		w.WriteHeader(http.StatusNoContent)
	}
}

// secretAdminWorkspace is shared by single and batch writes so credential
// handlers do not drift on authorization or workspace-resolution semantics.
func secretAdminWorkspace(ctx context.Context, s store.Store, r *http.Request) (int, string, bool) {
	accountID, err := controlActorAccount(ctx)
	// Anonymous callers cannot mutate workspace credential material.
	if err != nil {
		return http.StatusUnauthorized, "Authentication is required to manage secrets.", false
	}
	// Workspace resolution is required before any bucket-scoped secret operation.
	if err := verifyWorkspaceActor(ctx, accountID); err != nil {
		return http.StatusInternalServerError, "The Engine could not resolve the workspace for secret management.", false
	}
	return http.StatusOK, "", true
}

// secretAdminErrorCode returns a stable code for the two authorization setup
// failures emitted by secretAdminWorkspace.
func secretAdminErrorCode(status int) string {
	// Only an authentication status represents a caller credential problem.
	if status == http.StatusUnauthorized {
		return "authentication_required"
	}
	return "workspace_resolution_failed"
}

// secretAdminRemediation supplies status-appropriate next steps without
// relying on mutable error text.
func secretAdminRemediation(status int) string {
	// Authentication failures are fixed by credentials, not Engine retries.
	if status == http.StatusUnauthorized {
		return "Log in or provide a valid Fused credential."
	}
	return "Retry and check Engine logs if the problem continues."
}

// decodeSecretBulkUpsertPayload rejects unknown fields so mistyped auth
// material fields fail loudly instead of becoming partial credentials.
func decodeSecretBulkUpsertPayload(r *http.Request) (SecretBulkUpsertPayload, error) {
	var payload SecretBulkUpsertPayload
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, errors.New("invalid request body")
	}
	if len(payload.Secrets) == 0 {
		return payload, errors.New("secrets is required")
	}
	return payload, nil
}

// validateSecretBulkPayload checks cheap, non-secret invariants before any
// encryption work and before the store transaction is opened.
func validateSecretBulkPayload(payloads []SecretUpsertPayload, now time.Time) error {
	for _, payload := range payloads {
		if payload.ServiceID == uuid.Nil || strings.TrimSpace(payload.KeyName) == "" || secretValueMissing(payload) {
			return errors.New("service_id, key_name, and value are required")
		}
		if payload.ExpiresAt != nil && payload.ExpiresAt.Before(now) {
			return errors.New("expires_at must be in the future")
		}
	}
	if err := validateBulkBasicPairs(payloads); err != nil {
		return err
	}
	return validateBulkMTLSPairs(payloads, now)
}

// Empty Basic passwords are data, not missing input: several providers use
// the username slot for an API key and require an explicitly empty password.
func secretValueMissing(payload SecretUpsertPayload) bool {
	if canonicalSecretCredentialType(payload.CredentialType) == "basic" && strings.HasSuffix(strings.TrimSpace(payload.KeyName), "_password") {
		return false
	}
	return strings.TrimSpace(payload.Value) == ""
}

type mtlsBulkPair struct {
	cert string
	key  string
}

type basicBulkPair struct {
	username    string
	usernameSet bool
}

// validateBulkBasicPairs rejects password-only writes while allowing the
// provider-neutral required/optional/empty password modes to share one API.
func validateBulkBasicPairs(payloads []SecretUpsertPayload) error {
	pairs := map[string]*basicBulkPair{}
	for _, payload := range payloads {
		if canonicalSecretCredentialType(payload.CredentialType) != "basic" {
			continue
		}
		group, field, ok := basicBulkKey(payload)
		if !ok {
			return errors.New("basic credentials must use matching _username and _password names")
		}
		pair := pairs[group]
		if pair == nil {
			pair = &basicBulkPair{}
			pairs[group] = pair
		}
		if field == "username" {
			pair.username = payload.Value
			pair.usernameSet = true
		}
	}
	for _, pair := range pairs {
		if !pair.usernameSet || strings.TrimSpace(pair.username) == "" {
			return errors.New("basic username is required when saving Basic credentials")
		}
	}
	return nil
}

func validateBulkMTLSPairs(payloads []SecretUpsertPayload, now time.Time) error {
	pairs := map[string]*mtlsBulkPair{}
	for _, payload := range payloads {
		if canonicalSecretCredentialType(payload.CredentialType) != "mtls" {
			continue
		}
		group, field, ok := mtlsBulkKey(payload)
		if !ok {
			return errors.New("mTLS secrets must use matching _cert and _key names")
		}
		pair := pairs[group]
		if pair == nil {
			pair = &mtlsBulkPair{}
			pairs[group] = pair
		}
		if field == "cert" {
			pair.cert = payload.Value
		} else {
			pair.key = payload.Value
		}
	}
	for _, pair := range pairs {
		// Cert/key parsing happens before encryption so broken mTLS material is
		// rejected at the admin boundary, not later during a provider request.
		if _, err := mtlsauth.CertificatePair(pair.cert, pair.key, now); err != nil {
			return errors.New("mTLS certificate/key invalid or mismatched")
		}
	}
	return nil
}

// basicBulkKey derives the auth family from bucket key names so direct API
// callers follow the same scheme-specific naming used by CLI/UI generation.
func basicBulkKey(payload SecretUpsertPayload) (string, string, bool) {
	keyName := strings.TrimSpace(payload.KeyName)
	for _, suffix := range []string{"_username", "_password"} {
		if strings.HasSuffix(keyName, suffix) {
			return payload.ServiceID.String() + ":" + strings.TrimSuffix(keyName, suffix), strings.TrimPrefix(suffix, "_"), true
		}
	}
	return "", "", false
}

// mtlsBulkKey derives the auth family from bucket key names because runtime
// dispatch resolves the same <authName>_cert/<authName>_key convention.
func mtlsBulkKey(payload SecretUpsertPayload) (string, string, bool) {
	keyName := strings.TrimSpace(payload.KeyName)
	for _, suffix := range []string{"_cert", "_key"} {
		if strings.HasSuffix(keyName, suffix) {
			return payload.ServiceID.String() + ":" + strings.TrimSuffix(keyName, suffix), strings.TrimPrefix(suffix, "_"), true
		}
	}
	return "", "", false
}

// countMTLSPairs emits only aggregate telemetry so debugging has shape without
// leaking credential key names or certificate fingerprints.
func countMTLSPairs(payloads []SecretUpsertPayload) int {
	groups := map[string]struct{}{}
	for _, payload := range payloads {
		if canonicalSecretCredentialType(payload.CredentialType) != "mtls" {
			continue
		}
		if group, _, ok := mtlsBulkKey(payload); ok {
			groups[group] = struct{}{}
		}
	}
	return len(groups)
}

// buildEncryptedSecrets keeps plaintext inside the HTTP boundary; Store only
// receives encrypted values and wrapped DEKs.
func buildEncryptedSecrets(bucketID uuid.UUID, payloads []SecretUpsertPayload, masterKey []byte) ([]store.WorkspaceSecret, error) {
	secrets := make([]store.WorkspaceSecret, 0, len(payloads))
	for _, payload := range payloads {
		secret, err := buildEncryptedSecret(bucketID, payload, masterKey)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

// canonicalSecretCredentialType accepts imported security-scheme spellings but
// stores and validates against the public credential type vocabulary.
func canonicalSecretCredentialType(value string) string {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", "_"))
	if normalized == "mutualtls" || normalized == "mutual_tls" {
		return "mtls"
	}
	return normalized
}

// credentialTypeRequiresPair centralizes the direct-write blocklist so future
// paired auth families do not accidentally bypass the bulk validation path.
func credentialTypeRequiresPair(value string) bool {
	credentialType := canonicalSecretCredentialType(value)
	return credentialType == "basic" || credentialType == "mtls"
}

func resolveSecretBucketID(ctx context.Context, s store.Store, rawBucketID string) (uuid.UUID, int, error) {
	if rawBucketID != "" {
		bucketID, err := uuid.Parse(rawBucketID)
		if err != nil {
			return uuid.Nil, http.StatusBadRequest, errors.New("invalid bucket_id")
		}
		return bucketID, http.StatusOK, nil
	}
	bucketID, err := defaultBucketID(ctx, s)
	if err != nil {
		return uuid.Nil, http.StatusInternalServerError, err
	}
	return bucketID, http.StatusOK, nil
}

func defaultBucketID(ctx context.Context, s store.Store) (uuid.UUID, error) {
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return uuid.Nil, errors.New("no default bucket found")
	}
	for _, bucket := range buckets {
		if bucket.IsDefault {
			return bucket.ID, nil
		}
	}
	return uuid.Nil, errors.New("no default bucket found")
}

func buildEncryptedSecret(bucketID uuid.UUID, payload SecretUpsertPayload, masterKey []byte) (store.WorkspaceSecret, error) {
	// Secrets are write-only in Engine reads, so the handler encrypts once at
	// the boundary and Store never receives plaintext credential material.
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return store.WorkspaceSecret{}, err
	}
	encryptedValue, err := store.EncryptWithDEK(dek, payload.Value)
	if err != nil {
		return store.WorkspaceSecret{}, err
	}
	return store.WorkspaceSecret{
		WorkspaceSecretMeta: store.WorkspaceSecretMeta{
			BucketID:       bucketID,
			ServiceID:      payload.ServiceID,
			KeyName:        payload.KeyName,
			CredentialType: payload.CredentialType,
			ExpiresAt:      payload.ExpiresAt,
		},
		EncryptedDEK:   wrappedDEK,
		EncryptedValue: encryptedValue,
	}, nil
}

// DeleteSecretHandler deletes one exact bucket/service/key secret and emits
// structured diagnostics without exposing any credential material.
func DeleteSecretHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.secrets.delete")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		// Secret deletion requires an authenticated control-plane actor.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to delete a secret.", "Log in or provide a valid Fused credential.", "secret_delete", "", "not_committed", "")
			return
		}

		// Workspace resolution prevents deletion through a foreign identity.
		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for secret deletion.", "Retry and check Engine logs if the problem continues.", "secret_delete", "", "not_committed", "")
			return
		}

		serviceIDStr := r.URL.Query().Get("service_id")
		keyName := r.URL.Query().Get("key_name")
		// Exact service and key identity is required for a safe deletion.
		if serviceIDStr == "" || keyName == "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "secret_identity_required", "The service ID and key name are required.", "Provide service_id and key_name query values.", "secret_delete", "", "not_committed", "")
			return
		}

		serviceID, err := uuid.Parse(serviceIDStr)
		// Reject malformed service identity before bucket or secret lookup.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_service_id", "The service ID is not a valid UUID.", "Use the service ID shown by the bucket or service commands.", "secret_delete", "", "not_committed", "")
			return
		}

		bucketID, status, err := resolveSecretBucketID(ctx, s, r.URL.Query().Get("bucket_id"))
		// Omitted bucket IDs may resolve only through the authoritative default.
		if err != nil {
			writeControlAPIMutationError(w, ctx, status, "secret_bucket_resolution_failed", err.Error(), "Provide an existing bucket ID or configure a default bucket.", "secret_delete", "", "not_committed", "")
			return
		}
		// Exact lookup prevents deletion from an unknown bucket.
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketMutationLookupError(ctx, w, err, "secret_delete")
			return
		}

		span.SetAttributes(
			attribute.String("service_id", serviceID.String()),
			attribute.String("bucket_id", bucketID.String()),
		)

		// Unclassified store failures hide database detail and retain an unknown deletion outcome.
		if err := s.DeleteSecret(ctx, bucketID, serviceID, keyName); err != nil {
			// A live reference makes deletion deterministically impossible; expose
			// that contract instead of disguising it as an unknown database failure.
			if errors.Is(err, store.ErrWorkspaceAuthReferenceInUse) {
				writeControlAPIMutationError(w, ctx, http.StatusConflict, "workspace_auth_reference_in_use", "The credential is used by another workspace service.", "Replace the dependent auth ref or remove its destination service before deleting this credential.", "secret_delete", "", "not_committed", "")
				return
			}
			slog.ErrorContext(ctx, "failed to delete secret", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "secret_delete_failed", "The Engine could not delete the secret.", "Inspect current secret metadata before retrying, and use the request or trace ID to check Engine logs.", "secret_delete", "", "unknown", "")
			return
		}
		span.SetAttributes(attribute.String("outcome", "deleted"))

		w.WriteHeader(http.StatusNoContent)
	}
}
