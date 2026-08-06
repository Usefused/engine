package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestPostgresUserLifecycleCredentialIsOneTimeAndAuditIsSecretSafe(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, owner, actor := bootstrapUserTest(t, ctx, repository, "lifecycle")

	created, err := repository.CreateUser(ctx, CreateUserInput{Email: "Alice@Example.COM", DisplayName: "Alice", Actor: actor})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.User.Status != UserStatusInvited || created.User.Email != "Alice@Example.COM" || created.AuthorizationRevision != owner.Revision+1 {
		t.Fatalf("created user = %#v", created)
	}
	if _, err := repository.CreateUser(ctx, CreateUserInput{Email: "alice@example.com", DisplayName: "Other", Actor: actor}); !errors.Is(err, ErrUserEmailConflict) {
		t.Fatalf("case-insensitive duplicate error = %v, want ErrUserEmailConflict", err)
	}
	var projectedUsers, userSubjects int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_users`).Scan(&projectedUsers); err != nil {
		t.Fatalf("count user projections: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_subjects WHERE kind = 'user'`).Scan(&userSubjects); err != nil {
		t.Fatalf("count user subjects: %v", err)
	}
	if projectedUsers != 1 || userSubjects != 1 {
		t.Fatalf("duplicate creation atomicity = %d projections/%d subjects, want 1/1", projectedUsers, userSubjects)
	}
	if _, err := repository.ReactivateUser(ctx, created.User.ID, actor); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("reactivate invited user error = %v, want ErrInvalidUser until credential issue", err)
	}

	previousUpdatedAt := created.User.UpdatedAt
	name := "Alice Smith"
	updated, err := repository.UpdateUser(ctx, created.User.ID, UserPatch{DisplayName: &name, Actor: actor})
	if err != nil || !updated.Changed || updated.User.DisplayName != name || !updated.User.UpdatedAt.After(previousUpdatedAt) {
		t.Fatalf("UpdateUser = %#v, %v", updated, err)
	}
	if updated.AuthorizationRevision != created.AuthorizationRevision {
		t.Fatalf("metadata update revision = %d, want %d", updated.AuthorizationRevision, created.AuthorizationRevision)
	}

	previousTracer := otel.GetTracerProvider()
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	otel.SetTracerProvider(tracerProvider)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
		otel.SetTracerProvider(previousTracer)
	}()
	issued, err := repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: created.User.ID, Name: "Laptop", Actor: actor})
	if err != nil {
		t.Fatalf("IssueUserControlCredential: %v", err)
	}
	if !issued.Changed || !strings.HasPrefix(issued.RawKey, "fsk_") || issued.Credential.KeyPrefix != accesscontrol.CredentialPrefix(issued.RawKey) {
		t.Fatalf("issued credential metadata = %#v", issued)
	}
	assertRawCredentialNotPersisted(t, ctx, pool, issued)
	assertRawCredentialNotTraced(t, spanRecorder, issued.RawKey)
	authenticator, err := accesscontrol.NewAuthenticator(repository, issued.AuthorizationRevision, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	principalActor, err := authenticator.AuthenticateControlCredential(ctx, issued.RawKey)
	if err != nil || principalActor.SubjectID != created.User.ID || principalActor.DisplayName != created.User.DisplayName || principalActor.Email != created.User.Email {
		t.Fatalf("AuthenticateControlCredential = %#v, %v", principalActor, err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(issued.RawKey))
	if err != nil || principal.SubjectID != created.User.ID || principal.Kind != accesscontrol.SubjectUser || principal.DisplayName != created.User.DisplayName || principal.Email != created.User.Email {
		t.Fatalf("issued principal = %#v, %v", principal, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(created.User.Email)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("email authentication error = %v, want ErrAuthenticationRequired", err)
	}

	suspended, err := repository.SuspendUser(ctx, created.User.ID, actor)
	if err != nil || suspended.User.Status != UserStatusSuspended || len(suspended.User.Credentials) != 1 {
		t.Fatalf("SuspendUser = %#v, %v", suspended, err)
	}
	authenticator.SetRevision(suspended.AuthorizationRevision)
	if _, err := authenticator.AuthenticateControlCredential(ctx, issued.RawKey); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("cached suspended authentication error = %v, want ErrAuthenticationRequired", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(issued.RawKey)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("suspended authentication error = %v, want 401 authentication failure", err)
	}
	reactivated, err := repository.ReactivateUser(ctx, created.User.ID, actor)
	if err != nil {
		t.Fatalf("ReactivateUser: %v", err)
	}
	authenticator.SetRevision(reactivated.AuthorizationRevision)
	if _, err := authenticator.AuthenticateControlCredential(ctx, issued.RawKey); err != nil {
		t.Fatalf("reactivated cached authentication: %v", err)
	}
	revoked, err := repository.RevokeUserControlCredential(ctx, created.User.ID, issued.Credential.ID, actor)
	if err != nil || !revoked.Changed {
		t.Fatalf("RevokeUserControlCredential = %#v, %v", revoked, err)
	}
	authenticator.SetRevision(revoked.AuthorizationRevision)
	if _, err := authenticator.AuthenticateControlCredential(ctx, issued.RawKey); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("cached revoked authentication error = %v, want ErrAuthenticationRequired", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(issued.RawKey)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("revoked authentication error = %v, want ErrAuthenticationRequired", err)
	}
	revokedAgain, err := repository.RevokeUserControlCredential(ctx, created.User.ID, issued.Credential.ID, actor)
	if err != nil || revokedAgain.Changed || revokedAgain.AuthorizationRevision != revoked.AuthorizationRevision {
		t.Fatalf("idempotent revoke = %#v, %v", revokedAgain, err)
	}
	assertUserAuditContext(t, ctx, pool, workspaceID, actor, created.User.ID, issued.Credential.ID)
}

func TestPostgresAddByEmailAndUserHydrationDeduplicateMembershipsAndCredentials(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, _, actor := bootstrapUserTest(t, ctx, repository, "hydration")
	firstTeam := createUserTestTeam(t, ctx, repository, actor, "platform")
	secondTeam := createUserTestTeam(t, ctx, repository, actor, "payments")

	added, err := repository.AddTeamMemberByEmail(ctx, AddTeamMemberByEmailInput{
		TeamID: firstTeam.ID, Email: "New.Person@Example.com", Role: MembershipRoleMember, Actor: actor,
	})
	if err != nil || !added.CreatedUser || added.User.DisplayName != "New.Person" || added.User.Status != UserStatusInvited {
		t.Fatalf("AddTeamMemberByEmail = %#v, %v", added, err)
	}
	duplicate, err := repository.AddTeamMemberByEmail(ctx, AddTeamMemberByEmailInput{
		TeamID: firstTeam.ID, Email: "new.person@example.COM", Role: MembershipRoleMember, Actor: actor,
	})
	if err != nil || duplicate.CreatedUser || duplicate.Changed {
		t.Fatalf("idempotent add by email = %#v, %v", duplicate, err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: secondTeam.ID, UserID: added.User.ID, Role: MembershipRoleManager, Actor: actor}); err != nil {
		t.Fatalf("AddTeamMember second team: %v", err)
	}
	firstCredential, err := repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: added.User.ID, Name: "First", Actor: actor})
	if err != nil {
		t.Fatalf("issue first credential: %v", err)
	}
	if _, err := repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: added.User.ID, Name: "Second", Actor: actor}); err != nil {
		t.Fatalf("issue second credential: %v", err)
	}
	user, err := repository.GetUser(ctx, added.User.ID)
	if err != nil || len(user.Memberships) != 2 || len(user.Credentials) != 2 {
		t.Fatalf("hydrated user = %#v, %v", user, err)
	}
	for _, credential := range user.Credentials {
		if credential.KeyPrefix == firstCredential.RawKey || strings.Contains(credential.Name, firstCredential.RawKey) {
			t.Fatal("hydrated credential exposed raw key")
		}
	}
	users, total, err := repository.ListUsers(ctx, UserListOptions{Search: "new.person", Limit: 10, IncludeChildren: true})
	if err != nil || total != 1 || len(users) != 1 || len(users[0].Memberships) != 2 || len(users[0].Credentials) != 2 {
		t.Fatalf("ListUsers hydration = %#v/%d, %v", users, total, err)
	}
	members, memberTotal, err := repository.ListTeamMembers(ctx, firstTeam.ID, UserListOptions{Limit: 10})
	if err != nil || memberTotal != 1 || len(members) != 1 || members[0].UserID != added.User.ID || members[0].MembershipRole != MembershipRoleMember {
		t.Fatalf("ListTeamMembers = %#v/%d, %v", members, memberTotal, err)
	}
}

