package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestTypedAppOAuthScopePostgres checks real principal loading, cached consent, family lookup and paginated catalogue filtering.
func TestTypedAppOAuthScopePostgres(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	workspaceID, owner, mutationActor := bootstrapUserTest(t, ctx, repository, "typed-app-scopes")
	ownerCredential := "fsk_user_test_typed-app-scopes"
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(ownerCredential))
	// Use only the fixture account, never a developer's existing workspace.
	if err != nil {
		t.Fatal(err)
	}
	families := make(map[string]uuid.UUID)
	for _, appType := range []string{"sdk", "api", "mcp"} {
		id := uuid.New()
		kind, mode, language := "sdk", any(appType), any("typescript")
		// MCP has a different persisted adapter; API remains SDK plus delivery mode.
		if appType == "mcp" {
			kind, mode, language = "mcp", nil, nil
		}
		_, err := pool.Exec(ctx, `INSERT INTO fused_app_families
			(app_family_id,account_id,kind,delivery_mode,target_language,canonical_name,display_name,owner_subject_id)
			VALUES ($1,$2,$3,$4,$5,$6,$6,$7)`, id, principal.AccountID, kind, mode, language, "typed-"+appType, owner.SubjectID)
		// Each row has distinct durable type evidence before authorization is evaluated.
		if err != nil {
			t.Fatal(err)
		}
		families[appType] = id
	}
	client, _ := oauthTestClient(t, ctx, repository, mutationActor, "https://app.example.com/callback", []string{"app.mcp.read", "app.mcp.create", "app.mcp.manage", "app.mcp.tokens.manage"})
	now := time.Now().UTC()
	code, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, client.RedirectURIs[0], client.AllowedScopes, now.Add(time.Minute))
	issue, narrowCredential, _ := oauthTestTokenIssue(now)
	_, err = repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(code), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeVerifier: verifier, Issue: issue}, now)
	// The delegated token must be minted by the real consent/code exchange path.
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := accesscontrol.NewAuthenticator(repository, 1, accesscontrol.AuthenticatorOptions{})
	// Cache assertions require the real store-backed authenticator.
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.AuthenticateControlCredential(ctx, narrowCredential)
	// Seed the narrow snapshot before any broad identity enters the cache.
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.AuthenticateControlCredential(ctx, ownerCredential)
	// Warming the broader credential must not replace the narrow credential's snapshot.
	if err != nil {
		t.Fatal(err)
	}
	actor, err := authenticator.AuthenticateControlCredential(ctx, narrowCredential)
	// Exercise the cache hit after the Owner credential has authenticated.
	if err != nil {
		t.Fatal(err)
	}
	authorizer := accesscontrol.SnapshotAuthorizer{}
	for appType, id := range families {
		for _, action := range []accesscontrol.Permission{accesscontrol.PermissionAppRead, accesscontrol.PermissionAppManage, accesscontrol.PermissionAppTokensManage} {
			err := authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{Permission: action, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: id}})
			// Generic internal detail routes must resolve the stored type before checking consent.
			if (err == nil) != (appType == "mcp") {
				t.Errorf("%s %s: %v", action, appType, err)
			}
		}
	}
	for _, appType := range []string{"sdk", "api", "mcp", "webhook"} {
		err := authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.AppPermission(appType, accesscontrol.PermissionAppCreate), Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
		// Full Owner authority at consent time must not widen the requested creation type.
		if (err == nil) != (appType == "mcp") {
			t.Errorf("create %s: %v", appType, err)
		}
	}
	scope, err := authorizer.Scope(ctx, actor, accesscontrol.PermissionAppRead, accesscontrol.ResourceApp)
	// Mixed listings must pass exact IDs to both pagination and counting.
	if err != nil || scope.All || len(scope.IDs) != 1 || scope.IDs[0] != families["mcp"] {
		t.Fatalf("scope=%#v error=%v", scope, err)
	}
	items, total, err := repository.ListAuthorizedAppFamilies(ctx, principal.AccountID, scope, "", "", false, 20, 0)
	// Row totals must use the same permission filter as the returned page.
	if err != nil || total != 1 || len(items) != 1 || items[0].AppFamilyID != families["mcp"] {
		t.Fatalf("catalogue items=%#v total=%d error=%v", items, total, err)
	}
	_, err = repository.ResolveAppPermission(ctx, uuid.New(), families["mcp"], accesscontrol.PermissionAppRead)
	// A known family UUID cannot cross account boundaries.
	if !errors.Is(err, accesscontrol.ErrPolicyDenied) {
		t.Fatalf("foreign account error=%v", err)
	}
	// A legacy token is rejected rather than silently expanded to every app type.
	_, err = pool.Exec(ctx, `UPDATE fused_oauth_tokens SET scope=ARRAY['app.create']::text[] WHERE access_token_hash=$1`, issue.AccessTokenHash)
	// The negative authentication check is meaningful only after installing the legacy scope.
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.LoadControlPrincipal(ctx, issue.AccessTokenHash)
	// Authentication, rather than a later API check, must reject the retired token.
	if !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("legacy scope authenticated: %v", err)
	}
}
