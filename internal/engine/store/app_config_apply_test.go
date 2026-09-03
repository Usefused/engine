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
	"github.com/Usefused/engine/internal/shared/models"
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

// TestValidateSDKGenerationStateAdmitsDirectAPIWithoutJob protects the package-free state rejected by the live apply path.
func TestValidateSDKGenerationStateAdmitsDirectAPIWithoutJob(t *testing.T) {
	tests := []struct {
		name             string
		status           AppStatus
		jobID            string
		generationStatus string
		wantError        bool
	}{
		{name: "active direct API", status: AppStatusActive, generationStatus: models.SDKGenerationStatusSkipped},
		{name: "deprecated direct API", status: AppStatusDeprecated, generationStatus: models.SDKGenerationStatusSkipped},
		{name: "active direct API with generation job", status: AppStatusActive, jobID: "job-1", generationStatus: models.SDKGenerationStatusSkipped, wantError: true},
		{name: "deprecated direct API with generation job", status: AppStatusDeprecated, jobID: "job-1", generationStatus: models.SDKGenerationStatusSkipped, wantError: true},
		{name: "building direct API", status: AppStatusBuilding, generationStatus: models.SDKGenerationStatusSkipped, wantError: true},
		{name: "complete package without job", status: AppStatusActive, generationStatus: models.SDKGenerationStatusComplete, wantError: true},
		{name: "complete package with job", status: AppStatusActive, jobID: "job-1", generationStatus: models.SDKGenerationStatusComplete},
		{name: "pending package with job", status: AppStatusBuilding, jobID: "job-1", generationStatus: models.SDKGenerationStatusPending},
		{name: "pending runnable package", status: AppStatusActive, jobID: "job-1", generationStatus: models.SDKGenerationStatusPending, wantError: true},
	}
	// Each table row pins one allowed or rejected lifecycle combination at the shared persistence boundary.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSDKGenerationState(test.status, test.jobID, test.generationStatus)
			// Error presence is the observable admission decision; message text remains an implementation detail.
			if (err != nil) != test.wantError {
				t.Fatalf("validateSDKGenerationState(%q, %q, %q) error = %v, wantError=%t", test.status, test.jobID, test.generationStatus, err, test.wantError)
			}
		})
	}
}

// TestApplyAppConfigPlanPersistsDirectAPIWithoutGenerationJob crosses the real transaction boundary that production API init uses.
func TestApplyAppConfigPlanPersistsDirectAPIWithoutGenerationJob(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	fixture.params.SDKGenerationJobID = ""
	fixture.params.SDKGenerationStatus = models.SDKGenerationStatusSkipped
	fixture.params.TokenHash = "direct-api-token-" + uuid.NewString()

	result, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	// Package-free publication must commit even though no Registry generation identity exists.
	if err != nil {
		t.Fatalf("apply direct API: %v", err)
	}
	appStore := NewPostgresStore(fixture.pool).(*postgresStore)
	app, err := appStore.GetApp(fixture.ctx, result.AppID)
	// The persisted marker is the authority used by quotas, runtime reads, and public projections.
	if err != nil {
		t.Fatalf("read direct API: %v", err)
	}
	// A fake job would recreate the competing package lifecycle that direct API mode removes.
	if app.Status != AppStatusActive || app.SDKGenerationStatus != models.SDKGenerationStatusSkipped || app.SDKGenerationJobID != "" {
		t.Fatalf("direct API state = status=%q generation=%q job=%q", app.Status, app.SDKGenerationStatus, app.SDKGenerationJobID)
	}
	family, err := appStore.GetAppFamily(fixture.ctx, result.AppFamilyID)
	// The first package-free apply permanently classifies the logical family even after all versions are deactivated.
	if err != nil || family.DeliveryMode != AppDeliveryModeAPI {
		t.Fatalf("direct API family = %#v, err=%v", family, err)
	}
	opposite := fixture.params
	opposite.SDKGenerationJobID = "job-generated"
	opposite.SDKGenerationStatus = models.SDKGenerationStatusComplete
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatalf("begin opposite-mode probe: %v", err)
	}
	defer tx.Rollback(fixture.ctx)
	_, err = upsertAppFamilyTx(fixture.ctx, tx, opposite)
	// A sibling generated version cannot reuse the direct API family or rewrite its durable delivery class.
	if !errors.Is(err, ErrAppDeliveryModeMismatch) {
		t.Fatalf("opposite delivery mode error = %v, want ErrAppDeliveryModeMismatch", err)
	}
}