func TestPostgresAdminCannotMutateOwnerPrincipalsOrCreateOwnerAccess(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, _, ownerActor := bootstrapUserTest(t, ctx, repository, "owner-protection")

	adminUser := createActiveUserWithCredential(t, ctx, repository, ownerActor, "admin-protection@example.com")
	adminTeam := createUserTestTeam(t, ctx, repository, ownerActor, "admin-protection")
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{
		TeamID: adminTeam.ID, RoleSlug: accesscontrol.RoleAdmin,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: ownerActor,
	}); err != nil {
		t.Fatalf("grant Admin team: %v", err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: adminTeam.ID, UserID: adminUser.UserID, Role: MembershipRoleMember, Actor: ownerActor}); err != nil {
		t.Fatalf("add Admin member: %v", err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(adminUser.RawKey))
	if err != nil {
		t.Fatalf("load Admin principal: %v", err)
	}
	adminActor := MutationActor{SubjectID: principal.SubjectID, CredentialID: principal.CredentialID, RequestID: "admin-owner-protection"}

	ownerTeam := createUserTestTeam(t, ctx, repository, ownerActor, "protected-owners")
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{
		TeamID: ownerTeam.ID, RoleSlug: accesscontrol.RoleOwner,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: ownerActor,
	}); err != nil {
		t.Fatalf("grant Owner team: %v", err)
	}
	ownerUser := createActiveUserWithCredential(t, ctx, repository, ownerActor, "protected-owner@example.com")
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: ownerTeam.ID, UserID: ownerUser.UserID, Role: MembershipRoleMember, Actor: ownerActor}); err != nil {
		t.Fatalf("add Owner member: %v", err)
	}

	ordinaryTeam := createUserTestTeam(t, ctx, repository, adminActor, "admin-managed")
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{
		TeamID: ordinaryTeam.ID, RoleSlug: accesscontrol.RoleBuilder,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: adminActor,
	}); err != nil {
		t.Fatalf("Admin should assign non-Owner role: %v", err)
	}
	if _, err := repository.AddTeamMemberByEmail(ctx, AddTeamMemberByEmailInput{TeamID: ordinaryTeam.ID, Email: "ordinary@example.com", Role: MembershipRoleMember, Actor: adminActor}); err != nil {
		t.Fatalf("Admin should manage ordinary team members: %v", err)
	}

	name := "forged-owner-team"
	_, err = repository.UpdateTeam(ctx, ownerTeam.ID, TeamPatch{Name: &name, Actor: adminActor})
	assertOwnerManagementDenied(t, err)
	_, err = repository.AddTeamBinding(ctx, TeamBindingMutation{
		TeamID: ordinaryTeam.ID, RoleSlug: accesscontrol.RoleOwner,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: adminActor,
	})
	assertOwnerManagementDenied(t, err)
	_, err = repository.AddTeamMemberByEmail(ctx, AddTeamMemberByEmailInput{TeamID: ownerTeam.ID, Email: "forged-owner@example.com", Role: MembershipRoleMember, Actor: adminActor})
	assertOwnerManagementDenied(t, err)
	_, err = repository.SuspendUser(ctx, ownerUser.UserID, adminActor)
	assertOwnerManagementDenied(t, err)
	_, err = repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: ownerUser.UserID, Name: "Forged", Actor: adminActor})
	assertOwnerManagementDenied(t, err)

	if _, err := repository.SuspendUser(ctx, ownerUser.UserID, ownerActor); err != nil {
		t.Fatalf("Owner suspends protected user: %v", err)
	}
	suspendedOwner, err := repository.GetUser(ctx, ownerUser.UserID)
	if err != nil || !suspendedOwner.OwnerProtected {
		t.Fatalf("suspended Owner protection = %t, %v", suspendedOwner.OwnerProtected, err)
	}
	if grants, _, err := repository.GetUserEffectiveAccess(ctx, ownerUser.UserID); err != nil || len(grants) != 0 {
		t.Fatalf("suspended Owner effective grants = %#v, %v", grants, err)
	}

	var forgedUsers int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_users WHERE email_normalized = 'forged-owner@example.com'`).Scan(&forgedUsers); err != nil || forgedUsers != 0 {
		t.Fatalf("denied Owner-team invite persisted %d user(s): %v", forgedUsers, err)
	}
}

func assertOwnerManagementDenied(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrOwnerManagementForbidden) {
		t.Fatalf("Owner-protected mutation error = %v, want ErrOwnerManagementForbidden", err)
	}
}

func TestPostgresUserHydrationBoundsNestedCollections(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, _, actor := bootstrapUserTest(t, ctx, repository, "bounded-hydration")
	created, err := repository.CreateUser(ctx, CreateUserInput{
		Email: "bounded@example.com", DisplayName: "Bounded", Actor: actor,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		WITH inserted_teams AS (
			INSERT INTO fused_teams (name, slug)
			SELECT 'Bounded Team ' || value, 'bounded-team-' || value
			FROM generate_series(1, 101) value
			RETURNING id
		)
		INSERT INTO fused_team_memberships (team_id, member_subject_id, created_by_subject_id)
		SELECT id, $1, $2 FROM inserted_teams
	`, created.User.ID, actor.SubjectID); err != nil {
		t.Fatalf("insert memberships: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_control_credentials (subject_id, key_hash, key_prefix, name)
		SELECT $1, 'bounded-hash-' || value, 'fsk_bound', 'Bounded Credential ' || value
		FROM generate_series(1, 101) value
	`, created.User.ID); err != nil {
		t.Fatalf("insert credentials: %v", err)
	}

	user, err := repository.GetUser(ctx, created.User.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if len(user.Memberships) != 100 || !user.MembershipsTruncated {
		t.Fatalf("memberships = %d/truncated=%t, want 100/true", len(user.Memberships), user.MembershipsTruncated)
	}
	if len(user.Credentials) != 100 || !user.CredentialsTruncated {
		t.Fatalf("credentials = %d/truncated=%t, want 100/true", len(user.Credentials), user.CredentialsTruncated)
	}
	users, total, err := repository.ListUsers(ctx, UserListOptions{Search: "bounded@example.com", Limit: 1, IncludeChildren: true})
	if err != nil || total != 1 || len(users) != 1 {
		t.Fatalf("ListUsers = %d/%d, %v", len(users), total, err)
	}
	if len(users[0].Memberships) != 100 || !users[0].MembershipsTruncated || len(users[0].Credentials) != 100 || !users[0].CredentialsTruncated {
		t.Fatalf("bounded ListUsers hydration = %#v", users[0])
	}
	summaries, total, err := repository.ListUsers(ctx, UserListOptions{Search: "bounded@example.com", Limit: 1})
	if err != nil || total != 1 || len(summaries) != 1 {
		t.Fatalf("summary ListUsers = %d/%d, %v", len(summaries), total, err)
	}
	if len(summaries[0].Memberships) != 0 || summaries[0].MembershipsTruncated || len(summaries[0].Credentials) != 0 || summaries[0].CredentialsTruncated {
		t.Fatalf("summary unexpectedly hydrated children = %#v", summaries[0])
	}
}

func TestPostgresTeamMembershipUnionAndRemovalInvalidatesImmediately(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, _, actor := bootstrapUserTest(t, ctx, repository, "union")
	firstTeam := createUserTestTeam(t, ctx, repository, actor, "union-a")
	secondTeam := createUserTestTeam(t, ctx, repository, actor, "union-b")
	serviceID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_slug, service_name) VALUES ($1, $2, 'Union Service')`, serviceID, "union-"+serviceID.String()); err != nil {
		t.Fatalf("insert union service: %v", err)
	}
	for _, team := range []Team{firstTeam, secondTeam} {
		if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: team.ID, RoleSlug: accesscontrol.RoleServiceUser, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, Actor: actor}); err != nil {
			t.Fatalf("grant service to %s: %v", team.Slug, err)
		}
	}
	user := createActiveUserWithCredential(t, ctx, repository, actor, "union@example.com")
	for _, team := range []Team{firstTeam, secondTeam} {
		if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: team.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); err != nil {
			t.Fatalf("add union member: %v", err)
		}
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(user.RawKey))
	if err != nil || !principalHasGrant(principal, accesscontrol.PermissionServiceConsume, serviceID) {
		t.Fatalf("union principal = %#v, %v", principal.EffectiveGrants, err)
	}
	access, _, err := repository.GetUserEffectiveAccess(ctx, user.UserID)
	if err != nil || countAccessSources(access, accesscontrol.PermissionServiceConsume, serviceID) != 2 {
		t.Fatalf("effective access provenance = %#v, %v", access, err)
	}
	if _, err := repository.RemoveTeamMember(ctx, firstTeam.ID, user.UserID, actor); err != nil {
		t.Fatalf("remove first membership: %v", err)
	}
	principal, err = repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(user.RawKey))
	if err != nil || !principalHasGrant(principal, accesscontrol.PermissionServiceConsume, serviceID) {
		t.Fatalf("remaining team grant = %#v, %v", principal.EffectiveGrants, err)
	}
	removed, err := repository.RemoveTeamMember(ctx, secondTeam.ID, user.UserID, actor)
	if err != nil {
		t.Fatalf("remove final membership: %v", err)
	}
	principal, err = repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(user.RawKey))
	if err != nil || principal.Revision != removed.AuthorizationRevision || principalHasGrant(principal, accesscontrol.PermissionServiceConsume, serviceID) {
		t.Fatalf("final removal principal = revision %d grants %#v, %v", principal.Revision, principal.EffectiveGrants, err)
	}
	_ = workspaceID
}

