package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestPostgresArtifactOwnershipSelectorsDiagnosticsAndAudit(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()

	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "Artifact access test")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	bootstrap, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_sprint5_test")
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	mutationActor := MutationActor{SubjectID: bootstrap.SubjectID, CredentialID: bootstrap.CredentialID, RequestID: "sprint5-test", TraceID: "0123456789abcdef0123456789abcdef"}
	user, err := repository.CreateUser(ctx, CreateUserInput{Email: "builder@example.com", DisplayName: "Builder", Actor: mutationActor})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'active' WHERE id = $1`, user.User.ID); err != nil {
		t.Fatalf("activate test user: %v", err)
	}
	teamA := createArtifactTestTeam(t, ctx, repository, "Team A", "team-a", mutationActor)
	teamB := createArtifactTestTeam(t, ctx, repository, "Team B", "team-b", mutationActor)
	serviceID, bucketID := seedArtifactSelectorResources(t, ctx, repository, accountID)
	bindArtifactBuildResources(t, ctx, repository, teamA.ID, workspaceID, serviceID, bucketID, mutationActor)
	bindArtifactBuildResources(t, ctx, repository, teamB.ID, workspaceID, serviceID, bucketID, mutationActor)
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: teamA.ID, UserID: user.User.ID, Role: MembershipRoleMember, Actor: mutationActor}); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}

	requirements := []accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionArtifactCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
	}
	allowed, err := repository.PreflightArtifactOwnership(ctx, ArtifactOwnershipPreflight{ActorSubjectID: user.User.ID, OwnerTeamID: teamA.ID, Requirements: requirements})
	if err != nil || !allowed.Allowed || len(allowed.ActorMissing) != 0 || len(allowed.TeamMissing) != 0 {
		t.Fatalf("allowed ownership decision = %#v, %v", allowed, err)
	}
	forged, err := repository.PreflightArtifactOwnership(ctx, ArtifactOwnershipPreflight{ActorSubjectID: user.User.ID, OwnerTeamID: teamB.ID, Requirements: requirements})
	if err != nil || forged.Allowed || forged.MembershipAllowed {
		t.Fatalf("forged ownership decision = %#v, %v", forged, err)
	}
	assertArtifactSelectors(t, ctx, repository, user.User.ID, teamA.ID, teamB.ID, serviceID, bucketID)
	assertArtifactOwningTeams(t, ctx, repository, user.User.ID, teamA.ID)
	assertAccessExplanation(t, ctx, repository, bootstrap.SubjectID, user.User.ID, serviceID)

	artifactID := uuid.New()
	actorCtx := accesscontrol.ContextWithActor(ctx, accesscontrol.Actor{SubjectID: user.User.ID})
	if err := repository.SaveArtifactScope(actorCtx, ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerTeamID: teamA.ID, BucketID: bucketID, Selections: []byte(`["old"]`), ScopeSchemaVersion: 1, Kind: "sdk", Name: "shared"}); err != nil {
		t.Fatalf("SaveArtifactScope: %v", err)
	}
	if err := repository.SaveArtifactScope(actorCtx, ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerTeamID: teamB.ID, Selections: []byte("[]"), ScopeSchemaVersion: 1}); !errors.Is(err, ErrArtifactOwnerMismatch) {
		t.Fatalf("owner change error = %v, want ErrArtifactOwnerMismatch", err)
	}
	if _, err := repository.ArchiveTeam(ctx, teamA.ID, mutationActor); err == nil {
		t.Fatal("expected active artifact ownership to block team archive")
	} else {
		var conflict *TeamArchiveConflictError
		if !errors.As(err, &conflict) || conflict.ActiveArtifactCount != 1 {
			t.Fatalf("archive conflict = %#v, %v", conflict, err)
		}
	}
	assertAtomicArtifactScopeBucket(t, actorCtx, repository, pool, accountID, teamA.ID, artifactID, bucketID)
	assertSharedManagerPreflight(t, ctx, repository, pool, teamB, artifactID, teamA.ID, requirements, mutationActor)

	tokenHash := "sprint5-token-" + uuid.NewString()
	if _, err := repository.CreateSDKToken(ctx, artifactID, tokenHash, "runtime"); err != nil {
		t.Fatalf("CreateSDKToken: %v", err)
	}
	if _, err := repository.RemoveTeamMember(ctx, teamA.ID, user.User.ID, mutationActor); err != nil {
		t.Fatalf("RemoveTeamMember: %v", err)
	}
	if token, err := repository.GetArtifactByToken(ctx, tokenHash); err != nil || token.ArtifactID != artifactID {
		t.Fatalf("runtime token after membership removal = %#v, %v", token, err)
	}
	assertSuspendedRequesterFailsClosed(t, ctx, repository, pool, workspaceID, user.User.ID, serviceID, mutationActor)
	assertStableSafeAuditPages(t, ctx, repository, bootstrap.SubjectID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_config_states (config_key, config_type, owner_team_id, source_hash, latest_resource_id, updated_by)
		VALUES ($1, 'sdk', $2, 'delete-test', $3, $4)
	`, "sdk:delete:"+uuid.NewString(), teamA.ID, artifactID, accountID); err != nil {
		t.Fatalf("seed artifact config state: %v", err)
	}
	if err := repository.DeleteArtifactScope(ctx, accountID, artifactID); err != nil {
		t.Fatalf("DeleteArtifactScope: %v", err)
	}
	var orphanBindings int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_role_bindings WHERE resource_type = 'artifact' AND resource_id = $1`, artifactID).Scan(&orphanBindings); err != nil || orphanBindings != 0 {
		t.Fatalf("orphan artifact bindings = %d, %v", orphanBindings, err)
	}
	var configStates int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_config_states WHERE latest_resource_id = $1`, artifactID).Scan(&configStates); err != nil || configStates != 0 {
		t.Fatalf("artifact config references after delete = %d, %v", configStates, err)
	}
}

