package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestPostgresTeamCRUDUniquenessArchiveAndAudit(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "Team CRUD Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_team_crud")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	actor := MutationActor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, TraceID: "trace-team-crud"}

	created, err := repository.CreateTeam(ctx, TeamMutation{Name: "Payments", Slug: "payments", Description: "Payment systems", Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if !created.Changed || created.Team.Status != TeamStatusActive || created.AuthorizationRevision != owner.Revision {
		t.Fatalf("created team = %#v; metadata create must retain revision %d", created, owner.Revision)
	}
	if _, err := repository.CreateTeam(ctx, TeamMutation{Name: "Duplicate", Slug: "payments", Actor: actor}); !errors.Is(err, ErrTeamSlugConflict) {
		t.Fatalf("duplicate slug error = %v, want ErrTeamSlugConflict", err)
	}

	name := "Payment Platform"
	updated, err := repository.UpdateTeam(ctx, created.Team.ID, TeamPatch{Name: &name, Actor: actor})
	if err != nil {
		t.Fatalf("UpdateTeam: %v", err)
	}
	if !updated.Changed || updated.Team.Name != name || updated.AuthorizationRevision != owner.Revision {
		t.Fatalf("updated team = %#v; metadata update must retain revision %d", updated, owner.Revision)
	}
	unchanged, err := repository.UpdateTeam(ctx, created.Team.ID, TeamPatch{Name: &name, Actor: actor})
	if err != nil || unchanged.Changed || unchanged.AuthorizationRevision != owner.Revision {
		t.Fatalf("unchanged update = %#v, %v", unchanged, err)
	}

	team, err := repository.GetTeam(ctx, created.Team.ID)
	if err != nil || team.Name != name || team.Bindings == nil {
		t.Fatalf("GetTeam = %#v, %v", team, err)
	}
	teams, total, err := repository.ListTeams(ctx, TeamListOptions{Statuses: []TeamStatus{TeamStatusActive}, Search: "payment", Limit: 10})
	if err != nil || total != 1 || len(teams) != 1 || teams[0].ID != team.ID {
		t.Fatalf("ListTeams = %#v/%d, %v", teams, total, err)
	}

	archived, err := repository.ArchiveTeam(ctx, team.ID, actor)
	if err != nil || !archived.Changed || archived.Team.Status != TeamStatusArchived || archived.AuthorizationRevision != owner.Revision {
		t.Fatalf("ArchiveTeam = %#v, %v", archived, err)
	}
	archivedAgain, err := repository.ArchiveTeam(ctx, team.ID, actor)
	if err != nil || archivedAgain.Changed {
		t.Fatalf("idempotent ArchiveTeam = %#v, %v", archivedAgain, err)
	}

	assertTeamAudit(t, pool, "team.create", actor, workspaceID, team.ID, true)
	assertTeamAudit(t, pool, "team.update", actor, workspaceID, team.ID, true)
	assertTeamAudit(t, pool, "team.archive", actor, workspaceID, team.ID, true)
}

func TestPostgresTeamArchiveBlockedByWebhookOwnership(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	team, actor, configStateID := seedWebhookOwnedTeam(t, ctx, pool, repository)

	assertWebhookConfigStateIsNotArtifact(t, ctx, repository, team.ID, configStateID, actor)
	assertWebhookOwnershipBlocksArchive(t, ctx, repository, team.ID, actor)
	if _, err := pool.Exec(ctx, `DELETE FROM fused_config_states WHERE id = $1`, configStateID); err != nil {
		t.Fatalf("delete webhook config state: %v", err)
	}
	archived, err := repository.ArchiveTeam(ctx, team.ID, actor)
	if err != nil || !archived.Changed || archived.Team.Status != TeamStatusArchived {
		t.Fatalf("archive after webhook ownership removal = %#v, %v", archived, err)
	}
}

func TestPostgresTeamArchiveSeesWebhookApplyThatHeldTeamLockFirst(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	accountID := uuid.New()
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Archive Apply Serialization"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_archive_apply")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	actor := MutationActor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, TraceID: "trace-archive-apply"}
	created, err := repository.CreateTeam(ctx, TeamMutation{Name: "Concurrent owners", Slug: "concurrent-owners", Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	configRepository := NewPostgresConfigRepository(pool)
	plan, err := configRepository.CreateConfigPlan(ctx, CreateConfigPlanParams{
		ConfigKey: "webhook:" + uuid.NewString(), ConfigType: ConfigTypeWebhook,
		OwnerTeamID: &created.Team.ID, SourceHash: "sha256:archive-race",
		DesiredState: json.RawMessage(`{"name":"archive-race"}`), CreatedBy: accountID,
		RequiredPermissions: testRequiredPermissions(created.Team.ID), SupersedeExisting: true,
	})
	if err != nil {
		t.Fatalf("CreateConfigPlan: %v", err)
	}
	applyTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin plan blocker: %v", err)
	}
	defer applyTx.Rollback(ctx)
	if _, err := applyTx.Exec(ctx, `SELECT id FROM fused_config_plans WHERE id = $1 FOR UPDATE`, plan.ID); err != nil {
		t.Fatalf("lock apply plan: %v", err)
	}
	applyResult := make(chan error, 1)
	go func() {
		_, err := configRepository.ApplyWebhookConfigPlan(ctx, ApplyWebhookConfigPlanParams{Plan: ApplyConfigPlanParams{
			State:  UpsertConfigStateParams{ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &created.Team.ID, SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID},
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
		}})
		applyResult <- err
	}()
	waitForWebhookApplyPlanLock(t, ctx, pool)
	archiveResult := make(chan error, 1)
	go func() {
		_, err := repository.ArchiveTeam(ctx, created.Team.ID, actor)
		archiveResult <- err
	}()
	waitForArchiveTeamLock(t, ctx, pool)
	if err := applyTx.Commit(ctx); err != nil {
		t.Fatalf("release plan blocker: %v", err)
	}
	select {
	case err := <-applyResult:
		if err != nil {
			t.Fatalf("real webhook apply: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook apply did not resume after plan lock release")
	}
	select {
	case err := <-archiveResult:
		if !errors.Is(err, ErrTeamArchiveConflict) {
			t.Fatalf("archive error = %v, want ErrTeamArchiveConflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("archive did not resume after apply committed")
	}
}

func waitForWebhookApplyPlanLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid() AND wait_event_type = 'Lock'
				  AND query LIKE '%SELECT owner_team_id FROM fused_config_plans%'
			)`).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect webhook apply lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("webhook apply did not reach the plan lock after acquiring its team lock")
}

func waitForArchiveTeamLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%FROM fused_teams team WHERE team.id = $1 FOR UPDATE%'
			)
		`).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect archive lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("archive did not block on the owner-team lock")
}

