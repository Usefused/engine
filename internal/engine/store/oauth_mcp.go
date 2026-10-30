package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OAuthMCPScope grants only the separately consented runtime policy, never control access.
const OAuthMCPScope = "mcp.execute"

// limitOAuthRegistrations bounds unconsented identities across replicas, not merely per-process HTTP bursts.
func limitOAuthRegistrations(ctx context.Context, tx pgx.Tx) error {
	// A table lock serializes count-and-insert without introducing another quota state store.
	if _, err := tx.Exec(ctx, `LOCK TABLE fused_oauth_clients IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var count int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM fused_oauth_clients WHERE is_dynamic AND expires_at IS NOT NULL AND expires_at>NOW() AND revoked_at IS NULL LIMIT 1000) pending`).Scan(&count)
	// Storage failure or exhausted provisional capacity must fail before creating credentials.
	if err != nil {
		return err
	}
	// Bound provisional client storage even across concurrent Engine replicas.
	if count >= 1000 {
		return ErrInvalidOAuthClient
	}
	return nil
}

// oauthMCPRuntimeAuthoritySQL is evaluated inside the canonical exact-version token query.
// Delegation requires the same live use and token-management rights as consent.
// A stable resource follows its family; a version resource stays pinned even on alternate execution adapters.
const oauthMCPRuntimeAuthoritySQL = `
 oauth.scope=ARRAY['mcp.execute']::text[] AND oauth.revoked_at IS NULL
 AND oauth.access_expires_at>NOW() AND f.kind='mcp'
 AND oauth.mcp_grant->>'app_family_id'=f.app_family_id::text
 AND RIGHT(oauth.mcp_grant->>'resource',36) IN (f.app_family_id::text,a.app_id::text)
 AND EXISTS (SELECT 1 FROM fused_oauth_clients client WHERE client.id=oauth.client_id
   AND client.revoked_at IS NULL AND (client.expires_at IS NULL OR client.expires_at>NOW()))
 AND EXISTS (SELECT 1 FROM fused_subjects subject WHERE subject.id=oauth.subject_id AND subject.status='active')
 AND (SELECT COUNT(DISTINCT permission.permission)
   FROM fused_role_bindings binding
   JOIN fused_roles role ON role.id=binding.role_id AND role.scope_type=binding.resource_type
   JOIN fused_role_permissions permission ON permission.role_id=role.id
   JOIN fused_workspaces workspace ON workspace.singleton_key=1
   WHERE permission.permission IN ('app.mcp.use','app.mcp.tokens.manage')
   AND ((binding.resource_type='workspace' AND binding.resource_id=workspace.id)
     OR (binding.resource_type='app' AND binding.resource_id=f.app_family_id))
   AND ((binding.subject_type='subject' AND binding.subject_id=oauth.subject_id)
     OR (binding.subject_type='workspace' AND binding.subject_id=workspace.id)
     OR (binding.subject_type='team' AND EXISTS (
       SELECT 1 FROM fused_team_memberships member JOIN fused_teams team ON team.id=member.team_id
       WHERE member.member_subject_id=oauth.subject_id AND member.team_id=binding.subject_id AND team.status='active')))
 )=2`

// OAuthMCPGrant is an immutable consent snapshot copied through code exchange and refresh.
// Provider credentials never enter this record; connection identities are revalidated by the token store.
type OAuthMCPGrant struct {
	Resource    string                      `json:"resource"`
	AppFamilyID uuid.UUID                   `json:"app_family_id"`
	AppID       uuid.UUID                   `json:"app_id"`
	Operations  []string                    `json:"operations"`
	Connections []OAuthMCPConnectionBinding `json:"connections,omitempty"`
}

// OAuthMCPConnectionBinding pins the account and optional resource selected at consent.
type OAuthMCPConnectionBinding struct {
	ServiceID    uuid.UUID  `json:"service_id"`
	AuthName     string     `json:"auth_name"`
	ConnectionID uuid.UUID  `json:"connection_id"`
	ResourceID   *uuid.UUID `json:"resource_id"`
}

type oauthMCPContextKey struct{}
type oauthMCPContext struct {
	Resource string
	Grant    *OAuthMCPGrant
}