func TestPostgresArtifactScopeSupportsSubjectOwner(t *testing.T) {
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
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Subject-owned artifact test"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	bootstrap, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_subject_owner_"+uuid.NewString())
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	serviceID, bucketID := seedArtifactSelectorResources(t, ctx, repository, accountID)
	servicePage, err := repository.ListArtifactBuildSelectors(ctx, ArtifactSelectorQuery{ActorSubjectID: bootstrap.SubjectID, ResourceType: accesscontrol.ResourceService, Limit: 10})
	if err != nil || servicePage.Total != 1 || len(servicePage.Items) != 1 || servicePage.Items[0].Resource.ID != serviceID {
		t.Fatalf("personal service selectors = %#v, %v", servicePage, err)
	}
	bucketPage, err := repository.ListArtifactBuildSelectors(ctx, ArtifactSelectorQuery{ActorSubjectID: bootstrap.SubjectID, ResourceType: accesscontrol.ResourceBucket, Limit: 10})
	if err != nil || !selectorPageContains(bucketPage, bucketID) {
		t.Fatalf("personal bucket selectors = %#v, %v", bucketPage, err)
	}
	artifactID := uuid.New()
	actorCtx := accesscontrol.ContextWithActor(ctx, accesscontrol.Actor{SubjectID: bootstrap.SubjectID, CredentialID: bootstrap.CredentialID})
	scope := ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerSubjectID: bootstrap.SubjectID, Selections: []byte("[]"), ScopeSchemaVersion: 2, Kind: "sdk", Name: "personal"}
	if err := repository.SaveArtifactScope(actorCtx, scope); err != nil {
		t.Fatalf("SaveArtifactScope(subject): %v", err)
	}
	stored, err := repository.GetArtifactScope(ctx, artifactID)
	if err != nil || stored.OwnerSubjectID != bootstrap.SubjectID || stored.OwnerTeamID != uuid.Nil {
		t.Fatalf("stored subject owner = %#v, %v", stored, err)
	}
	var bindingCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM fused_role_bindings binding
		JOIN fused_roles role ON role.id = binding.role_id
		WHERE binding.subject_type = 'subject' AND binding.subject_id = $1
		  AND binding.resource_type = 'artifact' AND binding.resource_id = $2
		  AND role.slug = $3
	`, bootstrap.SubjectID, artifactID, accesscontrol.RoleArtifactManager).Scan(&bindingCount); err != nil || bindingCount != 1 {
		t.Fatalf("subject artifact-manager binding count = %d, %v", bindingCount, err)
	}
	mutationActor := MutationActor{SubjectID: bootstrap.SubjectID, CredentialID: bootstrap.CredentialID, RequestID: "subject-owner-test", TraceID: "0123456789abcdef0123456789abcdef"}
	other, err := repository.CreateUser(ctx, CreateUserInput{Email: "other-owner@example.com", DisplayName: "Other Owner", Actor: mutationActor})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'active' WHERE id = $1`, other.User.ID); err != nil {
		t.Fatalf("activate other owner: %v", err)
	}
	if err := repository.SaveArtifactScope(actorCtx, ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerSubjectID: other.User.ID, Selections: []byte("[]"), ScopeSchemaVersion: 2}); !errors.Is(err, ErrArtifactOwnerMismatch) {
		t.Fatalf("subject owner replacement error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'suspended' WHERE id = $1`, bootstrap.SubjectID); err != nil {
		t.Fatalf("suspend subject owner: %v", err)
	}
	if err := repository.SaveArtifactScope(actorCtx, scope); err != nil {
		t.Fatalf("administrator-manageable suspended owner scope: %v", err)
	}
	newArtifact := scope
	newArtifact.ArtifactID = uuid.New()
	// Use a new logical identity so the assertion reaches the suspended-owner
	// authorization fence instead of the independent name/version constraint.
	newArtifact.Name = "new-personal"
	if err := repository.SaveArtifactScope(actorCtx, newArtifact); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("new artifact with suspended owner error = %v", err)
	}
}

