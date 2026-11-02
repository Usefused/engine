package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"strings"
)

// OAuthDeploymentStore exposes a deployment capability without admitting it to management APIs.
type OAuthDeploymentStore interface {
	LoadOAuthDeployment(context.Context, string) (accesscontrol.Actor, OAuthRuntimeGrant, []byte, error)
	IssueOAuthDeploymentToken(context.Context, string, OAuthRuntimeGrant, string) error
}

// LoadOAuthDeployment rechecks live consent and user permissions on each deployment without a cache.
func (s *postgresStore) LoadOAuthDeployment(ctx context.Context, hash string) (accesscontrol.Actor, OAuthRuntimeGrant, []byte, error) {
	var grant OAuthRuntimeGrant
	var raw, manifest []byte
	err := s.db.QueryRow(ctx, `SELECT token.runtime_grant, client.runtime_manifest
 FROM fused_oauth_tokens token JOIN fused_oauth_clients client ON client.id=token.client_id
 JOIN fused_oauth_user_consents consent ON consent.client_id=token.client_id AND consent.subject_id=token.subject_id
 WHERE token.access_token_hash=$1 AND token.scope=ARRAY['app.api.deploy']::text[]
 AND consent.granted_scope=token.scope AND client.allowed_scopes=token.scope
 AND token.revoked_at IS NULL AND token.access_expires_at>NOW() AND client.revoked_at IS NULL
 AND (client.expires_at IS NULL OR client.expires_at>NOW())`, hash).Scan(&raw, &manifest)
	// Missing or malformed ceilings must never become workspace-wide actors.
	if err != nil || json.Unmarshal(raw, &grant) != nil || !validRuntimeGrant(grant) || RuntimeManifestDigest(manifest) != grant.ManifestDigest {
		return accesscontrol.Actor{}, grant, nil, accesscontrol.ErrAuthenticationRequired
	}
	principal, err := s.loadOAuthPrincipal(ctx, hash, true)
	// This private loader cannot be entered through ordinary control authentication.
	if err != nil {
		return accesscontrol.Actor{}, grant, nil, err
	}
	actor, err := deploymentActor(ctx, principal, grant.AppFamilyID)
	return actor, grant, manifest, err
}

// deploymentActor narrows live app authority to one family and removes all app-creation permissions.
func deploymentActor(ctx context.Context, principal accesscontrol.ControlPrincipal, family uuid.UUID) (accesscontrol.Actor, error) {
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(principal.Revision, principal.EffectiveGrants...)
	// Invalid live grants cannot bypass the normal authorizer.
	if err != nil {
		return accesscontrol.Actor{}, err
	}
	actor := accesscontrol.Actor{AccountID: principal.AccountID, WorkspaceID: principal.WorkspaceID, SubjectID: principal.SubjectID, CredentialID: principal.CredentialID, CredentialSource: "oauth_deployment", AuthenticationMethod: "oauth2", Kind: principal.Kind, Authorization: snapshot, AppPermissions: principal.AppPermissions}
	grants := []accesscontrol.Grant{}
	// Service/bucket authority is additionally fenced by the handler's immutable selection ceiling.
	for _, grant := range principal.EffectiveGrants {
		// Broad, cross-kind and creation app grants cannot survive this family ceiling.
		if !strings.HasPrefix(string(grant.Permission), "app.") {
			grants = append(grants, grant)
		}
	}
	for _, permission := range []accesscontrol.Permission{accesscontrol.PermissionAppAPIRead, accesscontrol.PermissionAppAPIManage, accesscontrol.PermissionAppAPITokensManage} {
		requirement := accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: family}}
		// Losing any required authority prevents subsequent publication or token issuance.
		if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, requirement); err != nil {
			return accesscontrol.Actor{}, err
		}
		grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: requirement.Resource})
	}
	actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(principal.Revision, grants...)
	return actor, err
}