// ContextWithOAuthMCP keeps audience and consent data on the trusted adapter boundary.
func ContextWithOAuthMCP(ctx context.Context, resource string, grant *OAuthMCPGrant) context.Context {
	return context.WithValue(ctx, oauthMCPContextKey{}, oauthMCPContext{resource, grant})
}

// OAuthMCPOnly prevents a runtime client or grant from acquiring management permissions.
func OAuthMCPOnly(scopes []string) bool { return len(scopes) == 1 && scopes[0] == OAuthMCPScope }

// oauthMCPCodeGrant requires a concrete action selection before issuing a runtime code.
func oauthMCPCodeGrant(ctx context.Context, scopes []string) ([]byte, error) {
	request, _ := ctx.Value(oauthMCPContextKey{}).(oauthMCPContext)
	// Ordinary management grants cannot inherit runtime metadata from a crafted request.
	if !OAuthMCPOnly(scopes) {
		// A management grant cannot be issued inside a runtime audience context.
		if request.Resource != "" {
			return nil, ErrOAuthGrantDenied
		}
		return nil, nil
	}
	grant := request.Grant
	// Empty action sets are never interpreted as wildcard access.
	if grant == nil || grant.Resource != request.Resource || grant.Resource == "" || grant.AppID == uuid.Nil || grant.AppFamilyID == uuid.Nil || len(grant.Operations) == 0 || len(grant.Operations) > 1000 {
		return nil, ErrOAuthGrantDenied
	}
	return json.Marshal(grant)
}

// validateOAuthMCPExchange binds both code and refresh grants to the original resource.
func validateOAuthMCPExchange(ctx context.Context, scopes []string, raw []byte) error {
	request, _ := ctx.Value(oauthMCPContextKey{}).(oauthMCPContext)
	// Management tokens cannot be exchanged through the MCP issuer, or vice versa.
	if !OAuthMCPOnly(scopes) {
		// Mixed issuer context or persisted runtime metadata is a cross-boundary error.
		if request.Resource != "" || len(raw) != 0 {
			return ErrOAuthGrantDenied
		}
		return nil
	}
	var grant OAuthMCPGrant
	// Missing metadata, another audience, or an empty policy fails before token consumption.
	if json.Unmarshal(raw, &grant) != nil || grant.Resource == "" || request.Resource != grant.Resource || len(grant.Operations) == 0 {
		return ErrOAuthGrantDenied
	}
	return nil
}

// issueOAuthMCPAppToken projects OAuth consent into the existing runtime enforcement path atomically.
func issueOAuthMCPAppToken(ctx context.Context, tx pgx.Tx, metadata OAuthTokenMetadata, issue OAuthTokenIssue, raw []byte) error {
	// Management grants never create executable credentials.
	if !OAuthMCPOnly(metadata.Scope) {
		return nil
	}
	var grant OAuthMCPGrant
	// Corrupt persisted consent must roll back the entire token exchange.
	if err := json.Unmarshal(raw, &grant); err != nil {
		return ErrOAuthGrantDenied
	}
	// Even a static-only grant is fixed: a later auth alternative must not accept caller-selected users.
	mode := AppTokenBindingFixed
	_, err := createAppTokenTx(ctx, tx, AppTokenIssue{
		ID: metadata.ID, AppFamilyID: grant.AppFamilyID, TokenHash: issue.AccessTokenHash,
		Name: "OAuth " + metadata.ID.String(), IssuedBySubjectID: &metadata.SubjectID,
		Policy:      AppTokenPolicy{AllowedOperations: slices.Clone(grant.Operations), ExpiresAt: &metadata.AccessExpiresAt},
		BindingMode: mode,
	})
	// Token persistence and the OAuth metadata must either both commit or both roll back.
	if err != nil {
		return err
	}
	// Refresh restores opaque connection identities rather than resolving a possibly reassigned user reference.
	if err := restoreOAuthMCPConnections(ctx, tx, metadata.ID, grant.Connections); err != nil {
		return err
	}
	raw, err = json.Marshal(grant)
	// Failed snapshot serialization must roll back both token representations.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_oauth_tokens SET mcp_grant=$2 WHERE id=$1`, metadata.ID, raw)
	// The foreign key prevents cleanup from leaving an independent executable token.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_app_tokens SET oauth_token_id=$1 WHERE id=$1`, metadata.ID)
	return err
}