// TestSDKGenerationCompletionPromotesDurableBuildingApp exercises the real plan-bound CAS and activation quota transaction.
func TestSDKGenerationCompletionPromotesDurableBuildingApp(t *testing.T) {
	fixture := newConcurrentArtifactApplyFixture(t, ConfigTypeSDK)
	fixture.params.AppStatus = AppStatusBuilding
	fixture.params.SDKGenerationJobID = "job-queued"
	fixture.params.SDKGenerationStatus = models.SDKGenerationStatusPending
	fixture.params.TokenHash = "queued-token-" + uuid.NewString()
	result, err := fixture.repository.ApplyAppConfigPlan(fixture.ctx, fixture.params)
	// Apply must first commit the exact version as non-runnable with its plan and token.
	if err != nil {
		t.Fatalf("apply building SDK: %v", err)
	}
	appStore := NewPostgresStore(fixture.pool).(*postgresStore)
	changed, err := appStore.CompleteSDKGeneration(fixture.ctx, result.AppID, "job-queued", fixture.params.Plan.PlanID.String())
	// The matching latest applied plan is the sole attempt authorized to make the version runnable.
	if err != nil || !changed {
		t.Fatalf("complete SDK generation: changed=%t error=%v", changed, err)
	}
	app, err := appStore.GetApp(fixture.ctx, result.AppID)
	// Persisted app state is the local status endpoint and runtime's single lifecycle authority.
	if err != nil || app.Status != AppStatusActive || app.SDKGenerationStatus != models.SDKGenerationStatusComplete {
		t.Fatalf("completed app = %#v / %v", app, err)
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
	auditCtx := accesscontrol.ContextWithMutationAuditEvidence(fixture.ctx)
	_, err := fixture.repository.ApplyAppConfigPlan(auditCtx, fixture.params)
	if !errors.Is(err, ErrSDKBucketImmutable) {
		t.Fatalf("ApplyAppConfigPlan(stale bucket) = %v, want ErrSDKBucketImmutable", err)
	}
	evidence, ok := accesscontrol.MutationAuditEvidenceFromContext(auditCtx)
	if !ok || !evidence.RolledBack || evidence.Cancelled {
		t.Fatalf("transaction audit evidence = %#v/%v", evidence, ok)
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

// newConcurrentArtifactApplyFixture creates one immutable plan/runtime pair for atomic apply integration coverage.
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
	owner, err := accesscontrol.BootstrapOwner(ctx, accessRepository, accountID, "fsk_artifact_apply_"+uuid.NewString())
	// The fixture needs persisted subject and credential IDs to verify durable attribution.
	if err != nil {
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
		ResolvedPayload: []byte(`{"description":"Coordinate work through the connected service."}`), Blockers: []byte("[]"), Warnings: []byte("[]"),
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
				BucketID: bucketID, Selections: []byte("[]"), ScopeSchemaVersion: models.AppScopeSchemaVersion,
				Kind: AppKind(configType), Name: "concurrent", Description: "Coordinate work through the connected service.", Version: version, ConfigKey: configKey},
			AuthorizedBucketName:      bucketName,
			TokenName:                 "default",
			TokenPolicy:               AppTokenPolicy{AllowAll: true, AllowedOperations: []string{}},
			TokenIssuedBySubjectID:    &owner.SubjectID,
			TokenIssuedByCredentialID: &owner.CredentialID,
			TargetLanguage:            targetLanguage,
			GeneratorVersion:          generatorVersion,
			AppStatus:                 AppStatusActive,
			SDKGenerationJobID:        conditionalSDKGenerationJob(configType, plan.ID),
			SDKGenerationStatus:       conditionalSDKGenerationStatus(configType),
		},
	}
}

// conditionalSDKGenerationJob gives SDK fixtures one terminal durable job while MCP remains package-free.
func conditionalSDKGenerationJob(configType ConfigType, planID uuid.UUID) string {
	// Only SDK configuration owns Registry generation identity.
	if configType == ConfigTypeSDK {
		return planID.String()
	}
	return ""
}

// conditionalSDKGenerationStatus keeps direct atomic-apply fixtures aligned with the adapter lifecycle.
func conditionalSDKGenerationStatus(configType ConfigType) string {
	// SDK fixtures model an already-complete generation response.
	if configType == ConfigTypeSDK {
		return models.SDKGenerationStatusComplete
	}
	return ""
}

