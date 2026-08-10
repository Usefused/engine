package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/capability"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplyAppConfigPlanConcurrentWinnerKeepsRuntimeAndToken(t *testing.T) {
	for _, configType := range []ConfigType{ConfigTypeSDK, ConfigTypeMCP} {
		t.Run(string(configType), func(t *testing.T) {
			fixture := newConcurrentArtifactApplyFixture(t, configType)
			start := make(chan struct{})
			errorsByApply := make([]error, 2)
			var wait sync.WaitGroup
			for index := range errorsByApply {
				wait.Add(1)
				go func(index int) {
					defer wait.Done()
					<-start
					params := fixture.params
					params.TokenHash = fmt.Sprintf("token-hash-%d-%s", index, uuid.NewString())
					_, errorsByApply[index] = fixture.repository.ApplyAppConfigPlan(fixture.ctx, params)
				}(index)
			}
			close(start)
			wait.Wait()

			succeeded := 0
			for _, err := range errorsByApply {
				if err == nil {
					succeeded++
					continue
				}
				if !errors.Is(err, ErrConfigPlanNotFound) && !errors.Is(err, ErrConfigPlanRevisionMismatch) && !strings.Contains(err.Error(), "config generation changed") {
					t.Fatalf("concurrent loser error = %v", err)
				}
			}
			if succeeded != 1 {
				t.Fatalf("successful applies = %d, errors=%v", succeeded, errorsByApply)
			}
			assertAtomicArtifactApplyState(t, fixture)
		})
	}
}

func TestApplyAppConfigPlanUsesAuthorizationBeforeTeamLockOrder(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	blocker, err := fixture.repository.db.Begin(fixture.ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(fixture.ctx)
	if _, err := lockAuthorizationState(fixture.ctx, blocker); err != nil {
		t.Fatalf("lock authorization state: %v", err)
	}
	params := fixture.params
	params.TokenHash = "lock-order-" + uuid.NewString()
	done := make(chan error, 1)
	go func() {
		_, applyErr := fixture.repository.ApplyAppConfigPlan(fixture.ctx, params)
		done <- applyErr
	}()
	// If apply ever locks the team before authorization again, this query and
	// the apply form the inverse wait cycle that previously deadlocked.
	time.Sleep(75 * time.Millisecond)
	teamCtx, cancel := context.WithTimeout(fixture.ctx, 500*time.Millisecond)
	defer cancel()
	var status TeamStatus
	if err := blocker.QueryRow(teamCtx, `SELECT status FROM fused_teams WHERE id = $1 FOR UPDATE`, params.Scope.OwnerTeamID).Scan(&status); err != nil {
		t.Fatalf("team lock blocked behind apply's authorization wait: %v", err)
	}
	if err := blocker.Commit(fixture.ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("apply after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("artifact apply did not finish after ordered locks were released")
	}
}

func TestApplyAppConfigPlanRejectsSDKWithoutPlanLease(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	fixture.params.Plan.ApplyLeaseID = uuid.Nil
	fixture.params.TokenHash = "missing-lease-" + uuid.NewString()

	_, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	if !errors.Is(err, ErrConfigPlanApplyInProgress) {
		t.Fatalf("SDK apply without lease error = %v, want ErrConfigPlanApplyInProgress", err)
	}
}

func TestApplyAppConfigPlanRejectsSameNameBucketReplacementAtomically(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	replacementID := uuid.New()
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM fused_buckets WHERE id = $1`, fixture.params.Scope.BucketID); err != nil {
		t.Fatalf("remove authorized bucket: %v", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, replacementID, fixture.params.AuthorizedBucketName); err != nil {
		t.Fatalf("seed same-name replacement: %v", err)
	}

	fixture.params.TokenHash = "replacement-race-" + uuid.NewString()
	_, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	if !errors.Is(err, ErrSDKBucketImmutable) {
		t.Fatalf("ApplyAppConfigPlan(stale bucket) = %v, want ErrSDKBucketImmutable", err)
	}
	assertRejectedArtifactApplyState(t, fixture, replacementID)
}

func assertRejectedArtifactApplyState(t *testing.T, fixture concurrentArtifactApplyFixture, replacementID uuid.UUID) {
	t.Helper()
	var scopes, tokens, states, applied, replacement int
	checks := []struct {
		query string
		arg   any
		out   *int
	}{
		{`SELECT COUNT(*) FROM fused_apps WHERE app_id = $1`, fixture.params.Scope.AppID, &scopes},
		{`SELECT COUNT(*) FROM fused_app_tokens token JOIN fused_apps app ON app.app_family_id = token.app_family_id WHERE app.app_id = $1`, fixture.params.Scope.AppID, &tokens},
		{`SELECT COUNT(*) FROM fused_config_states WHERE latest_resource_id = $1`, fixture.params.Scope.AppID, &states},
		{`SELECT COUNT(*) FROM fused_config_plans WHERE id = $1 AND status = 'applied'`, fixture.params.Plan.PlanID, &applied},
		{`SELECT COUNT(*) FROM fused_buckets WHERE id = $1`, replacementID, &replacement},
	}
	for _, check := range checks {
		if err := fixture.pool.QueryRow(fixture.ctx, check.query, check.arg).Scan(check.out); err != nil {
			t.Fatalf("verify rejected apply: %v", err)
		}
	}
	if scopes != 0 || tokens != 0 || states != 0 || applied != 0 || replacement != 1 {
		t.Fatalf("rejected apply left partial state scopes=%d tokens=%d states=%d applied=%d replacement=%d", scopes, tokens, states, applied, replacement)
	}
}

type concurrentArtifactApplyFixture struct {
	ctx        context.Context
	repository *postgresConfigRepository
	pool       *pgxpool.Pool
	params     ApplyAppConfigPlanParams
}

func newConcurrentArtifactApplyFixture(t *testing.T, configType ConfigType) concurrentArtifactApplyFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL is required for artifact apply integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM fused_workspaces`); err != nil {
		t.Fatalf("clean workspace: %v", err)
	}
	accountID := uuid.New()
	accessRepository := NewPostgresStore(pool).(*postgresStore)
	if _, err := accessRepository.BootstrapWorkspace(ctx, accountID, "Artifact Apply Workspace"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	if _, err := accesscontrol.BootstrapOwner(ctx, accessRepository, accountID, "fsk_artifact_apply_"+uuid.NewString()); err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	ownerTeamID := seedAppOwnerTeam(t, ctx, pool)
	bucketID := uuid.New()
	bucketName := "apply-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, bucketName); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	repository := NewPostgresConfigRepository(pool).(*postgresConfigRepository)
	configKey := string(configType) + ":concurrent:" + uuid.NewString()
	required, err := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionAppCreate,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: uuid.New()},
	}})
	if err != nil {
		t.Fatalf("required permissions: %v", err)
	}
	plan, err := repository.CreateConfigPlan(ctx, CreateConfigPlanParams{
		ConfigKey: configKey, ConfigType: configType, OwnerTeamID: &ownerTeamID,
		SourceHash: "source", BaseGeneration: 0, Actions: []byte("[]"), DesiredState: []byte("{}"),
		ResolvedPayload: []byte("{}"), Blockers: []byte("[]"), Warnings: []byte("[]"),
		RequiredPermissions: required, CreatedBy: accountID,
	})
	if err != nil {
		t.Fatalf("CreateConfigPlan: %v", err)
	}
	lease, err := repository.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
	if err != nil {
		t.Fatalf("ReserveConfigPlanApply: %v", err)
	}
	appID := uuid.New()
	generatorVersion := ""
	targetLanguage := ""
	if configType == ConfigTypeSDK {
		generatorVersion = "registry-generator-v1"
		targetLanguage = "typescript"
	}
	version := uuid.NewString()
	return concurrentArtifactApplyFixture{
		ctx: ctx, repository: repository, pool: pool,
		params: ApplyAppConfigPlanParams{
			Plan: ApplyConfigPlanParams{
				State:  UpsertConfigStateParams{ConfigKey: configKey, ConfigType: configType, SourceHash: "source", DesiredState: []byte("{}"), ManagedResources: []byte("{}"), LatestResourceID: &appID, UpdatedBy: accountID},
				PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision, ApplyLeaseID: lease.ID,
			},
			Scope: AppRuntime{AccountID: accountID, AppID: appID, OwnerTeamID: ownerTeamID,
				BucketID: bucketID, Selections: []byte("[]"), ScopeSchemaVersion: 2,
				Kind: AppKind(configType), Name: "concurrent", Version: version, ConfigKey: configKey},
			AuthorizedBucketName: bucketName,
			TokenName:            "default", TargetLanguage: targetLanguage,
			GeneratorVersion: generatorVersion,
		},
	}
}