func seedWebhookOwnedTeam(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repository *postgresStore) (Team, MutationActor, uuid.UUID) {
	t.Helper()
	accountID := uuid.New()
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Webhook Ownership Workspace"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_webhook_owner")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	actor := MutationActor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, TraceID: "trace-webhook-owner"}
	created, err := repository.CreateTeam(ctx, TeamMutation{Name: "Webhook Owners", Slug: "webhook-owners", Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	var configStateID uuid.UUID
	configKey := "webhook/" + uuid.NewString()
	if err := pool.QueryRow(ctx, `
		INSERT INTO fused_config_states (config_key, config_type, owner_team_id, source_hash)
		VALUES ($1, 'webhook', $2, $3)
		RETURNING id
	`, configKey, created.Team.ID, uuid.NewString()).Scan(&configStateID); err != nil {
		t.Fatalf("insert webhook config state: %v", err)
	}
	// SDK and MCP states mirror scope metadata and must not inflate ownership;
	// their live resource is already represented by fused_artifact_scopes.
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_config_states (config_key, config_type, owner_team_id, source_hash)
		VALUES ($1, 'sdk', $3, $5), ($2, 'mcp', $3, $4)
	`, "sdk/"+uuid.NewString(), "mcp/"+uuid.NewString(), created.Team.ID, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("insert SDK/MCP config states: %v", err)
	}
	return created.Team, actor, configStateID
}

func assertWebhookConfigStateIsNotArtifact(t *testing.T, ctx context.Context, repository *postgresStore, teamID, configStateID uuid.UUID, actor MutationActor) {
	t.Helper()
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{
		TeamID: teamID, RoleSlug: accesscontrol.RoleArtifactManager,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: configStateID}, Actor: actor,
	}); !errors.Is(err, ErrInvalidTeamBinding) {
		t.Fatalf("webhook config state artifact binding error = %v, want ErrInvalidTeamBinding", err)
	}
}

func assertWebhookOwnershipBlocksArchive(t *testing.T, ctx context.Context, repository *postgresStore, teamID uuid.UUID, actor MutationActor) {
	t.Helper()
	if _, err := repository.ArchiveTeam(ctx, teamID, actor); !errors.Is(err, ErrTeamArchiveConflict) {
		t.Fatalf("archive webhook-owning team error = %v, want ErrTeamArchiveConflict", err)
	} else {
		var conflict *TeamArchiveConflictError
		if !errors.As(err, &conflict) || conflict.ActiveArtifactCount != 1 {
			t.Fatalf("webhook archive conflict = %#v, %v", conflict, err)
		}
	}
}

func TestPostgresTeamBindingLevelReplacementInvalidationAndArchiveConflict(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "Team Binding Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_team_bindings")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	actor := MutationActor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, TraceID: "trace-team-binding"}
	teamResult, err := repository.CreateTeam(ctx, TeamMutation{Name: "Builders", Slug: "builders", Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	serviceID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_slug, service_name) VALUES ($1, 'billing', 'Billing')`, serviceID); err != nil {
		t.Fatalf("insert service: %v", err)
	}

	userID, userCredentialID, userHash := insertTeamBindingUser(t, ctx, pool, teamResult.Team.ID)
	useInput := TeamBindingMutation{
		TeamID: teamResult.Team.ID, RoleSlug: accesscontrol.RoleServiceUser,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, Actor: actor,
	}
	use, err := repository.AddTeamBinding(ctx, useInput)
	if err != nil || !use.Changed || use.AuthorizationRevision != owner.Revision+1 {
		t.Fatalf("grant use = %#v, %v", use, err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, userHash)
	if err != nil || !principalHasGrant(principal, accesscontrol.PermissionServiceConsume, serviceID) {
		t.Fatalf("user grant after add = %#v, %v", principal.EffectiveGrants, err)
	}
	if principal.SubjectID != userID || principal.CredentialID != userCredentialID {
		t.Fatalf("principal actor IDs = %s/%s", principal.SubjectID, principal.CredentialID)
	}

	duplicate, err := repository.AddTeamBinding(ctx, useInput)
	if err != nil || duplicate.Changed || duplicate.AuthorizationRevision != use.AuthorizationRevision {
		t.Fatalf("duplicate use = %#v, %v", duplicate, err)
	}
	manageInput := useInput
	manageInput.RoleSlug = accesscontrol.RoleServiceManager
	manage, err := repository.AddTeamBinding(ctx, manageInput)
	if err != nil || !manage.Changed || manage.AuthorizationRevision != use.AuthorizationRevision+1 {
		t.Fatalf("replace with manage = %#v, %v", manage, err)
	}
	assertSingleTeamResourceBinding(t, pool, teamResult.Team.ID, serviceID, accesscontrol.RoleServiceManager)
	workspaceInput := TeamBindingMutation{
		TeamID: teamResult.Team.ID, RoleSlug: accesscontrol.RoleViewer,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor,
	}
	viewer, err := repository.AddTeamBinding(ctx, workspaceInput)
	if err != nil || !viewer.Changed || viewer.AuthorizationRevision != manage.AuthorizationRevision+1 {
		t.Fatalf("grant workspace viewer = %#v, %v", viewer, err)
	}
	workspaceInput.RoleSlug = accesscontrol.RoleBuilder
	builder, err := repository.AddTeamBinding(ctx, workspaceInput)
	if err != nil || !builder.Changed || builder.AuthorizationRevision != viewer.AuthorizationRevision+1 {
		t.Fatalf("replace workspace viewer with builder = %#v, %v", builder, err)
	}
	assertSingleTeamResourceBinding(t, pool, teamResult.Team.ID, workspaceID, accesscontrol.RoleBuilder)
	bucketID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, "team-test-"+bucketID.String()); err != nil {
		t.Fatalf("insert bucket: %v", err)
	}
	bucketInput := TeamBindingMutation{
		TeamID: teamResult.Team.ID, RoleSlug: accesscontrol.RoleBucketUser,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}, Actor: actor,
	}
	bucketUser, err := repository.AddTeamBinding(ctx, bucketInput)
	if err != nil || !bucketUser.Changed || bucketUser.AuthorizationRevision != builder.AuthorizationRevision+1 {
		t.Fatalf("grant bucket user = %#v, %v", bucketUser, err)
	}
	bucketInput.RoleSlug = accesscontrol.RoleBucketManager
	bucketManager, err := repository.AddTeamBinding(ctx, bucketInput)
	if err != nil || !bucketManager.Changed || bucketManager.AuthorizationRevision != bucketUser.AuthorizationRevision+1 {
		t.Fatalf("replace bucket user with manager = %#v, %v", bucketManager, err)
	}
	assertSingleTeamResourceBinding(t, pool, teamResult.Team.ID, bucketID, accesscontrol.RoleBucketManager)
	bucketDuplicate, err := repository.AddTeamBinding(ctx, bucketInput)
	if err != nil || bucketDuplicate.Changed || bucketDuplicate.AuthorizationRevision != bucketManager.AuthorizationRevision {
		t.Fatalf("duplicate bucket manager = %#v, %v", bucketDuplicate, err)
	}
	principal, err = repository.LoadControlPrincipal(ctx, userHash)
	if err != nil || !principalHasGrant(principal, accesscontrol.PermissionBucketManage, bucketID) {
		t.Fatalf("user bucket grant after replacement = %#v, %v", principal.EffectiveGrants, err)
	}
	loadedTeam, err := repository.GetTeam(ctx, teamResult.Team.ID)
	if err != nil || len(loadedTeam.Bindings) != 3 {
		t.Fatalf("team with hydrated bindings = %#v, %v", loadedTeam, err)
	}

	if _, err := repository.ArchiveTeam(ctx, teamResult.Team.ID, actor); !errors.Is(err, ErrTeamArchiveConflict) {
		t.Fatalf("archive with binding error = %v, want ErrTeamArchiveConflict", err)
	}
	removed, err := repository.RemoveTeamBinding(ctx, manageInput)
	if err != nil || !removed.Changed || removed.AuthorizationRevision != bucketManager.AuthorizationRevision+1 {
		t.Fatalf("remove manage = %#v, %v", removed, err)
	}
	principal, err = repository.LoadControlPrincipal(ctx, userHash)
	if err != nil || principal.Revision != removed.AuthorizationRevision || principalHasGrant(principal, accesscontrol.PermissionServiceConsume, serviceID) {
		t.Fatalf("user grant after removal = revision %d grants %#v, %v", principal.Revision, principal.EffectiveGrants, err)
	}
	noOpRemoval, err := repository.RemoveTeamBinding(ctx, manageInput)
	if err != nil || noOpRemoval.Changed || noOpRemoval.AuthorizationRevision != removed.AuthorizationRevision || noOpRemoval.Binding.ID != uuid.Nil {
		t.Fatalf("no-op remove = %#v, %v", noOpRemoval, err)
	}

	workspaceRemoved, err := repository.ClearTeamWorkspaceRole(ctx, teamResult.Team.ID, workspaceID, actor)
	if err != nil || !workspaceRemoved.Changed || workspaceRemoved.AuthorizationRevision != removed.AuthorizationRevision+1 {
		t.Fatalf("clear workspace builder = %#v, %v", workspaceRemoved, err)
	}
	if workspaceRemoved.Binding.RoleSlug != accesscontrol.RoleBuilder || workspaceRemoved.Binding.ID == uuid.Nil {
		t.Fatalf("cleared workspace binding = %#v, want persisted Builder identity", workspaceRemoved.Binding)
	}
	workspaceNoOp, err := repository.ClearTeamWorkspaceRole(ctx, teamResult.Team.ID, workspaceID, actor)
	if err != nil || workspaceNoOp.Changed || workspaceNoOp.AuthorizationRevision != workspaceRemoved.AuthorizationRevision || workspaceNoOp.Binding.ID != uuid.Nil {
		t.Fatalf("idempotent workspace clear = %#v, %v", workspaceNoOp, err)
	}
	bucketRemoved, err := repository.RemoveTeamBinding(ctx, bucketInput)
	if err != nil || !bucketRemoved.Changed || bucketRemoved.AuthorizationRevision != workspaceNoOp.AuthorizationRevision+1 {
		t.Fatalf("remove bucket manager = %#v, %v", bucketRemoved, err)
	}
	archived, err := repository.ArchiveTeam(ctx, teamResult.Team.ID, actor)
	if err != nil || archived.Team.Status != TeamStatusArchived {
		t.Fatalf("archive after removal = %#v, %v", archived, err)
	}
	if _, err := repository.AddTeamBinding(ctx, useInput); !errors.Is(err, ErrTeamArchived) {
		t.Fatalf("archived grant error = %v, want ErrTeamArchived", err)
	}
	assertTeamBindingAudit(t, pool, actor, teamResult.Team.ID, serviceID)
}

func insertTeamBindingUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, teamID uuid.UUID) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	userID, credentialID := uuid.New(), uuid.New()
	hash := accesscontrol.HashControlCredential("fsk_team_member")
	if _, err := pool.Exec(ctx, `
		WITH subject AS (
			INSERT INTO fused_subjects (id, kind, display_name, status)
			VALUES ($1, 'user', 'Team Member', 'active')
		), credential AS (
			INSERT INTO fused_control_credentials (id, subject_id, key_hash, key_prefix, name)
			VALUES ($2, $1, $3, 'fsk_team', 'team test')
		)
		INSERT INTO fused_team_memberships (team_id, member_subject_id, created_by_subject_id)
		VALUES ($4, $1, $1)
	`, userID, credentialID, hash, teamID); err != nil {
		t.Fatalf("insert team member: %v", err)
	}
	return userID, credentialID, hash
}

func principalHasGrant(principal accesscontrol.ControlPrincipal, permission accesscontrol.Permission, resourceID uuid.UUID) bool {
	for _, grant := range principal.EffectiveGrants {
		if grant.Permission == permission && grant.Resource.ID == resourceID {
			return true
		}
	}
	return false
}

func assertSingleTeamResourceBinding(t *testing.T, pool *pgxpool.Pool, teamID, resourceID uuid.UUID, roleSlug string) {
	t.Helper()
	var count int
	var actualRole string
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*), MIN(role.slug)
		FROM fused_role_bindings binding JOIN fused_roles role ON role.id = binding.role_id
		WHERE binding.subject_type = 'team' AND binding.subject_id = $1 AND binding.resource_id = $2
	`, teamID, resourceID).Scan(&count, &actualRole); err != nil {
		t.Fatalf("load team resource bindings: %v", err)
	}
	if count != 1 || actualRole != roleSlug {
		t.Fatalf("team resource bindings = %d/%q, want 1/%q", count, actualRole, roleSlug)
	}
}

func assertTeamAudit(t *testing.T, pool *pgxpool.Pool, action string, actor MutationActor, workspaceID, teamID uuid.UUID, changed bool) {
	t.Helper()
	var subjectID, credentialID, resourceID uuid.UUID
	var auditTeamID string
	var auditChanged bool
	if err := pool.QueryRow(context.Background(), `
		SELECT actor_subject_id, actor_credential_id, resource_id,
			metadata->>'team_id', (metadata->>'changed')::boolean
		FROM fused_audit_events WHERE action = $1 AND (metadata->>'changed')::boolean = $2
		ORDER BY occurred_at LIMIT 1
	`, action, changed).Scan(&subjectID, &credentialID, &resourceID, &auditTeamID, &auditChanged); err != nil {
		t.Fatalf("load %s audit: %v", action, err)
	}
	if subjectID != actor.SubjectID || credentialID != actor.CredentialID || resourceID != workspaceID || auditTeamID != teamID.String() || auditChanged != changed {
		t.Fatalf("%s audit actor/resource/team/changed mismatch", action)
	}
}

func assertTeamBindingAudit(t *testing.T, pool *pgxpool.Pool, actor MutationActor, teamID, resourceID uuid.UUID) {
	t.Helper()
	var subjectID, credentialID, auditResourceID uuid.UUID
	var auditTeamID string
	if err := pool.QueryRow(context.Background(), `
		SELECT actor_subject_id, actor_credential_id, resource_id, metadata->>'team_id'
		FROM fused_audit_events WHERE action = 'team.binding.revoke' AND (metadata->>'changed')::boolean = true
			AND resource_id = $1
		ORDER BY occurred_at DESC LIMIT 1
	`, resourceID).Scan(&subjectID, &credentialID, &auditResourceID, &auditTeamID); err != nil {
		t.Fatalf("load team binding audit: %v", err)
	}
	if subjectID != actor.SubjectID || credentialID != actor.CredentialID || auditResourceID != resourceID || auditTeamID != teamID.String() {
		t.Fatal("team binding audit actor/resource/team mismatch")
	}
}