func TestPostgresMembershipStatusRulesRejectArchivedAndKeepSuspendedInert(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, _, actor := bootstrapUserTest(t, ctx, repository, "membership-status")
	activeTeam := createUserTestTeam(t, ctx, repository, actor, "status-active")
	archivedTeam := createUserTestTeam(t, ctx, repository, actor, "status-archived")
	if _, err := repository.ArchiveTeam(ctx, archivedTeam.ID, actor); err != nil {
		t.Fatalf("archive status team: %v", err)
	}
	user := createActiveUserWithCredential(t, ctx, repository, actor, "status-user@example.com")
	if _, err := repository.SuspendUser(ctx, user.UserID, actor); err != nil {
		t.Fatalf("suspend status user: %v", err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: activeTeam.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); err != nil {
		t.Fatalf("add suspended user to active team: %v", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(user.RawKey)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("suspended member authentication error = %v", err)
	}
	if _, err := repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: user.UserID, Name: "Blocked", Actor: actor}); !errors.Is(err, ErrInvalidControlCredential) {
		t.Fatalf("suspended credential issue error = %v, want ErrInvalidControlCredential", err)
	}
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: archivedTeam.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); !errors.Is(err, ErrTeamArchived) {
		t.Fatalf("archived team membership error = %v, want ErrTeamArchived", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE fused_subjects SET status = 'archived' WHERE id = $1`, user.UserID); err != nil {
		t.Fatalf("archive user fixture: %v", err)
	}
	secondTeam := createUserTestTeam(t, ctx, repository, actor, "status-second")
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: secondTeam.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); !errors.Is(err, ErrUserArchived) {
		t.Fatalf("archived user membership error = %v, want ErrUserArchived", err)
	}
}

func TestPostgresLastEffectiveOwnerInvariantSerializesConcurrentSuspension(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, owner, actor := bootstrapUserTest(t, ctx, repository, "owner-concurrency")
	ownerTeam := createUserTestTeam(t, ctx, repository, actor, "owners")
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: ownerTeam.ID, RoleSlug: accesscontrol.RoleOwner, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor}); err != nil {
		t.Fatalf("grant Owner team: %v", err)
	}
	first := createActiveUserWithCredential(t, ctx, repository, actor, "owner-one@example.com")
	second := createActiveUserWithCredential(t, ctx, repository, actor, "owner-two@example.com")
	for _, user := range []issuedUser{first, second} {
		if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: ownerTeam.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); err != nil {
			t.Fatalf("add Owner member: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM fused_role_bindings binding USING fused_roles role WHERE binding.role_id = role.id AND binding.subject_type = 'subject' AND binding.subject_id = $1 AND role.slug = 'owner'`, owner.SubjectID); err != nil {
		t.Fatalf("remove bootstrap Owner fixture binding: %v", err)
	}
	grantFixtureAccountManager(t, ctx, pool, owner.SubjectID, workspaceID)

	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, userID := range []uuid.UUID{first.UserID, second.UserID} {
		wait.Add(1)
		go func(id uuid.UUID) {
			defer wait.Done()
			_, err := repository.SuspendUser(context.Background(), id, actor)
			results <- err
		}(userID)
	}
	wait.Wait()
	close(results)
	var succeeded, protected int
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrLastEffectiveOwner) {
			protected++
		} else {
			t.Fatalf("unexpected concurrent suspension error: %v", err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("concurrent suspension outcomes = %d success/%d protected", succeeded, protected)
	}
	var activeOwners int
	if err := pool.QueryRow(ctx, effectiveOwnerCountSQL).Scan(&activeOwners); err != nil || activeOwners != 1 {
		t.Fatalf("effective Owner count = %d, %v", activeOwners, err)
	}
}