func selectorPageContains(page ArtifactSelectorPage, resourceID uuid.UUID) bool {
	for _, item := range page.Items {
		if item.Resource.ID == resourceID {
			return true
		}
	}
	return false
}

func assertSuspendedRequesterFailsClosed(t *testing.T, ctx context.Context, repository *postgresStore, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, workspaceID, targetUserID, serviceID uuid.UUID, actor MutationActor) {
	t.Helper()
	inspector, err := repository.CreateUser(ctx, CreateUserInput{Email: "inspector@example.com", DisplayName: "Inspector", Actor: actor})
	if err != nil {
		t.Fatalf("CreateUser(inspector): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'active' WHERE id = $1`, inspector.User.ID); err != nil {
		t.Fatalf("activate inspector: %v", err)
	}
	team := createArtifactTestTeam(t, ctx, repository, "Inspectors", "inspectors", actor)
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: team.ID, RoleSlug: accesscontrol.RoleOwner,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor}); err != nil {
		t.Fatalf("grant inspector owner: %v", err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: team.ID, UserID: inspector.User.ID, Role: MembershipRoleMember, Actor: actor}); err != nil {
		t.Fatalf("add inspector membership: %v", err)
	}
	explanationQuery := AccessExplanationQuery{RequesterSubjectID: inspector.User.ID, TargetSubjectID: targetUserID,
		Requirement: accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}}}
	if _, err := repository.ExplainAccess(ctx, explanationQuery); err != nil {
		t.Fatalf("active inspector explanation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'suspended' WHERE id = $1`, inspector.User.ID); err != nil {
		t.Fatalf("suspend inspector: %v", err)
	}
	if _, err := repository.ExplainAccess(ctx, explanationQuery); !errors.Is(err, ErrAccessExplanationHidden) {
		t.Fatalf("suspended explanation error = %v", err)
	}
	auditPage, err := repository.QueryAuditEvents(ctx, AuditQuery{RequesterSubjectID: inspector.User.ID, Limit: 10})
	if err != nil || auditPage.Total != 0 || len(auditPage.Items) != 0 {
		t.Fatalf("suspended audit page = %#v, %v", auditPage, err)
	}
	exported, err := repository.ExportAuditEvents(ctx, AuditExportQuery{RequesterSubjectID: inspector.User.ID, Limit: 10})
	if err != nil || len(exported) != 0 {
		t.Fatalf("suspended audit export = %#v, %v", exported, err)
	}
	teams, err := repository.ListArtifactOwningTeams(ctx, ActorTeamSelectorQuery{ActorSubjectID: inspector.User.ID, Limit: 10})
	if err != nil || teams.Total != 0 || len(teams.Items) != 0 {
		t.Fatalf("suspended owning teams = %#v, %v", teams, err)
	}
}

