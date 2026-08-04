package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestPostgresAccessControlBootstrapAndAuthentication(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "Access Test Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	bucketID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, 'payments-production')`, bucketID); err != nil {
		t.Fatalf("insert display-name bucket: %v", err)
	}
	displayNames, err := repository.ResolveAuthorizationResourceDisplayNames(ctx, []accesscontrol.ResourceRef{
		{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		{Type: accesscontrol.ResourceBucket, ID: bucketID},
	})
	workspaceRef := accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}
	bucketRef := accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}
	if err != nil || displayNames[workspaceRef] != "Access Test Workspace" || displayNames[bucketRef] != "payments-production" {
		t.Fatalf("authorization display names = %#v, %v", displayNames, err)
	}
	first, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_bootstrap_test")
	if err != nil {
		t.Fatalf("first BootstrapOwner: %v", err)
	}
	if !first.Changed || first.WorkspaceID != workspaceID || first.Revision != 2 {
		t.Fatalf("first bootstrap = %#v, want changed workspace with revision 2", first)
	}

	second, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_bootstrap_test")
	if err != nil {
		t.Fatalf("second BootstrapOwner: %v", err)
	}
	if second.Changed || second.SubjectID != first.SubjectID || second.CredentialID != first.CredentialID || second.Revision != first.Revision {
		t.Fatalf("second bootstrap = %#v, want unchanged IDs/revision from %#v", second, first)
	}

	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential("fsk_bootstrap_test"))
	if err != nil {
		t.Fatalf("LoadControlPrincipal: %v", err)
	}
	if principal.WorkspaceID != workspaceID || principal.SubjectID != first.SubjectID || principal.Kind != accesscontrol.SubjectBootstrap || principal.Revision != first.Revision {
		t.Fatalf("principal = %#v, want bootstrap subject at revision %d", principal, first.Revision)
	}
	revision, err := repository.LoadAuthorizationRevision(ctx)
	if err != nil || revision != first.Revision {
		t.Fatalf("LoadAuthorizationRevision = %d, %v; want %d, nil", revision, err, first.Revision)
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(principal.Revision, principal.EffectiveGrants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot: %v", err)
	}
	requirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionAccessManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	}
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, accesscontrol.Actor{Authorization: snapshot}, requirement); err != nil {
		t.Fatalf("bootstrap Owner access.manage: %v", err)
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_audit_events WHERE action = 'access.bootstrap_owner'`).Scan(&auditCount); err != nil {
		t.Fatalf("count bootstrap audits: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("bootstrap audit count = %d, want 1", auditCount)
	}

	event := accesscontrol.AuditEvent{
		ID:                uuid.New(),
		ActorSubjectID:    first.SubjectID,
		ActorCredentialID: first.CredentialID,
		Action:            "workspace.read",
		Permission:        accesscontrol.PermissionWorkspaceRead,
		Resource:          accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		Outcome:           accesscontrol.AuditAllowed,
		StatusCode:        200,
		Metadata:          map[string]any{"requirements": 1},
	}
	if err := repository.RecordAuthorizationAudit(ctx, event); err != nil {
		t.Fatalf("RecordAuthorizationAudit: %v", err)
	}
	if err := repository.RecordAuthorizationAudit(ctx, event); err != nil {
		t.Fatalf("idempotent RecordAuthorizationAudit retry: %v", err)
	}
	var outcome string
	var requirements int
	if err := pool.QueryRow(ctx, `
		SELECT outcome, (metadata->>'requirements')::integer
		FROM fused_audit_events WHERE action = 'workspace.read'
	`).Scan(&outcome, &requirements); err != nil {
		t.Fatalf("load authorization audit: %v", err)
	}
	if outcome != "allowed" || requirements != 1 {
		t.Fatalf("authorization audit = %q/%d", outcome, requirements)
	}
	denied := event
	denied.Action = "workspace.update"
	denied.Permission = accesscontrol.PermissionWorkspaceUpdate
	denied.Outcome = accesscontrol.AuditDenied
	denied.StatusCode = 403
	denied.ReasonCode = "permission_denied"
	denied.ID = uuid.New()
	denied.MissingRequirements = []accesscontrol.Requirement{{Permission: accesscontrol.PermissionWorkspaceUpdate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}}
	if err := repository.RecordAuthorizationAudit(ctx, denied); err != nil {
		t.Fatalf("RecordAuthorizationAudit denied: %v", err)
	}
	var missingRequirements []byte
	if err := pool.QueryRow(ctx, `SELECT outcome, missing_requirements FROM fused_audit_events WHERE action = 'workspace.update'`).Scan(&outcome, &missingRequirements); err != nil {
		t.Fatalf("load denied authorization audit: %v", err)
	}
	if outcome != "denied" {
		t.Fatalf("denied authorization audit outcome = %q", outcome)
	}
	decodedMissing, err := accesscontrol.UnmarshalRequiredPermissions(missingRequirements)
	if err != nil || len(decodedMissing) != 1 || decodedMissing[0].Permission != accesscontrol.PermissionWorkspaceUpdate {
		t.Fatalf("missing audit requirements = %#v, %v", decodedMissing, err)
	}
	event.Metadata = map[string]any{"api_key": "must-not-persist"}
	if err := repository.RecordAuthorizationAudit(ctx, event); !errors.Is(err, accesscontrol.ErrUnsafeAuditMetadata) {
		t.Fatalf("unsafe audit error = %v", err)
	}
}

func TestPostgresWorkspaceShareGrantsUsersAndOwningTeamsBoundedUse(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)

	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "Workspace share test")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "test-control-credential-workspace-share")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	actor := MutationActor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, RequestID: "workspace-share-integration", TraceID: "0123456789abcdef0123456789abcdef"}
	userResult, err := repository.CreateUser(ctx, CreateUserInput{Email: "staff@example.com", DisplayName: "Staff", Actor: actor})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	credential, err := repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: userResult.User.ID, Name: "test", Actor: actor})
	if err != nil {
		t.Fatalf("IssueUserControlCredential: %v", err)
	}
	teamResult, err := repository.CreateTeam(ctx, TeamMutation{Name: "Applications", Slug: "applications", Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: teamResult.Team.ID, UserID: userResult.User.ID, Role: MembershipRoleMember, Actor: actor}); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: teamResult.Team.ID, RoleSlug: accesscontrol.RoleBuilder,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor}); err != nil {
		t.Fatalf("AddTeamBinding(builder): %v", err)
	}
	bucket, err := repository.CreateBucket(ctx, "company", false)
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	requirements := []accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionArtifactCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucket.ID}},
	}
	before, err := repository.PreflightArtifactOwnership(ctx, ArtifactOwnershipPreflight{ActorSubjectID: userResult.User.ID, OwnerTeamID: teamResult.Team.ID, Requirements: requirements})
	if err != nil || before.Allowed {
		t.Fatalf("pre-share decision = %#v, %v; want denied", before, err)
	}

	grant, err := repository.GrantWorkspaceShare(ctx, WorkspaceShareMutation{Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucket.ID}, Actor: actor})
	if err != nil || !grant.Changed || grant.Share.RoleSlug != accesscontrol.RoleBucketUser {
		t.Fatalf("GrantWorkspaceShare = %#v, %v", grant, err)
	}
	idempotent, err := repository.GrantWorkspaceShare(ctx, WorkspaceShareMutation{Resource: grant.Share.Resource, Actor: actor})
	if err != nil || idempotent.Changed {
		t.Fatalf("idempotent GrantWorkspaceShare = %#v, %v", idempotent, err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(credential.RawKey))
	if err != nil {
		t.Fatalf("LoadControlPrincipal: %v", err)
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(principal.Revision, principal.EffectiveGrants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot: %v", err)
	}
	bucketRequirement := requirements[1]
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, accesscontrol.Actor{Authorization: snapshot}, bucketRequirement); err != nil {
		t.Fatalf("workspace-shared bucket permission: %v", err)
	}
	after, err := repository.PreflightArtifactOwnership(ctx, ArtifactOwnershipPreflight{ActorSubjectID: userResult.User.ID, OwnerTeamID: teamResult.Team.ID, Requirements: requirements})
	if err != nil || !after.Allowed || len(after.ActorMissing) != 0 || len(after.TeamMissing) != 0 {
		t.Fatalf("post-share decision = %#v, %v; want allowed", after, err)
	}
	shares, total, err := repository.ListWorkspaceShares(ctx, WorkspaceShareListOptions{Limit: 20})
	if err != nil || total != 1 || len(shares) != 1 || shares[0].Resource.ID != bucket.ID {
		t.Fatalf("ListWorkspaceShares = %#v/%d, %v", shares, total, err)
	}
	inactiveArtifactID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_artifact_scopes
			(account_id, artifact_id, owner_subject_id, scope_schema_version, selections, kind, name, version, deactivated_at)
		VALUES ($1, $2, $3, 1, '[]', 'sdk', 'Retired SDK', '1.0.0', NOW())
	`, accountID, inactiveArtifactID, owner.SubjectID); err != nil {
		t.Fatalf("insert inactive artifact: %v", err)
	}
	_, err = repository.GrantWorkspaceShare(ctx, WorkspaceShareMutation{
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: inactiveArtifactID}, Actor: actor,
	})
	if !errors.Is(err, ErrInvalidWorkspaceShare) {
		t.Fatalf("inactive artifact share error = %v, want ErrInvalidWorkspaceShare", err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_audit_events WHERE action = 'workspace.share.grant' AND resource_id = $1`, bucket.ID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("workspace share audit count = %d, %v; want one changed and one idempotent attempt", auditCount, err)
	}
}

func TestPostgresControlPrincipalUnionsFiftyTeamsWithoutDuplicateGrants(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	accountID := uuid.New()
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Team Union Workspace"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	if _, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_owner"); err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}

	userID := uuid.New()
	credentialHash := accesscontrol.HashControlCredential("fsk_team_user")
	if _, err := pool.Exec(ctx, `
		WITH subject AS (
			INSERT INTO fused_subjects (id, kind, display_name, status)
			VALUES ($1, 'user', 'Team User', 'active')
		)
		INSERT INTO fused_control_credentials (subject_id, key_hash, key_prefix, name)
		VALUES ($1, $2, 'fsk_team', 'test')
	`, userID, credentialHash); err != nil {
		t.Fatalf("insert user credential: %v", err)
	}

	teamIDs := make([]uuid.UUID, 50)
	teamNames := make([]string, 50)
	resourceIDs := make([]uuid.UUID, 50)
	for i := range teamIDs {
		teamIDs[i] = uuid.New()
		teamNames[i] = "team-" + uuid.NewString()
		resourceIDs[i] = uuid.New()
	}
	if _, err := pool.Exec(ctx, `
		WITH teams AS (
			INSERT INTO fused_teams (id, name, slug)
			SELECT id, slug, slug FROM unnest($1::uuid[], $2::text[]) AS input(id, slug)
		), memberships AS (
			INSERT INTO fused_team_memberships (team_id, member_subject_id)
			SELECT id, $3 FROM unnest($1::uuid[]) AS input(id)
		)
		INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id)
		SELECT 'team', input.team_id, role.id, 'service', input.resource_id
		FROM unnest($1::uuid[], $4::uuid[]) AS input(team_id, resource_id)
		CROSS JOIN fused_roles role
		WHERE role.slug = 'service-user'
	`, teamIDs, teamNames, userID, resourceIDs); err != nil {
		t.Fatalf("insert team grants: %v", err)
	}

	principal, err := repository.LoadControlPrincipal(ctx, credentialHash)
	if err != nil {
		t.Fatalf("LoadControlPrincipal: %v", err)
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(principal.Revision, principal.EffectiveGrants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot: %v", err)
	}
	scope, err := (accesscontrol.SnapshotAuthorizer{}).Scope(ctx, accesscontrol.Actor{Authorization: snapshot}, accesscontrol.PermissionServiceConsume, accesscontrol.ResourceService)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	if scope.All || len(scope.IDs) != 50 {
		t.Fatalf("service consume scope = %#v, want 50 resource IDs", scope)
	}
}

func TestPostgresControlPrincipalRecordsColdButNotCachedCredentialUse(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	accountID := uuid.New()
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Last Used Workspace"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	result, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_last_used")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	authenticator, err := accesscontrol.NewAuthenticator(repository, result.Revision, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, err := authenticator.AuthenticateControlCredential(ctx, "fsk_last_used"); err != nil {
		t.Fatalf("cold AuthenticateControlCredential: %v", err)
	}
	var usedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM fused_control_credentials WHERE id = $1`, result.CredentialID).Scan(&usedAt); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if usedAt == nil {
		t.Fatal("last_used_at was not recorded")
	}
	marker := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE fused_control_credentials SET last_used_at = $2 WHERE id = $1`, result.CredentialID, marker); err != nil {
		t.Fatalf("set last_used_at marker: %v", err)
	}
	if _, err := authenticator.AuthenticateControlCredential(ctx, "fsk_last_used"); err != nil {
		t.Fatalf("cached AuthenticateControlCredential: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM fused_control_credentials WHERE id = $1`, result.CredentialID).Scan(&usedAt); err != nil {
		t.Fatalf("read cached last_used_at: %v", err)
	}
	if usedAt == nil || !usedAt.Equal(marker) {
		t.Fatalf("cached authentication wrote last_used_at = %v, want %v", usedAt, marker)
	}
}