func assertAtomicArtifactApplyState(t *testing.T, fixture concurrentArtifactApplyFixture) {
	t.Helper()
	var scopes, tokens, states, applied int
	queries := []struct {
		query string
		arg   any
		out   *int
	}{
		{`SELECT COUNT(*) FROM fused_apps WHERE app_id = $1`, fixture.params.Scope.AppID, &scopes},
		{`SELECT COUNT(*) FROM fused_app_tokens token JOIN fused_apps app ON app.app_family_id = token.app_family_id WHERE app.app_id = $1`, fixture.params.Scope.AppID, &tokens},
		{`SELECT COUNT(*) FROM fused_config_states WHERE latest_resource_id = $1`, fixture.params.Scope.AppID, &states},
		{`SELECT COUNT(*) FROM fused_config_plans WHERE id = $1 AND status = 'applied'`, fixture.params.Plan.PlanID, &applied},
	}
	for _, query := range queries {
		if err := fixture.pool.QueryRow(fixture.ctx, query.query, query.arg).Scan(query.out); err != nil {
			t.Fatalf("verify atomic state: %v", err)
		}
	}
	if scopes != 1 || tokens != 1 || states != 1 || applied != 1 {
		t.Fatalf("atomic artifact state scopes=%d tokens=%d states=%d applied=%d", scopes, tokens, states, applied)
	}
	assertCanonicalCapabilityHash(t, fixture)
}

func assertCanonicalCapabilityHash(t *testing.T, fixture concurrentArtifactApplyFixture) {
	t.Helper()
	_, expected, err := capability.KeysAndHash(fixture.params.Scope.Selections)
	if err != nil {
		t.Fatalf("canonical capability hash: %v", err)
	}
	var persisted string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT capability_hash FROM fused_apps WHERE app_id = $1`, fixture.params.Scope.AppID).Scan(&persisted); err != nil {
		t.Fatalf("read persisted capability hash: %v", err)
	}
	if persisted != expected {
		t.Fatalf("capability hash = %q, want canonical %q", persisted, expected)
	}
}
