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
		if !ok {
			http.Error(w, message, status)
			return
		}

		var payload SecretUpsertPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if payload.ExpiresAt != nil && payload.ExpiresAt.Before(time.Now()) {
			http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
			return
		}
		if credentialTypeRequiresPair(payload.CredentialType) {
			http.Error(w, "paired credentials must be saved together", http.StatusBadRequest)
			return
		}

		bucketID, status, err := resolveSecretBucketID(ctx, s, payload.BucketID)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketLookupError(w, err)
			return
		}

		span.SetAttributes(
			attribute.String("service_id", payload.ServiceID.String()),
			attribute.String("credential_type", canonicalSecretCredentialType(payload.CredentialType)),
			attribute.String("bucket_id", bucketID.String()),
		)

		secret, err := buildEncryptedSecret(bucketID, payload, masterKey)
		if err != nil {
			http.Error(w, "failed to encrypt secret", http.StatusInternalServerError)
			return
		}

		if err := s.UpsertSecret(ctx, secret); err != nil {
			slog.ErrorContext(ctx, "failed to upsert secret", slog.Any("error", err))
			http.Error(w, "failed to save secret", http.StatusInternalServerError)
			return
		}

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
		if !ok {
			http.Error(w, message, status)
			return
		}

		payload, err := decodeSecretBulkUpsertPayload(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bucketID, status, err := resolveSecretBucketID(ctx, s, payload.BucketID)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketLookupError(w, err)
			return
		}
		if err := validateSecretBulkPayload(payload.Secrets, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		secrets, err := buildEncryptedSecrets(bucketID, payload.Secrets, masterKey)
		if err != nil {
			http.Error(w, "failed to encrypt secrets", http.StatusInternalServerError)
			return
		}
		if err := s.UpsertSecrets(ctx, secrets); err != nil {
			slog.ErrorContext(ctx, "failed to upsert secrets", slog.Any("error", err))
			http.Error(w, "failed to save secrets", http.StatusInternalServerError)
			return
		}

		span.SetAttributes(
			attribute.String("bucket_id", bucketID.String()),
			attribute.Int("secret_count", len(secrets)),
			attribute.Int("mtls_pair_count", countMTLSPairs(payload.Secrets)),
		)
		w.WriteHeader(http.StatusNoContent)
	}
}

// secretAdminWorkspace is shared by single and batch writes so credential
// handlers do not drift on authorization or workspace-resolution semantics.
func secretAdminWorkspace(ctx context.Context, s store.Store, r *http.Request) (int, string, bool) {
	accountID, err := controlActorAccount(ctx)
	if err != nil {
		return http.StatusUnauthorized, "unauthorized", false
	}
	if err := verifyWorkspaceActor(ctx, accountID); err != nil {
		return http.StatusInternalServerError, "failed to resolve workspace", false
	}
	return http.StatusOK, "", true
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
		if payload.ServiceID == uuid.Nil || strings.TrimSpace(payload.KeyName) == "" || strings.TrimSpace(payload.Value) == "" {
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

type mtlsBulkPair struct {
	cert string
	key  string
}

type basicBulkPair struct {
	username string
	password string
}

// validateBulkBasicPairs enforces the same all-or-none rule for Basic auth
// that runtime resolution expects when it fetches username and password keys.
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
		} else {
			pair.password = payload.Value
		}
	}
	for _, pair := range pairs {
		if strings.TrimSpace(pair.username) == "" || strings.TrimSpace(pair.password) == "" {
			return errors.New("basic username/password must be saved together")
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

func DeleteSecretHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.secrets.delete")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			http.Error(w, "failed to resolve workspace", http.StatusInternalServerError)
			return
		}

		serviceIDStr := r.URL.Query().Get("service_id")
		keyName := r.URL.Query().Get("key_name")
		if serviceIDStr == "" || keyName == "" {
			http.Error(w, "missing service_id or key_name", http.StatusBadRequest)
			return
		}

		serviceID, err := uuid.Parse(serviceIDStr)
		if err != nil {
			http.Error(w, "invalid service_id", http.StatusBadRequest)
			return
		}

		bucketID, status, err := resolveSecretBucketID(ctx, s, r.URL.Query().Get("bucket_id"))
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketLookupError(w, err)
			return
		}

		span.SetAttributes(
			attribute.String("service_id", serviceID.String()),
			attribute.String("bucket_id", bucketID.String()),
		)

		if err := s.DeleteSecret(ctx, bucketID, serviceID, keyName); err != nil {
			slog.ErrorContext(ctx, "failed to delete secret", slog.Any("error", err))
			http.Error(w, "failed to delete secret", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