func assertAtomicArtifactScopeBucket(t *testing.T, ctx context.Context, repository *postgresStore, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, accountID, ownerTeamID, artifactID, originalBucketID uuid.UUID) {
	t.Helper()
	otherBucketID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, otherBucketID, "Other-"+otherBucketID.String()); err != nil {
		t.Fatalf("insert other bucket: %v", err)
	}
	if err := repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerTeamID: ownerTeamID,
		BucketID: originalBucketID, Selections: []byte(`["same-bucket"]`), ScopeSchemaVersion: 1}); err != nil {
		t.Fatalf("same bucket update: %v", err)
	}
	var auditBefore int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_audit_events WHERE action = 'artifact.scope.persist' AND resource_id = $1`, artifactID).Scan(&auditBefore); err != nil {
		t.Fatalf("count artifact audits: %v", err)
	}
	err := repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerTeamID: ownerTeamID,
		Selections: []byte(`["removed"]`), ScopeSchemaVersion: 1})
	if !errors.Is(err, ErrSDKBucketImmutable) {
		t.Fatalf("bucket removal error = %v, want ErrSDKBucketImmutable", err)
	}
	err = repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: artifactID, OwnerTeamID: ownerTeamID,
		BucketID: otherBucketID, Selections: []byte(`["forged"]`), ScopeSchemaVersion: 1})
	if !errors.Is(err, ErrSDKBucketImmutable) {
		t.Fatalf("bucket replacement error = %v, want ErrSDKBucketImmutable", err)
	}
	scope, err := repository.GetArtifactScope(ctx, artifactID)
	if err != nil || scope.BucketID != originalBucketID || string(scope.Selections) != `["same-bucket"]` {
		t.Fatalf("scope after immutable rejection = %#v, %v", scope, err)
	}
	var auditAfter int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_audit_events WHERE action = 'artifact.scope.persist' AND resource_id = $1`, artifactID).Scan(&auditAfter); err != nil || auditAfter != auditBefore {
		t.Fatalf("audit count after rejection = %d, %v; before=%d", auditAfter, err, auditBefore)
	}
	nilBucketArtifactID := uuid.New()
	if err := repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: nilBucketArtifactID, OwnerTeamID: ownerTeamID,
		Selections: []byte(`["nil-initial"]`), ScopeSchemaVersion: 1}); err != nil {
		t.Fatalf("create nil-bucket artifact: %v", err)
	}
	if err := repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: nilBucketArtifactID, OwnerTeamID: ownerTeamID,
		Selections: []byte(`["nil-idempotent"]`), ScopeSchemaVersion: 1}); err != nil {
		t.Fatalf("same nil-bucket update: %v", err)
	}
	var nilAuditBefore int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_audit_events WHERE action = 'artifact.scope.persist' AND resource_id = $1`, nilBucketArtifactID).Scan(&nilAuditBefore); err != nil {
		t.Fatalf("count nil-bucket audits: %v", err)
	}
	err = repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: nilBucketArtifactID, OwnerTeamID: ownerTeamID,
		BucketID: originalBucketID, Selections: []byte(`["added"]`), ScopeSchemaVersion: 1})
	if !errors.Is(err, ErrSDKBucketImmutable) {
		t.Fatalf("bucket addition error = %v, want ErrSDKBucketImmutable", err)
	}
	nilScope, err := repository.GetArtifactScope(ctx, nilBucketArtifactID)
	if err != nil || nilScope.BucketID != uuid.Nil || string(nilScope.Selections) != `["nil-idempotent"]` {
		t.Fatalf("nil scope after immutable rejection = %#v, %v", nilScope, err)
	}
	var nilAuditAfter int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_audit_events WHERE action = 'artifact.scope.persist' AND resource_id = $1`, nilBucketArtifactID).Scan(&nilAuditAfter); err != nil || nilAuditAfter != nilAuditBefore {
		t.Fatalf("nil audit count after rejection = %d, %v; before=%d", nilAuditAfter, err, nilAuditBefore)
	}
	orphanID := uuid.New()
	err = repository.SaveArtifactScope(ctx, ArtifactScope{AccountID: accountID, ArtifactID: orphanID, OwnerTeamID: ownerTeamID,
		BucketID: uuid.New(), Selections: []byte(`["new"]`), ScopeSchemaVersion: 1})
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("missing bucket error = %v, want ErrBucketNotFound", err)
	}
	if _, err := repository.GetArtifactScope(ctx, orphanID); !errors.Is(err, ErrArtifactScopeNotFound) {
		t.Fatalf("orphan scope error = %v", err)
	}
}

