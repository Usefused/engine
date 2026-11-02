package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OAuthRuntimeScope authorizes one API app provisioning ceremony, never management calls.
const OAuthRuntimeScope = "api.execute"

// OAuthDeploymentScope delegates only the manifest-bound app deployment endpoint.
const OAuthDeploymentScope = "app.api.deploy"

// OAuthRuntimeGrant pins execution to the reviewed immutable API app and compiled manifest.
type OAuthRuntimeGrant struct {
	// ContractBindings retain original provider schema/content identity across all later app deployments.
	ContractBindings []models.SDKContractBinding `json:"contract_bindings,omitempty"`
	AppID            uuid.UUID                   `json:"app_id"`
	AppFamilyID      uuid.UUID                   `json:"app_family_id"`
	ManifestDigest   string                      `json:"manifest_digest"`
	DefaultBucketID  uuid.UUID                   `json:"default_bucket_id"`
	Buckets          []OAuthRuntimeBucket        `json:"buckets"`
}

// OAuthRuntimeBucket exposes approved routing choices without credential material.
type OAuthRuntimeBucket struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type runtimeRegistrationKey struct{}
type runtimeConsentKey struct{}
type runtimeExchangeKey struct{}

// RuntimeExchange transfers a freshly generated token hash into the atomic code exchange.
type RuntimeExchange struct {
	TokenHash string
	Grant     OAuthRuntimeGrant
}

// ContextWithRuntimeManifest binds anonymous registration to immutable compiler output.
func ContextWithRuntimeManifest(ctx context.Context, manifest []byte) context.Context {
	return context.WithValue(ctx, runtimeRegistrationKey{}, append([]byte(nil), manifest...))
}

// ContextWithRuntimeConsent is available only after the browser adapter has published the reviewed API app.
func ContextWithRuntimeConsent(ctx context.Context, grant OAuthRuntimeGrant) context.Context {
	return context.WithValue(ctx, runtimeConsentKey{}, grant)
}

// ContextWithRuntimeExchange keeps the independent runtime secret out of OAuth access-token state.
func ContextWithRuntimeExchange(ctx context.Context, exchange *RuntimeExchange) context.Context {
	return context.WithValue(ctx, runtimeExchangeKey{}, exchange)
}

// OAuthRuntimeOnly rejects mixed management/runtime grants at every protocol boundary.
func OAuthRuntimeOnly(scopes []string) bool {
	return len(scopes) == 1 && (scopes[0] == OAuthRuntimeScope || scopes[0] == OAuthDeploymentScope)
}

// RuntimeManifestDigest matches the compiler's digest of the exact submitted bytes.
func RuntimeManifestDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// runtimeRegistrationManifest refuses a runtime client without a bound, bounded declaration.
func runtimeRegistrationManifest(ctx context.Context, scopes []string) ([]byte, error) {
	raw, _ := ctx.Value(runtimeRegistrationKey{}).([]byte)
	// Normal clients cannot attach executable provisioning metadata.
	if !OAuthRuntimeOnly(scopes) {
		// Metadata on another client type would cross the runtime-management boundary.
		if len(raw) != 0 {
			return nil, ErrInvalidOAuthClient
		}
		return nil, nil
	}
	// Semantic validation is performed by the runtime registration adapter before storage.
	if len(raw) == 0 || len(raw) > 64<<10 || !json.Valid(raw) {
		return nil, ErrInvalidOAuthClient
	}
	return raw, nil
}

// OAuthRuntimeManifestStore exposes public compiler requirements, never runtime secrets.
type OAuthRuntimeManifestStore interface {
	OAuthRuntimeManifest(context.Context, string) ([]byte, error)
}

// OAuthRuntimeRevoker resolves a client-bound credential before using canonical app-token revocation.
type OAuthRuntimeRevoker interface {
	RevokeOAuthRuntimeToken(context.Context, string, string) error
}