func TestPostgresLastEffectiveOwnerInvariantSerializesMembershipRemovalAgainstSuspension(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, owner, actor := bootstrapUserTest(t, ctx, repository, "owner-mixed")
	ownerTeam := createUserTestTeam(t, ctx, repository, actor, "mixed-owners")
	if _, err := repository.AddTeamBinding(ctx, TeamBindingMutation{TeamID: ownerTeam.ID, RoleSlug: accesscontrol.RoleOwner, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor}); err != nil {
		t.Fatalf("grant mixed Owner team: %v", err)
	}
	first := createActiveUserWithCredential(t, ctx, repository, actor, "mixed-one@example.com")
	second := createActiveUserWithCredential(t, ctx, repository, actor, "mixed-two@example.com")
	for _, user := range []issuedUser{first, second} {
		if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: ownerTeam.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); err != nil {
			t.Fatalf("add mixed Owner member: %v", err)
		}
	}
	removeBootstrapOwnerBinding(t, ctx, pool, owner.SubjectID)
	grantFixtureAccountManager(t, ctx, pool, owner.SubjectID, workspaceID)

	results := make(chan error, 2)
	go func() {
		_, err := repository.SuspendUser(context.Background(), first.UserID, actor)
		results <- err
	}()
	go func() {
		_, err := repository.RemoveTeamMember(context.Background(), ownerTeam.ID, second.UserID, actor)
		results <- err
	}()
	var succeeded, protected int
	for range 2 {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrLastEffectiveOwner) {
			protected++
		} else {
			t.Fatalf("unexpected mixed Owner mutation error: %v", err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("mixed Owner outcomes = %d success/%d protected", succeeded, protected)
	}
	var activeOwners int
	if err := pool.QueryRow(ctx, effectiveOwnerCountSQL).Scan(&activeOwners); err != nil || activeOwners != 1 {
		t.Fatalf("mixed effective Owner count = %d, %v", activeOwners, err)
	}
}