func createArtifactTestTeam(t *testing.T, ctx context.Context, repository *postgresStore, name, slug string, actor MutationActor) Team {
	t.Helper()
	result, err := repository.CreateTeam(ctx, TeamMutation{Name: name, Slug: slug, Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam(%s): %v", slug, err)
	}
	return result.Team
}

func seedArtifactSelectorResources(t *testing.T, ctx context.Context, repository *postgresStore, accountID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	serviceID, versionID, bucketID := uuid.New(), uuid.New(), uuid.New()
	if err := repository.AddWorkspaceServiceVersion(ctx, serviceID, "payments", "1.0.0", versionID, "Payments", accountID); err != nil {
		t.Fatalf("AddWorkspaceServiceVersion: %v", err)
	}
	if _, err := repository.db.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, "Production-"+bucketID.String()); err != nil {
		t.Fatalf("insert bucket: %v", err)
	}
	// This raw orphan represents a deactivated/invalid service fixture and must
	// never appear because selectors require an active version row in SQL.
	if _, err := repository.db.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_name) VALUES ($1, 'Inactive')`, uuid.New()); err != nil {
		t.Fatalf("insert inactive service fixture: %v", err)
	}
	return serviceID, bucketID
}

func bindArtifactBuildResources(t *testing.T, ctx context.Context, repository *postgresStore, teamID, workspaceID, serviceID, bucketID uuid.UUID, actor MutationActor) {
	t.Helper()
	for _, binding := range []TeamBindingMutation{
		{TeamID: teamID, RoleSlug: accesscontrol.RoleBuilder, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor},
		{TeamID: teamID, RoleSlug: accesscontrol.RoleServiceUser, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, Actor: actor},
		{TeamID: teamID, RoleSlug: accesscontrol.RoleBucketUser, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}, Actor: actor},
	} {
		if _, err := repository.AddTeamBinding(ctx, binding); err != nil {
			t.Fatalf("AddTeamBinding(%s): %v", binding.RoleSlug, err)
		}
	}
}

func assertArtifactSelectors(t *testing.T, ctx context.Context, repository *postgresStore, actorID, teamAID, teamBID, serviceID, bucketID uuid.UUID) {
	t.Helper()
	resolvedTeamID, err := repository.ResolveArtifactOwningTeamReference(ctx, ArtifactOwningTeamReferenceQuery{ActorSubjectID: actorID, Reference: "team-a"})
	if err != nil || resolvedTeamID != teamAID {
		t.Fatalf("authorized owning-team reference = %s, %v", resolvedTeamID, err)
	}
	for _, reference := range []string{"team-b", teamBID.String(), "missing-team"} {
		if _, err := repository.ResolveArtifactOwningTeamReference(ctx, ArtifactOwningTeamReferenceQuery{ActorSubjectID: actorID, Reference: reference}); !errors.Is(err, ErrResourceReferenceNotFound) {
			t.Fatalf("ineligible owning-team reference %q error = %v", reference, err)
		}
	}
	servicePage, err := repository.ListArtifactBuildSelectors(ctx, ArtifactSelectorQuery{ActorSubjectID: actorID, OwnerTeamID: teamAID, ResourceType: accesscontrol.ResourceService, Limit: 1})
	if err != nil || servicePage.Total != 1 || len(servicePage.Items) != 1 || servicePage.Items[0].Resource.ID != serviceID {
		t.Fatalf("service selector = %#v, %v", servicePage, err)
	}
	pastEnd, err := repository.ListArtifactBuildSelectors(ctx, ArtifactSelectorQuery{ActorSubjectID: actorID, OwnerTeamID: teamAID, ResourceType: accesscontrol.ResourceService, Limit: 1, Offset: 10})
	if err != nil || pastEnd.Total != 1 || len(pastEnd.Items) != 0 {
		t.Fatalf("past-end selector = %#v, %v", pastEnd, err)
	}
	bucketPage, err := repository.ListArtifactBuildSelectors(ctx, ArtifactSelectorQuery{ActorSubjectID: actorID, OwnerTeamID: teamAID, ResourceType: accesscontrol.ResourceBucket, Limit: 1})
	if err != nil || bucketPage.Total != 1 || len(bucketPage.Items) != 1 || bucketPage.Items[0].Resource.ID != bucketID {
		t.Fatalf("bucket selector = %#v, %v", bucketPage, err)
	}
	forged, err := repository.ListArtifactBuildSelectors(ctx, ArtifactSelectorQuery{ActorSubjectID: actorID, OwnerTeamID: teamBID, ResourceType: accesscontrol.ResourceService, Limit: 10})
	if err != nil || forged.Total != 0 || len(forged.Items) != 0 {
		t.Fatalf("forged selector = %#v, %v", forged, err)
	}
}

func assertArtifactOwningTeams(t *testing.T, ctx context.Context, repository *postgresStore, actorID, expectedTeamID uuid.UUID) {
	t.Helper()
	page, err := repository.ListArtifactOwningTeams(ctx, ActorTeamSelectorQuery{ActorSubjectID: actorID, Limit: 10})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != expectedTeamID {
		t.Fatalf("artifact owning teams = %#v, %v", page, err)
	}
}

func assertAccessExplanation(t *testing.T, ctx context.Context, repository *postgresStore, requesterID, targetID, serviceID uuid.UUID) {
	t.Helper()
	query := AccessExplanationQuery{RequesterSubjectID: requesterID, TargetSubjectID: targetID,
		Requirement: accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}}}
	explanation, err := repository.ExplainAccess(ctx, query)
	if err != nil || !explanation.Allowed || len(explanation.Sources) != 1 || explanation.Sources[0].RoleSlug != accesscontrol.RoleServiceUser {
		t.Fatalf("access explanation = %#v, %v", explanation, err)
	}
	query.RequesterSubjectID = targetID
	if _, err := repository.ExplainAccess(ctx, query); !errors.Is(err, ErrAccessExplanationHidden) {
		t.Fatalf("hidden explanation error = %v", err)
	}
}

func assertSharedManagerPreflight(t *testing.T, ctx context.Context, repository *postgresStore, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sharedTeam Team, artifactID, ownerTeamID uuid.UUID, requirements []accesscontrol.Requirement, actor MutationActor) {
	t.Helper()
	sharedUser, err := repository.CreateUser(ctx, CreateUserInput{Email: "shared@example.com", DisplayName: "Shared builder", Actor: actor})
	if err != nil {
		t.Fatalf("CreateUser(shared): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'active' WHERE id = $1`, sharedUser.User.ID); err != nil {
		t.Fatalf("activate shared user: %v", err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: sharedTeam.ID, UserID: sharedUser.User.ID, Role: MembershipRoleMember, Actor: actor}); err != nil {
		t.Fatalf("AddTeamMember(shared): %v", err)
	}
	existingArtifactID := artifactID
	input := ArtifactOwnershipPreflight{ActorSubjectID: sharedUser.User.ID, OwnerTeamID: ownerTeamID, ExistingArtifactID: &existingArtifactID, Requirements: requirements}
	unshared, err := repository.PreflightArtifactOwnership(ctx, input)
	if err != nil || unshared.Allowed || unshared.MembershipAllowed {
		t.Fatalf("unshared update decision = %#v, %v", unshared, err)
	}
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: sharedTeam.ID, RoleSlug: accesscontrol.RoleArtifactReader,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: artifactID}, Actor: actor}); err != nil {
		t.Fatalf("artifact reader share: %v", err)
	}
	reader, err := repository.PreflightArtifactOwnership(ctx, input)
	if err != nil || reader.Allowed || reader.MembershipAllowed {
		t.Fatalf("reader update decision = %#v, %v", reader, err)
	}
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: sharedTeam.ID, RoleSlug: accesscontrol.RoleArtifactManager,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: artifactID}, Actor: actor}); err != nil {
		t.Fatalf("artifact manager share: %v", err)
	}
	manager, err := repository.PreflightArtifactOwnership(ctx, input)
	if err != nil || !manager.Allowed || !manager.MembershipAllowed {
		t.Fatalf("manager update decision = %#v, %v", manager, err)
	}
	input.ExistingArtifactID = nil
	create, err := repository.PreflightArtifactOwnership(ctx, input)
	if err != nil || create.Allowed || create.MembershipAllowed {
		t.Fatalf("shared manager create decision = %#v, %v", create, err)
	}
}