// restoreOAuthMCPConnections validates the entire persisted binding set before attaching it to the rotated token.
func restoreOAuthMCPConnections(ctx context.Context, tx pgx.Tx, tokenID uuid.UUID, connections []OAuthMCPConnectionBinding) error {
	// Static-only grants deliberately have no connected-account authority.
	if len(connections) == 0 {
		return nil
	}
	raw, err := json.Marshal(connections)
	// Encoding errors must not turn a connected grant into a static-only grant.
	if err != nil {
		return err
	}
	var count int
	err = tx.QueryRow(ctx, `WITH restored AS (
	 INSERT INTO fused_app_token_bindings(token_id,service_id,auth_name,auth_connection_id,resource_id)
	 SELECT $1,b.service_id,b.auth_name,b.connection_id,b.resource_id
	 FROM jsonb_to_recordset($2::jsonb) b(service_id uuid,auth_name text,connection_id uuid,resource_id uuid)
	 JOIN fused_auth_connections connection ON connection.id=b.connection_id AND connection.service_id=b.service_id AND connection.auth_name=b.auth_name
	 JOIN fused_app_tokens token ON token.id=$1
	 WHERE connection.bucket_id=(SELECT bucket_id FROM fused_app_family_buckets bucket WHERE bucket.app_family_id=token.app_family_id AND (bucket.service_id=b.service_id OR bucket.service_id IS NULL) ORDER BY bucket.service_id NULLS LAST LIMIT 1)
	 AND (b.resource_id IS NULL OR EXISTS (SELECT 1 FROM fused_connection_resources resource WHERE resource.id=b.resource_id AND resource.connection_id=b.connection_id AND resource.is_active))
	 RETURNING 1) SELECT COUNT(*) FROM restored`, tokenID, raw).Scan(&count)
	// Any removed connection or resource invalidates the refresh atomically.
	if err != nil {
		return err
	}
	// A missing connection must roll back the entire binding set rather than partially authorize it.
	if count != len(connections) {
		return ErrAppTokenBindingInvalid
	}
	return nil
}

// OAuthMCPResource reads only a live grant's audience for transport admission.
func (s *postgresStore) OAuthMCPResource(ctx context.Context, hash string) (string, error) {
	var resource string
	err := s.db.QueryRow(ctx, `SELECT t.mcp_grant->>'resource' FROM fused_oauth_tokens t
		JOIN fused_oauth_clients c ON c.id=t.client_id AND c.revoked_at IS NULL
		WHERE t.access_token_hash=$1 AND t.scope=ARRAY['mcp.execute']::text[]
		AND t.revoked_at IS NULL AND t.access_expires_at>NOW()
		AND (c.expires_at IS NULL OR c.expires_at>NOW())`, hash).Scan(&resource)
	return resource, err
}

// OAuthMCPResource bypasses caches so disconnect is effective at the next request.
func (s *cachedStore) OAuthMCPResource(ctx context.Context, hash string) (string, error) {
	repository, ok := s.Store.(interface {
		OAuthMCPResource(context.Context, string) (string, error)
	})
	// Missing support must never admit a management or unbound token.
	if !ok {
		return "", errors.New("MCP OAuth storage unavailable")
	}
	return repository.OAuthMCPResource(ctx, hash)
}

// revokeOAuthForAppToken prevents a manual token revocation from being undone by the client's next refresh.
func revokeOAuthForAppToken(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, name string) error {
	var grantID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT oauth.token_family_id FROM fused_app_tokens token
	 JOIN fused_oauth_tokens oauth ON oauth.id=token.oauth_token_id
	 WHERE token.app_family_id=$1 AND token.name=$2 FOR UPDATE OF oauth`, familyID, name).Scan(&grantID)
	// Manual credentials have no OAuth grant and keep the existing revocation path.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	// Lookup failure must not report successful revocation of a renewable credential.
	if err != nil {
		return err
	}
	return revokeOAuthTokenFamily(ctx, tx, grantID, "oauth.token.revoke")
}