// RevokeOAuthRuntimeToken disconnects only this authenticated client; token hashes identify revocation targets, never authenticate API requests.
func (s *postgresStore) RevokeOAuthRuntimeToken(ctx context.Context, client, hash string) error {
	tx, err := s.db.Begin(ctx)
	// Client-bound disconnection uses runtime proof rather than minting a management credential.
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var clientID, subjectID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT client.id,history.issued_by_subject_id FROM fused_app_tokens token JOIN fused_oauth_clients client ON client.id=token.runtime_client_id JOIN fused_app_token_history history ON history.id=token.id WHERE client.client_id=$1 AND token.token_hash=$2
 UNION ALL SELECT client.id,token.subject_id FROM fused_oauth_tokens token JOIN fused_oauth_clients client ON client.id=token.client_id
 WHERE client.client_id=$1 AND token.scope=ARRAY['app.api.deploy']::text[] AND (token.access_token_hash=$2 OR token.refresh_token_hash=$2) LIMIT 1`, client, hash).Scan(&clientID, &subjectID)
	// RFC 7009 makes unknown and already-revoked tokens indistinguishable from success.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	// A failed lookup cannot broaden revocation to any other token.
	if err != nil {
		return err
	}
	// Reuse Connected Apps disconnection to revoke siblings and pending codes and release the consent quota slot.
	_, err = revokeOAuthConnectedAppTx(ctx, tx, clientID, subjectID, MutationActor{SubjectID: subjectID})
	// Concurrent disconnection remains idempotent through the canonical lifecycle.
	if errors.Is(err, ErrOAuthConsentNotFound) {
		return nil
	}
	// Any failed lifecycle mutation must leave the connection intact for a safe retry.
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeOAuthRuntimeToken bypasses caches and reuses the authoritative token lifecycle.
func (s *cachedStore) RevokeOAuthRuntimeToken(ctx context.Context, client, hash string) error {
	repository, ok := s.Store.(OAuthRuntimeRevoker)
	// Missing persistence support must not report successful credential revocation.
	if !ok {
		return errors.New("runtime revocation unavailable")
	}
	return repository.RevokeOAuthRuntimeToken(ctx, client, hash)
}

// OAuthRuntimeManifest reads only live runtime clients and preserves the registered bytes for hashing.
func (s *postgresStore) OAuthRuntimeManifest(ctx context.Context, client string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT runtime_manifest FROM fused_oauth_clients WHERE client_id=$1 AND (allowed_scopes=ARRAY['api.execute']::text[] OR allowed_scopes=ARRAY['app.api.deploy']::text[]) AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>NOW())`, client).Scan(&raw)
	return raw, err
}

// OAuthRuntimeManifest deliberately bypasses metadata caches after client revocation.
func (s *cachedStore) OAuthRuntimeManifest(ctx context.Context, client string) ([]byte, error) {
	repository, ok := s.Store.(OAuthRuntimeManifestStore)
	// Missing backing storage cannot fabricate a consent definition.
	if !ok {
		return nil, errors.New("runtime consent storage unavailable")
	}
	return repository.OAuthRuntimeManifest(ctx, client)
}

// validRuntimeGrant requires one concrete version and its explicitly approved default bucket.
func validRuntimeGrant(grant OAuthRuntimeGrant) bool {
	return grant.AppID != uuid.Nil && grant.AppFamilyID != uuid.Nil && grant.DefaultBucketID != uuid.Nil
}