func TestPostgresLastEffectiveOwnerBlocksTeamRoleClearAndDowngrade(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, owner, actor := bootstrapUserTest(t, ctx, repository, "owner-team-role")
	ownerTeam := createUserTestTeam(t, ctx, repository, actor, "sole-owner-team")
	ownerBinding := TeamBindingMutation{TeamID: ownerTeam.ID, RoleSlug: accesscontrol.RoleOwner, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor}
	if _, err := repository.AddTeamBinding(ctx, ownerBinding); err != nil {
		t.Fatalf("grant sole Owner team: %v", err)
	}
	user := createActiveUserWithCredential(t, ctx, repository, actor, "sole-owner@example.com")
	if _, err := repository.AddTeamMember(ctx, TeamMemberMutation{TeamID: ownerTeam.ID, UserID: user.UserID, Role: MembershipRoleMember, Actor: actor}); err != nil {
		t.Fatalf("add sole Owner member: %v", err)
	}
	removeBootstrapOwnerBinding(t, ctx, pool, owner.SubjectID)
	grantFixtureAccountManager(t, ctx, pool, owner.SubjectID, workspaceID)

	if _, err := repository.ClearTeamWorkspaceRole(ctx, ownerTeam.ID, workspaceID, actor); !errors.Is(err, ErrLastEffectiveOwner) {
		t.Fatalf("clear sole Owner role error = %v, want ErrLastEffectiveOwner", err)
	}
	downgrade := ownerBinding
	downgrade.RoleSlug = accesscontrol.RoleViewer
	if _, err := repository.AddTeamBinding(ctx, downgrade); !errors.Is(err, ErrLastEffectiveOwner) {
		t.Fatalf("downgrade sole Owner role error = %v, want ErrLastEffectiveOwner", err)
	}
	assertSingleTeamResourceBinding(t, pool, ownerTeam.ID, workspaceID, accesscontrol.RoleOwner)
}