func assertStableSafeAuditPages(t *testing.T, ctx context.Context, repository *postgresStore, requesterID uuid.UUID) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Minute)
	for index := 0; index < 3; index++ {
		if err := repository.RecordAuthorizationAudit(ctx, accesscontrol.AuditEvent{OccurredAt: base.Add(time.Duration(index) * time.Second),
			Action: "artifact.audit.test", Outcome: accesscontrol.AuditSucceeded, Metadata: map[string]any{"artifact_count": index}}); err != nil {
			t.Fatalf("RecordAuthorizationAudit: %v", err)
		}
	}
	query := AuditQuery{RequesterSubjectID: requesterID, Actions: []string{"artifact.audit.test"}, Limit: 2}
	first, err := repository.QueryAuditEvents(ctx, query)
	if err != nil || first.Total != 3 || len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first audit page = %#v, %v", first, err)
	}
	query.After = first.NextCursor
	second, err := repository.QueryAuditEvents(ctx, query)
	if err != nil || second.Total != 3 || len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID || second.Items[0].ID == first.Items[1].ID {
		t.Fatalf("second audit page = %#v, %v", second, err)
	}
	exported, err := repository.ExportAuditEvents(ctx, AuditExportQuery{RequesterSubjectID: requesterID, Actions: []string{"artifact.audit.test"}, Limit: 10})
	if err != nil || len(exported) != 3 {
		t.Fatalf("audit export = %#v, %v", exported, err)
	}
}
