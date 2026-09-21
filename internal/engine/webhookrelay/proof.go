package webhookrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/shared/managedpublication"

	"github.com/Usefused/engine/internal/shared/signaturepolicy"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProofStore binds a provider response to the authenticated remote installation without retaining provider tokens.
type ProofStore struct{ DB *pgxpool.Pool }

// RecordExchange stores only claims extracted from the broker's own successful provider token response.
func (s ProofStore) RecordExchange(ctx context.Context, installation, service uuid.UUID, authName, token, refreshToken string, raw []byte, applicationIDs ...string) error {
	applicationID, err := managedpublication.Selector(applicationIDs)
	// Invalid selectors cannot create a proof for the default publication.
	if err != nil {
		return ErrDenied
	}
	rows, err := s.DB.Query(ctx, `SELECT w.id,w.relay_config,w.signature_policy,`+policyFingerprintSQL+`
 FROM fused_workspace_webhooks w JOIN fused_oauth_publications p ON `+managedpublication.WebhookPublicationSQL+`
 JOIN fused_managed_auth_installations i ON i.id=$4
 WHERE w.service_id=$1 AND p.auth_name=$2 AND w.signature_policy IS NOT NULL
 AND w.secret_bucket_id IS NOT NULL AND p.service_version_id=w.service_version_id AND p.application_id=$3
 AND i.revoked_at IS NULL AND i.access_expires_at>NOW() AND `+managedpublication.AudienceSQL, service, authName, applicationID, installation)
	// A failed proof lookup must fail the delegated exchange rather than issuing unbound claims.
	if err != nil {
		return err
	}
	defer rows.Close()
	grants := []proofRow{}
	// The result is bounded by the existing registrations for this exact service and scheme.
	for rows.Next() {
		var row proofRow
		var config, verification []byte
		// No partial grant is persisted when any configured registration cannot establish proof.
		if err = rows.Scan(&row.RegistrationID, &config, &verification, &row.PolicyHash); err != nil {
			return err
		}
		var policy signaturepolicy.Config
		// Stored or manually edited registrations must satisfy the same verification floor as plan/apply.
		if json.Unmarshal(verification, &policy) != nil || ValidateExportVerification(&policy) != nil {
			return ErrDenied
		}
		row.ResourceID, row.AppID, err = providerClaims(config, raw)
		// No partial or unsupported provider proof may be persisted.
		if err != nil {
			return err
		}
		grants = append(grants, row)
	}
	// Query failures after iteration must not commit a partial set of grants.
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	encoded, err := json.Marshal(grants)
	// Serialization failure must precede the single set-based write.
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO fused_webhook_grants(installation_id,registration_id,token_hash,resource_id,provider_app_id,policy_hash,refresh_hash)
 SELECT $1,x.registration_id,$2,x.resource_id,x.app_id,x.policy_hash,$4 FROM jsonb_to_recordset($3::jsonb)
 AS x(registration_id uuid,resource_id text,app_id text,policy_hash text)
 ON CONFLICT(installation_id,registration_id,token_hash) DO NOTHING`, installation, Digest(token), encoded, Digest(refreshToken))
	return err
}

type proofRow struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	ResourceID     string    `json:"resource_id"`
	AppID          string    `json:"app_id"`
	PolicyHash     string    `json:"policy_hash"`
}

// policyFingerprintSQL invalidates grants when publication, signature verification, or routing policy changes.
const policyFingerprintSQL = `encode(sha256(convert_to(jsonb_build_array(w.relay_config,w.signature_policy,w.secret_bucket_id,w.secret_ref,w.slug,w.service_version_id,p.registration_hash)::text,'UTF8')),'hex')`

// RecordRefresh rotates the token proof only for grants this installation already established by code exchange.
func (s ProofStore) RecordRefresh(ctx context.Context, installation, service uuid.UUID, authName, oldToken, newToken, newRefresh string, applicationIDs ...string) error {
	applicationID, err := managedpublication.Selector(applicationIDs)
	// Refresh proof may move only within the exact original application.
	if err != nil {
		return ErrDenied
	}
	_, err = s.DB.Exec(ctx, `UPDATE fused_webhook_grants g SET token_hash=$1, refresh_hash=CASE WHEN $6='' THEN g.refresh_hash ELSE $6 END
 FROM fused_workspace_webhooks w JOIN fused_oauth_publications p ON `+managedpublication.WebhookPublicationSQL+`
 JOIN fused_managed_auth_installations i ON i.id=$2 WHERE g.registration_id=w.id AND g.installation_id=$2
 AND g.refresh_hash=$3 AND w.service_id=$4 AND w.relay_config->'publish'->>'auth_name'=$5 AND p.application_id=$7 AND g.policy_hash=`+policyFingerprintSQL+`
 AND i.revoked_at IS NULL AND i.access_expires_at>NOW() AND `+managedpublication.AudienceSQL, Digest(newToken), installation, Digest(oldToken), service, authName, optionalDigest(newRefresh), applicationID)
	return err
}

// Authorization is the broker-owned subscription projection; consumers never choose its resource, app, subject or start time.
type Authorization struct {
	ID             uuid.UUID
	GrantID        uuid.UUID
	RegistrationID uuid.UUID
	ReceiverID     uuid.UUID
	ServiceID      uuid.UUID
	VersionID      uuid.UUID
	AccountID      uuid.UUID
	Label          string
	ResourceID     string
	AppID          string
	Policy         Routing
	CreatedAt      time.Time
}

// Authorize rechecks installation expiry, grant ownership and current pinned policy on every pull and acknowledgement.
func (s ProofStore) Authorize(ctx context.Context, installation, subscription uuid.UUID) (Authorization, error) {
	var a Authorization
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT sub.id,g.id,w.id,sub.receiver_id,w.service_id,w.service_version_id,
 ws.account_id,w.label,g.resource_id,g.provider_app_id,w.relay_config,g.created_at
 FROM fused_webhook_subscriptions sub JOIN fused_webhook_grants g ON g.id=sub.grant_id
 JOIN fused_managed_auth_installations i ON i.id=g.installation_id
 JOIN fused_workspace_webhooks w ON w.id=g.registration_id CROSS JOIN fused_workspaces ws
 JOIN fused_oauth_publications p ON `+managedpublication.WebhookPublicationSQL+`
 WHERE sub.id=$1 AND g.installation_id=$2 AND sub.revoked_at IS NULL
 AND i.revoked_at IS NULL AND i.access_expires_at>NOW() AND `+managedpublication.AudienceSQL+` AND g.policy_hash=`+policyFingerprintSQL, subscription, installation).Scan(
		&a.ID, &a.GrantID, &a.RegistrationID, &a.ReceiverID, &a.ServiceID, &a.VersionID, &a.AccountID, &a.Label, &a.ResourceID, &a.AppID, &raw, &a.CreatedAt)
	// Absent and unauthorized subscriptions share one bounded failure.
	if err != nil {
		return a, fmt.Errorf("authorize webhook: %w", err)
	}
	c, err := Decode(raw)
	// A changed or unsupported routing policy cannot reuse a previously established grant.
	if err != nil || c.Publish == nil {
		return a, ErrDenied
	}
	a.Policy = *c.Publish
	return a, nil
}