// runtimeConsentGrant checks compiler binding inside the same transaction that issues the code.
func runtimeConsentGrant(ctx context.Context, tx pgx.Tx, code OAuthAuthorizationCodeIssue) ([]byte, error) {
	grant, present := ctx.Value(runtimeConsentKey{}).(OAuthRuntimeGrant)
	// Management and MCP consent cannot inherit a runtime grant.
	if !OAuthRuntimeOnly(code.Scope) {
		// A runtime grant is valid only for the specialized consent issuer.
		if present {
			return nil, ErrOAuthGrantDenied
		}
		return nil, nil
	}
	// Only a concrete published version may receive a runtime credential.
	if !present || !validRuntimeGrant(grant) {
		return nil, ErrOAuthGrantDenied
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT runtime_manifest FROM fused_oauth_clients WHERE id=$1 FOR UPDATE`, code.ClientID).Scan(&raw)
	// Exact bytes, rather than a browser-supplied digest, bind approval to the compiler request.
	if err != nil || RuntimeManifestDigest(raw) != grant.ManifestDigest {
		return nil, ErrOAuthGrantDenied
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_apps app JOIN fused_app_families family USING(app_family_id) WHERE app.app_id=$1 AND family.app_family_id=$2 AND family.kind='sdk' AND family.delivery_mode='api' AND family.archived_at IS NULL AND app.status IN ('active','deprecated'))`, grant.AppID, grant.AppFamilyID).Scan(&valid)
	// An unavailable or differently typed app cannot inherit the approved identity.
	if err != nil || !valid {
		return nil, ErrOAuthGrantDenied
	}
	return json.Marshal(grant)
}

// issueRuntimeAppToken commits code consumption and its independent, version-pinned execution credential together.
func issueRuntimeAppToken(ctx context.Context, tx pgx.Tx, code lockedOAuthCode, oauthTokenID uuid.UUID) error {
	exchange, _ := ctx.Value(runtimeExchangeKey{}).(*RuntimeExchange)
	// Other issuers must never exchange a runtime code, even with otherwise valid client credentials.
	if !OAuthRuntimeOnly(code.scope) {
		// Exchange metadata cannot be attached to management or MCP codes.
		if exchange != nil || len(code.runtimeGrant) != 0 {
			return ErrOAuthGrantDenied
		}
		return nil
	}
	grant, err := runtimeExchangeGrant(exchange, code.runtimeGrant)
	// A missing private exchange context cannot create a management token from runtime consent.
	if err != nil {
		return err
	}
	// Lock consent while issuing the token so a concurrent disconnect cannot leave revivable credentials.
	if err := lockRuntimeConsent(ctx, tx, code); err != nil {
		return err
	}
	// Retain the exact deployment ceiling on the OAuth token, independently of execution credentials.
	if len(code.scope) == 1 && code.scope[0] == OAuthDeploymentScope {
		if _, err := tx.Exec(ctx, `UPDATE fused_oauth_tokens SET runtime_grant=$2 WHERE id=$1`, oauthTokenID, code.runtimeGrant); err != nil {
			return err
		}
	}
	id := uuid.New()
	_, err = createAppTokenTx(ctx, tx, AppTokenIssue{ID: id, AppFamilyID: grant.AppFamilyID, Name: "Runtime " + id.String(), TokenHash: exchange.TokenHash, IssuedBySubjectID: &code.subjectID, Policy: AppTokenPolicy{AllowAll: true}, BindingMode: AppTokenBindingDynamic})
	// No partial credential is returned if token history or audit persistence fails.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_app_tokens SET runtime_app_id=$2, runtime_client_id=$3, runtime_default_bucket_id=$4 WHERE id=$1`, id, grant.AppID, code.clientID, grant.DefaultBucketID)
	// The response receives public identity only after the transaction has a complete token record.
	if err == nil {
		exchange.Grant = grant
	}
	return err
}

// runtimeExchangeGrant validates the persisted snapshot and independent credential before any token mutation.
func runtimeExchangeGrant(exchange *RuntimeExchange, raw []byte) (OAuthRuntimeGrant, error) {
	var grant OAuthRuntimeGrant
	// The protocol adapter must supply a fresh hash; malformed stored grants never become wildcard credentials.
	if exchange == nil || len(exchange.TokenHash) != 64 || json.Unmarshal(raw, &grant) != nil || !validRuntimeGrant(grant) {
		return grant, ErrOAuthGrantDenied
	}
	return grant, nil
}