type issuedUser struct {
	UserID uuid.UUID
	RawKey string
}

func bootstrapUserTest(t *testing.T, ctx context.Context, repository *postgresStore, suffix string) (uuid.UUID, accesscontrol.BootstrapResult, MutationActor) {
	t.Helper()
	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "User Test "+suffix)
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, accountID, "fsk_user_test_"+suffix)
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	actor := MutationActor{SubjectID: owner.SubjectID, CredentialID: owner.CredentialID, RequestID: "request-" + suffix, TraceID: "trace-" + suffix}
	return workspaceID, owner, actor
}

func createUserTestTeam(t *testing.T, ctx context.Context, repository *postgresStore, actor MutationActor, slug string) Team {
	t.Helper()
	result, err := repository.CreateTeam(ctx, TeamMutation{Name: slug, Slug: slug, Actor: actor})
	if err != nil {
		t.Fatalf("CreateTeam(%s): %v", slug, err)
	}
	return result.Team
}

func createActiveUserWithCredential(t *testing.T, ctx context.Context, repository *postgresStore, actor MutationActor, email string) issuedUser {
	t.Helper()
	created, err := repository.CreateUser(ctx, CreateUserInput{Email: email, DisplayName: strings.Split(email, "@")[0], Actor: actor})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	issued, err := repository.IssueUserControlCredential(ctx, IssueCredentialInput{UserID: created.User.ID, Name: "Test", Actor: actor})
	if err != nil {
		t.Fatalf("IssueUserControlCredential(%s): %v", email, err)
	}
	return issuedUser{UserID: created.User.ID, RawKey: issued.RawKey}
}

func removeBootstrapOwnerBinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subjectID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM fused_role_bindings binding USING fused_roles role WHERE binding.role_id = role.id AND binding.subject_type = 'subject' AND binding.subject_id = $1 AND role.slug = 'owner'`, subjectID); err != nil {
		t.Fatalf("remove bootstrap Owner fixture binding: %v", err)
	}
}

func grantFixtureAccountManager(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subjectID, workspaceID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		WITH role AS (
			INSERT INTO fused_roles (slug, display_name, scope_type)
			VALUES ('test-account-manager', 'Test account manager', 'workspace')
			ON CONFLICT (slug) DO UPDATE SET display_name = EXCLUDED.display_name
			RETURNING id
		), permission AS (
			INSERT INTO fused_role_permissions (role_id, permission)
			SELECT id, 'account.manage' FROM role ON CONFLICT DO NOTHING
		)
		INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id)
		SELECT 'subject', $1, id, 'workspace', $2 FROM role
		ON CONFLICT DO NOTHING
	`, subjectID, workspaceID)
	if err != nil {
		t.Fatalf("grant fixture account manager: %v", err)
	}
}

func assertRawCredentialNotPersisted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issued IssuedControlCredential) {
	t.Helper()
	var hash, prefix string
	if err := pool.QueryRow(ctx, `SELECT key_hash, key_prefix FROM fused_control_credentials WHERE id = $1`, issued.Credential.ID).Scan(&hash, &prefix); err != nil {
		t.Fatalf("load stored credential: %v", err)
	}
	if hash == issued.RawKey || hash != accesscontrol.HashControlCredential(issued.RawKey) || prefix != issued.Credential.KeyPrefix {
		t.Fatal("raw credential was not reduced to hash and safe prefix")
	}
	var leaked bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fused_audit_events WHERE metadata::text LIKE '%' || $1 || '%' OR action LIKE '%' || $1 || '%' OR trace_id LIKE '%' || $1 || '%')`, issued.RawKey).Scan(&leaked); err != nil {
		t.Fatalf("search audit for raw credential: %v", err)
	}
	if leaked {
		t.Fatal("raw credential leaked into audit")
	}
}

func assertRawCredentialNotTraced(t *testing.T, recorder *tracetest.SpanRecorder, rawKey string) {
	t.Helper()
	for _, span := range recorder.Ended() {
		values := []string{span.Name(), span.Status().Description}
		for _, attr := range span.Attributes() {
			values = append(values, string(attr.Key), fmt.Sprint(attr.Value.AsInterface()))
		}
		for _, event := range span.Events() {
			values = append(values, event.Name)
			for _, attr := range event.Attributes {
				values = append(values, string(attr.Key), fmt.Sprint(attr.Value.AsInterface()))
			}
		}
		if strings.Contains(strings.Join(values, " "), rawKey) {
			t.Fatalf("raw credential leaked into OTEL span %q", span.Name())
		}
	}
}

func assertUserAuditContext(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID, actor MutationActor, userID, credentialID uuid.UUID) {
	t.Helper()
	var subjectID, actorCredentialID, resourceID uuid.UUID
	var requestID, traceID, auditUserID, auditCredentialID string
	if err := pool.QueryRow(ctx, `
		SELECT actor_subject_id, actor_credential_id, resource_id, request_id, trace_id,
			metadata->>'user_id', metadata->>'credential_id'
		FROM fused_audit_events WHERE action = 'user.credential.issue' ORDER BY occurred_at DESC LIMIT 1
	`).Scan(&subjectID, &actorCredentialID, &resourceID, &requestID, &traceID, &auditUserID, &auditCredentialID); err != nil {
		t.Fatalf("load credential audit: %v", err)
	}
	if subjectID != actor.SubjectID || actorCredentialID != actor.CredentialID || resourceID != workspaceID || requestID != actor.RequestID || traceID != actor.TraceID || auditUserID != userID.String() || auditCredentialID != credentialID.String() {
		t.Fatal("credential audit actor/request/resource identity mismatch")
	}
}

func countAccessSources(access []EffectiveAccessGrant, permission accesscontrol.Permission, resourceID uuid.UUID) int {
	count := 0
	for _, grant := range access {
		if grant.Permission == permission && grant.Resource.ID == resourceID {
			count++
		}
	}
	return count
}