// Subscribe admits an explicit receiver only when the supplied token hash matches this installation's broker-verified grant.
func (s ProofStore) Subscribe(ctx context.Context, installation, registration, receiver uuid.UUID, token string, applicationIDs ...string) (uuid.UUID, error) {
	applicationID, err := managedpublication.Selector(applicationIDs)
	// Invalid selectors cannot subscribe to the default publication.
	if err != nil {
		return uuid.Nil, ErrDenied
	}
	var id uuid.UUID
	// Empty identities must never acquire a durable receiver.
	if registration == uuid.Nil || receiver == uuid.Nil || token == "" {
		return id, ErrDenied
	}
	tx, err := s.DB.Begin(ctx)
	// Subscription quotas must be atomic across simultaneous requests from the same installation.
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(context.Background())
	// This installation-local advisory lock keeps count-and-create bounded without serializing other tenants.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,619038522))`, installation.String()); err != nil {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO fused_webhook_subscriptions(grant_id,receiver_id)
 SELECT g.id,$3 FROM fused_webhook_grants g JOIN fused_managed_auth_installations i ON i.id=g.installation_id
 JOIN fused_workspace_webhooks w ON w.id=g.registration_id
 JOIN fused_oauth_publications p ON `+managedpublication.WebhookPublicationSQL+`
 WHERE g.installation_id=$1 AND g.registration_id=$2 AND g.token_hash=$4 AND p.application_id=$5
 AND `+managedpublication.AudienceSQL+` AND g.policy_hash=`+policyFingerprintSQL+`
 AND i.revoked_at IS NULL AND i.access_expires_at>NOW()
 AND ((SELECT count(*) FROM fused_webhook_subscriptions sub JOIN fused_webhook_grants owned ON owned.id=sub.grant_id
 WHERE owned.installation_id=$1 AND sub.revoked_at IS NULL)<128
 OR EXISTS(SELECT 1 FROM fused_webhook_subscriptions sub WHERE sub.grant_id=g.id AND sub.receiver_id=$3))
 ON CONFLICT(grant_id,receiver_id) DO UPDATE SET receiver_id=EXCLUDED.receiver_id
 WHERE fused_webhook_subscriptions.revoked_at IS NULL RETURNING id`, installation, registration, receiver, Digest(token), applicationID).Scan(&id)
	// Do not reveal whether another installation owns the provider token or registration.
	if err != nil {
		return uuid.Nil, fmt.Errorf("subscribe webhook: %w", err)
	}
	// Commit the opaque receiver identity before loading its complete current policy projection.
	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	_, err = s.Authorize(ctx, installation, id)
	return id, err
}

// Revoke preserves the subscription tombstone so stale setup requests cannot resurrect withdrawn access.
func (s ProofStore) Revoke(ctx context.Context, installation, id uuid.UUID) error {
	_, err := s.DB.Exec(ctx, `UPDATE fused_webhook_subscriptions sub SET revoked_at=COALESCE(revoked_at,NOW())
 FROM fused_webhook_grants g WHERE sub.grant_id=g.id AND g.installation_id=$1 AND sub.id=$2`, installation, id)
	return err
}

// optionalDigest preserves a provider refresh token when its rotation response omits a replacement.
func optionalDigest(value string) string {
	// Omission means retain existing refresh proof, never authorize an empty token.
	if value == "" {
		return ""
	}
	return Digest(value)
}

// providerClaims extracts only the explicit resource and app from the successful broker-owned token response.
func providerClaims(config, raw []byte) (string, string, error) {
	c, err := Decode(config)
	// An omitted publish policy cannot authorize any provider resource.
	if err != nil || c.Publish == nil {
		return "", "", ErrDenied
	}
	resource, err := StringClaim(raw, c.Publish.TokenResourcePath)
	// A missing resource must not create a wildcard grant even if the app claim is present.
	if err != nil {
		return "", "", err
	}
	app, err := StringClaim(raw, c.Publish.TokenAppPath)
	return resource, app, err
}

// RevokeReceiver uses a durable local identity to settle setup requests whose acknowledgement was lost.
func (s ProofStore) RevokeReceiver(ctx context.Context, installation, receiver uuid.UUID) error {
	_, err := s.DB.Exec(ctx, `UPDATE fused_webhook_subscriptions sub SET revoked_at=COALESCE(revoked_at,NOW())
 FROM fused_webhook_grants g WHERE sub.grant_id=g.id AND g.installation_id=$1 AND sub.receiver_id=$2`, installation, receiver)
	return err
}