// lockRuntimeConsent serializes independent credential issuance against Connected Apps disconnection.
func lockRuntimeConsent(ctx context.Context, tx pgx.Tx, code lockedOAuthCode) error {
	var consent bool
	// A disconnected client cannot exchange a code issued before revocation.
	if err := tx.QueryRow(ctx, `SELECT (granted_scope=ARRAY['api.execute']::text[] OR granted_scope=ARRAY['app.api.deploy']::text[]) FROM fused_oauth_user_consents WHERE client_id=$1 AND subject_id=$2 FOR UPDATE`, code.clientID, code.subjectID).Scan(&consent); err != nil || !consent {
		return ErrOAuthGrantDenied
	}
	return nil
}

// revokeRuntimeConsentTokens uses the existing token-history and audit lifecycle for every affected credential.
func revokeRuntimeConsentTokens(ctx context.Context, tx pgx.Tx, client, subject uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT token.app_family_id,token.name FROM fused_app_tokens token JOIN fused_app_token_history history ON history.id=token.id WHERE token.runtime_client_id=$1 AND history.issued_by_subject_id=$2`, client, subject)
	// Failed lookup must roll back consent revocation rather than leaving restorable credentials.
	if err != nil {
		return err
	}
	type target struct {
		family uuid.UUID
		name   string
	}
	var targets []target
	// Drain the query before issuing token lifecycle mutations on the same transaction.
	for rows.Next() {
		var value target
		// Scan failures cannot silently revoke only part of the connection's authority.
		if err := rows.Scan(&value.family, &value.name); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, value)
	}
	rows.Close()
	// Iteration errors prevent any partial successful report.
	if err := rows.Err(); err != nil {
		return err
	}
	// Retire every matching credential through the existing history and audit path.
	for _, value := range targets {
		_, err := revokeAppTokenTx(ctx, tx, value.family, value.name)
		// Concurrent explicit revocation is already the desired terminal state.
		if err != nil && !errors.Is(err, ErrAppTokenNotFound) {
			return err
		}
	}
	// Pending codes must not become usable after a later reconnection by the same client.
	_, err = tx.Exec(ctx, `UPDATE fused_oauth_authorization_codes SET consumed_at=NOW() WHERE client_id=$1 AND subject_id=$2 AND (scope=ARRAY['api.execute']::text[] OR scope=ARRAY['app.api.deploy']::text[]) AND consumed_at IS NULL`, client, subject)
	return err
}

// runtimeTokenAuthoritySQL pins the version and makes Connected Apps revocation effective on the next authorization.
const runtimeTokenAuthoritySQL = `t.runtime_app_id=a.app_id AND f.kind='sdk' AND f.delivery_mode='api'
 AND EXISTS (SELECT 1 FROM fused_app_family_buckets bucket WHERE bucket.app_family_id=f.app_family_id AND bucket.service_id IS NULL AND bucket.bucket_id=t.runtime_default_bucket_id)
 AND NOT EXISTS (SELECT 1 FROM fused_app_family_buckets bucket WHERE bucket.app_family_id=f.app_family_id AND bucket.bucket_id<>t.runtime_default_bucket_id)
 AND EXISTS (SELECT 1 FROM fused_oauth_clients client JOIN fused_oauth_user_consents consent ON consent.client_id=client.id
 JOIN fused_subjects subject ON subject.id=consent.subject_id AND subject.status='active'
 WHERE client.id=t.runtime_client_id AND client.revoked_at IS NULL
 AND (client.expires_at IS NULL OR client.expires_at>NOW())
 AND consent.subject_id=(SELECT issued_by_subject_id FROM fused_app_token_history history WHERE history.id=t.id) AND (consent.granted_scope=ARRAY['api.execute']::text[] OR consent.granted_scope=ARRAY['app.api.deploy']::text[]))`