// TestValidSDKGenerationTransitionRejectsRunnableDowngrade pins the only mutable retry edge for an immutable SDK version.
func TestValidSDKGenerationTransitionRejectsRunnableDowngrade(t *testing.T) {
	tests := []struct {
		name              string
		currentStatus     AppStatus
		currentGeneration string
		nextStatus        AppStatus
		nextGeneration    string
		nextJobID         string
		want              bool
	}{
		{name: "failed retry pending", currentStatus: AppStatusBuilding, currentGeneration: models.SDKGenerationStatusFailed, nextStatus: AppStatusBuilding, nextGeneration: models.SDKGenerationStatusPending, nextJobID: "job-1", want: true},
		{name: "failed retry complete", currentStatus: AppStatusBuilding, currentGeneration: models.SDKGenerationStatusFailed, nextStatus: AppStatusActive, nextGeneration: models.SDKGenerationStatusComplete, nextJobID: "job-1", want: true},
		{name: "failed generation cannot become direct API", currentStatus: AppStatusBuilding, currentGeneration: models.SDKGenerationStatusFailed, nextStatus: AppStatusActive, nextGeneration: models.SDKGenerationStatusSkipped, nextJobID: "job-1"},
		{name: "active cannot downgrade", currentStatus: AppStatusActive, currentGeneration: models.SDKGenerationStatusComplete, nextStatus: AppStatusBuilding, nextGeneration: models.SDKGenerationStatusPending, nextJobID: "job-1"},
		{name: "pending cannot replace attempt", currentStatus: AppStatusBuilding, currentGeneration: models.SDKGenerationStatusPending, nextStatus: AppStatusBuilding, nextGeneration: models.SDKGenerationStatusPending, nextJobID: "job-2"},
		{name: "failed cannot replace job", currentStatus: AppStatusBuilding, currentGeneration: models.SDKGenerationStatusFailed, nextStatus: AppStatusBuilding, nextGeneration: models.SDKGenerationStatusPending, nextJobID: "job-2"},
	}
	// Every case uses the same retained job unless it deliberately probes substitution.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := validSDKGenerationTransition(test.currentStatus, "job-1", test.currentGeneration, test.nextStatus, test.nextJobID, test.nextGeneration)
			// A false positive could make an immutable version execute the wrong package or regress a runnable app.
			if got != test.want {
				t.Fatalf("validSDKGenerationTransition() = %t, want %t", got, test.want)
			}
		})
	}
}

// assertAtomicArtifactApplyState verifies every runtime, token, state, plan, and identity write committed together.
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
	assertAppRuntimeDescription(t, fixture)
	assertArtifactTokenAuditActor(t, fixture)
	assertCanonicalCapabilityHash(t, fixture)
}

// assertAppRuntimeDescription proves runtime reads project immutable server prose from the exact applied plan.
func assertAppRuntimeDescription(t *testing.T, fixture concurrentArtifactApplyFixture) {
	t.Helper()
	runtime, err := NewPostgresStore(fixture.pool).GetAppRuntime(fixture.ctx, fixture.params.Scope.AppID)
	// A missing runtime row cannot supply protocol identity for the applied app version.
	if err != nil {
		t.Fatalf("read runtime description: %v", err)
	}
	// Mismatched prose would make the session advertise identity from a different source than its catalogue.
	if runtime.Description != fixture.params.Scope.Description {
		t.Fatalf("runtime description = %q, want %q", runtime.Description, fixture.params.Scope.Description)
	}
}

// assertArtifactTokenAuditActor proves account-level config attribution cannot
// be substituted for the authenticated local subject at the audit boundary.
func assertArtifactTokenAuditActor(t *testing.T, fixture concurrentArtifactApplyFixture) {
	t.Helper()
	var subjectID, credentialID uuid.UUID
	err := fixture.pool.QueryRow(fixture.ctx, `
		SELECT actor_subject_id, actor_credential_id
		FROM fused_audit_events
		WHERE action = 'app.token.generate'
		  AND resource_id = (SELECT app_family_id FROM fused_apps WHERE app_id = $1)
	`, fixture.params.Scope.AppID).Scan(&subjectID, &credentialID)
	// A missing audit row fails the atomic-apply contract before identity comparison.
	if err != nil {
		t.Fatalf("read app token audit actor: %v", err)
	}
	// Both foreign keys must identify the authenticated principal supplied to apply.
	if fixture.params.TokenIssuedBySubjectID == nil || fixture.params.TokenIssuedByCredentialID == nil ||
		subjectID != *fixture.params.TokenIssuedBySubjectID || credentialID != *fixture.params.TokenIssuedByCredentialID {
		t.Fatalf("app token audit actor = %s/%s, want %v/%v", subjectID, credentialID, fixture.params.TokenIssuedBySubjectID, fixture.params.TokenIssuedByCredentialID)
	}
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