// IssueOAuthDeploymentToken serializes issuance against revocation and preserves prior release tokens.
func (s *postgresStore) IssueOAuthDeploymentToken(ctx context.Context, hash string, grant OAuthRuntimeGrant, tokenHash string) error {
	tx, err := s.db.Begin(ctx)
	// No token mutation precedes the transaction.
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var client, subject uuid.UUID
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT token.client_id,token.subject_id,token.runtime_grant FROM fused_oauth_tokens token
 JOIN fused_oauth_clients client ON client.id=token.client_id JOIN fused_oauth_user_consents consent ON consent.client_id=token.client_id AND consent.subject_id=token.subject_id
 JOIN fused_subjects subject ON subject.id=token.subject_id AND subject.status='active'
 WHERE token.access_token_hash=$1 AND token.scope=ARRAY['app.api.deploy']::text[] AND consent.granted_scope=token.scope
 AND token.revoked_at IS NULL AND token.access_expires_at>NOW() AND client.revoked_at IS NULL
 AND (client.expires_at IS NULL OR client.expires_at>NOW()) FOR UPDATE OF token,consent,client`, hash).Scan(&client, &subject, &raw)
	var ceiling OAuthRuntimeGrant
	// Other families and unapproved bucket identities require a separate consent ceremony.
	if err != nil || json.Unmarshal(raw, &ceiling) != nil || !deploymentGrantWithin(grant, ceiling) || len(tokenHash) != 64 {
		return ErrOAuthGrantDenied
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_apps app JOIN fused_app_families family USING(app_family_id) WHERE app.app_id=$1 AND app.app_family_id=$2 AND family.archived_at IS NULL AND family.kind='sdk' AND family.delivery_mode='api' AND app.status IN ('active','deprecated'))`, grant.AppID, grant.AppFamilyID).Scan(&valid)
	// The published version must still belong to the approved family at credential commit.
	if err != nil || !valid {
		return ErrOAuthGrantDenied
	}
	id := uuid.New()
	_, err = createAppTokenTx(ctx, tx, AppTokenIssue{ID: id, AppFamilyID: grant.AppFamilyID, Name: "Runtime " + id.String(), TokenHash: tokenHash, IssuedBySubjectID: &subject, Policy: AppTokenPolicy{AllowAll: true}, BindingMode: AppTokenBindingDynamic})
	// History and audit must commit together with the pinned runtime identity.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_app_tokens SET runtime_app_id=$2,runtime_client_id=$3,runtime_default_bucket_id=$4 WHERE id=$1`, id, grant.AppID, client, grant.DefaultBucketID)
	// Failed binding cannot leave an unconstrained execution credential behind.
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// deploymentGrantWithin keeps approved credential routing identical across releases.
func deploymentGrantWithin(next, ceiling OAuthRuntimeGrant) bool {
	// Missing identity or a changed default bucket requires new consent.
	if !validRuntimeGrant(next) || next.AppFamilyID != ceiling.AppFamilyID || next.DefaultBucketID != ceiling.DefaultBucketID || len(next.Buckets) != len(ceiling.Buckets) {
		return false
	}
	allowed := map[uuid.UUID]bool{}
	for _, bucket := range ceiling.Buckets {
		allowed[bucket.ID] = true
	}
	for _, bucket := range next.Buckets {
		// Consuming identities rejects duplicates even when list lengths match.
		if !allowed[bucket.ID] {
			return false
		}
		delete(allowed, bucket.ID)
	}
	return len(allowed) == 0
}

// LoadOAuthDeployment bypasses caches so revoked grants lose deployment authority immediately.
func (s *cachedStore) LoadOAuthDeployment(ctx context.Context, hash string) (accesscontrol.Actor, OAuthRuntimeGrant, []byte, error) {
	repository, ok := s.Store.(OAuthDeploymentStore)
	// Older backing stores cannot simulate scoped deployment support.
	if !ok {
		return accesscontrol.Actor{}, OAuthRuntimeGrant{}, nil, errors.New("deployment grants unavailable")
	}
	return repository.LoadOAuthDeployment(ctx, hash)
}

// IssueOAuthDeploymentToken delegates atomic issuance to the authoritative store.
func (s *cachedStore) IssueOAuthDeploymentToken(ctx context.Context, hash string, grant OAuthRuntimeGrant, tokenHash string) error {
	repository, ok := s.Store.(OAuthDeploymentStore)
	// Unsupported stores must not report successful issuance.
	if !ok {
		return errors.New("deployment grants unavailable")
	}
	return repository.IssueOAuthDeploymentToken(ctx, hash, grant, tokenHash)
}