func TestPostgresControlPrincipalRejectsSuspendedOrRevokedCredential(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	accountID := uuid.New()
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Revocation Workspace"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	result, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_revoke")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	hash := accesscontrol.HashControlCredential("fsk_revoke")

	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'suspended' WHERE id = $1`, result.SubjectID); err != nil {
		t.Fatalf("suspend subject: %v", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, hash); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("suspended principal error = %v, want ErrAuthenticationRequired", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'active' WHERE id = $1`, result.SubjectID); err != nil {
		t.Fatalf("reactivate subject: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_control_credentials SET revoked_at = NOW() WHERE id = $1`, result.CredentialID); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, hash); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("revoked credential error = %v, want ErrAuthenticationRequired", err)
	}
}

func TestPostgresAuthorizedGraphQLCollectionsFilterBeforeTotals(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		DELETE FROM fused_artifact_buckets;
		DELETE FROM fused_artifact_scopes;
		DELETE FROM fused_workspace_service_versions;
		DELETE FROM fused_workspace_services;
		DELETE FROM fused_buckets;
	`); err != nil {
		t.Fatalf("reset collection fixtures: %v", err)
	}
	accountID := uuid.New()
	allowedArtifact, deniedArtifact := uuid.New(), uuid.New()
	ownerTeamID := seedArtifactOwnerTeam(t, ctx, pool)
	allowedBucket, deniedBucket := uuid.New(), uuid.New()
	allowedService, deniedService := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_artifact_scopes (account_id, artifact_id, owner_team_id, selections, kind, name)
		VALUES ($1, $2, $4, '[]', 'mcp', 'allowed'), ($1, $3, $4, '[]', 'mcp', 'denied')`, accountID, allowedArtifact, deniedArtifact, ownerTeamID); err != nil {
		t.Fatalf("insert artifact fixtures: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, 'allowed'), ($2, 'denied')`, allowedBucket, deniedBucket); err != nil {
		t.Fatalf("insert bucket fixtures: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_slug, service_name)
		VALUES ($1, 'allowed-service', 'Allowed'), ($2, 'denied-service', 'Denied')`, allowedService, deniedService); err != nil {
		t.Fatalf("insert service fixtures: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version)
		VALUES ($1, gen_random_uuid(), '1.0.0'), ($2, gen_random_uuid(), '1.0.0')`, allowedService, deniedService); err != nil {
		t.Fatalf("insert collection fixtures: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_artifact_buckets (artifact_id, bucket_id) VALUES ($1, $3), ($2, $3)`,
		allowedArtifact, deniedArtifact, allowedBucket); err != nil {
		t.Fatalf("insert related artifact fixtures: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_bucket_values (bucket_id, service_id, key_name, location, value)
		VALUES ($1, $2, 'allowed', 'header', 'one'), ($1, $3, 'denied', 'header', 'two')`,
		allowedBucket, allowedService, deniedService); err != nil {
		t.Fatalf("insert related service fixtures: %v", err)
	}

	artifacts, artifactTotal, err := repository.ListAuthorizedMCPScopesByAccount(ctx, accountID, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedArtifact}}, 10, 0)
	if err != nil || artifactTotal != 1 || len(artifacts) != 1 || artifacts[0].ArtifactID != allowedArtifact {
		t.Fatalf("authorized artifacts = %#v/%d, %v", artifacts, artifactTotal, err)
	}
	buckets, bucketTotal, err := repository.ListAuthorizedBucketSummaries(ctx, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedBucket}}, 10, 0)
	if err != nil || bucketTotal != 1 || len(buckets) != 1 || buckets[0].ID != allowedBucket {
		t.Fatalf("authorized buckets = %#v/%d, %v", buckets, bucketTotal, err)
	}
	services, err := repository.ListAuthorizedWorkspaceServices(ctx, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedService}}, nil)
	if err != nil || len(services) != 1 || services[0].ServiceID != allowedService {
		t.Fatalf("authorized services with nil names = %#v, %v", services, err)
	}
	services, err = repository.ListAuthorizedWorkspaceServices(ctx, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedService}}, []string{"allowed-service"})
	if err != nil || len(services) != 1 || services[0].ServiceID != allowedService {
		t.Fatalf("authorized services by slug = %#v, %v", services, err)
	}
	services, err = repository.ListAuthorizedWorkspaceServices(ctx, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedService}}, []string{"@provider/allowed-service"})
	if err != nil || len(services) != 1 || services[0].ServiceID != allowedService {
		t.Fatalf("authorized services by qualified slug = %#v, %v", services, err)
	}
	page, serviceTotal, err := repository.ListAuthorizedWorkspaceServicesPage(ctx, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedService}}, nil, 10, 0)
	if err != nil || serviceTotal != 1 || len(page) != 1 || page[0].ServiceID != allowedService {
		t.Fatalf("authorized service page with nil names = %#v/%d, %v", page, serviceTotal, err)
	}
	linkedBuckets, err := repository.ListAuthorizedBucketsForSDK(ctx, allowedArtifact, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedBucket}})
	if err != nil || len(linkedBuckets) != 1 || linkedBuckets[0].ID != allowedBucket {
		t.Fatalf("authorized linked buckets = %#v, %v", linkedBuckets, err)
	}
	linkedArtifacts, linkedArtifactTotal, err := repository.ListAuthorizedArtifactScopesForBucket(ctx, allowedBucket, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedArtifact}}, 10, 0)
	if err != nil || linkedArtifactTotal != 1 || len(linkedArtifacts) != 1 || linkedArtifacts[0].ArtifactID != allowedArtifact {
		t.Fatalf("authorized linked artifacts = %#v/%d, %v", linkedArtifacts, linkedArtifactTotal, err)
	}
	linkedServices, linkedServiceTotal, err := repository.ListAuthorizedBucketServiceSummaries(ctx, allowedBucket, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{allowedService}}, "", 10, 0)
	if err != nil || linkedServiceTotal != 1 || len(linkedServices) != 1 || linkedServices[0].ServiceID != allowedService {
		t.Fatalf("authorized linked services = %#v/%d, %v", linkedServices, linkedServiceTotal, err)
	}
}

func accessControlTestRepository(t *testing.T) (context.Context, context.CancelFunc, *pgxpool.Pool, *postgresStore) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	// Access-control tests mutate nearly every authorization table. A dedicated
	// schema keeps those decisions deterministic without erasing a developer's
	// local workspace or coupling this suite to legacy table state.
	pool := isolatedBootstrapPool(t, ctx, dbURL)
	return ctx, cancel, pool, NewPostgresStore(pool).(*postgresStore)
}
