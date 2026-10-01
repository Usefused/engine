package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestUnifiedWebhookResultAttribution admits tokenless automatic events without weakening caller-token requirements.
func TestUnifiedWebhookResultAttribution(t *testing.T) {
	record := ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(),
		AppVersion: "1.0.0", Status: "queued", Mode: "live", ReadHandleHash: strings.Repeat("a", 64)}
	// Ordinary calls must retain an authenticated token identity.
	if err := validateNewExecutionResult(record); !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("tokenless caller admitted: %v", err)
	}
	record.SourceWebhookEventID = uuid.NewString()
	// A stable webhook event supplies explicit automatic provenance instead of a fabricated token.
	if err := validateNewExecutionResult(record); err != nil {
		t.Fatal(err)
	}
	record.AppTokenID = uuid.New()
	// Authenticated callers cannot masquerade as an automatically delivered event.
	if err := validateNewExecutionResult(record); !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("caller injected event provenance: %v", err)
	}
}

// newUnifiedWebhookDatabase creates two immutable versions and one real applied attachment for transactional tests.
func newUnifiedWebhookDatabase(t *testing.T) (*postgresStore, *pgxpool.Pool, context.Context, ExecutionResult, uuid.UUID) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	// Integration writes require an explicitly supplied isolated test database.
	if url == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, url)
	// Tests use the production schema, including additive convergence for existing databases.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	teamID := seedAppOwnerTeam(t, ctx, pool)
	record := ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(),
		AppVersion: "1.0.0", Status: "queued", Mode: "live", ReadHandleHash: strings.Repeat("a", 64),
		Input: json.RawMessage(`{"body":{"value":"event"}}`), SourceWebhookEventID: uuid.NewString()}
	secondID := uuid.New()
	configKey := "unified_app:webhook-test:" + record.AppFamilyID.String()
	// Cleanup is scoped to this test's generated identities and leaves other fixture data untouched.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_unified_app_webhook_deliveries WHERE app_family_id=$1`, record.AppFamilyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_unified_app_results WHERE app_family_id=$1`, record.AppFamilyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_apps WHERE app_family_id=$1`, record.AppFamilyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_app_families WHERE app_family_id=$1`, record.AppFamilyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_config_states WHERE config_key IN ($1,$2)`, configKey, configKey+"-v2")
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_teams WHERE id=$1`, teamID)
	})
	_, err = pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_team_id)
		VALUES($1,$2,'unified_app',$3,'Webhook test','typescript',$4)`, record.AppFamilyID, record.AccountID, configKey, teamID)
	// The family is the ownership and promotion authority for both versions.
	if err != nil {
		t.Fatal(err)
	}
	selections, err := json.Marshal([]models.SDKSelection{{SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: uuid.New(), ServiceVersionID: uuid.New(), WebhookNames: []string{"issue.created"}}})
	// Immutable event scope must use the production selection codec.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,bundle_digest,status,scope_schema_version,selections)
		VALUES($1,$3,$4,'1.0.0',$5,'sha256:webhook',$6,'active',$7,$8),($2,$3,$4,'2.0.0',$5||'-v2','sha256:webhook',$6,'active',$7,$8)`,
		record.AppID, secondID, record.AppFamilyID, record.AccountID, configKey, UnifiedAppBundleDigest([]byte("bundle")), models.AppScopeSchemaVersion, selections)
	// Both versions are retained so later assertions can distinguish promotion from deletion.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_unified_app_bundles(app_id,source_hash,bundle_js,manifest)
		VALUES($1,'sha256:webhook','bundle','{}'),($2,'sha256:webhook','bundle','{}')`, record.AppID, secondID)
	// Only versions with attached bundles may receive automatic traffic.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_config_states(config_key,config_type,owner_team_id,source_hash,desired_state)
		VALUES($1,'unified_app',$2,'sha256:webhook','{"webhook_attachment":"team.events"}'),($1||'-v2','unified_app',$2,'sha256:webhook','{"webhook_attachment":"team.events"}')`, configKey, teamID)
	// Applied desired state supplies exactly the registration name used by existing SDK receivers.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=$2 WHERE app_family_id=$1`, record.AppFamilyID, record.AppID)
	// A retained but unpromoted version must not appear in discovery.
	if err != nil {
		t.Fatal(err)
	}
	return NewPostgresStore(pool).(*postgresStore), pool, ctx, record, secondID
}

// TestUnifiedWebhookPostgresAdmission proves concurrent consumers share one reservation and promotion cannot replay retained events.
func TestUnifiedWebhookPostgresAdmission(t *testing.T) {
	repository, pool, ctx, record, secondID := newUnifiedWebhookDatabase(t)
	target, err := repository.GetUnifiedAppWebhookTarget(ctx, record.AppFamilyID)
	// The target query must join the exact promoted version, attachment, and immutable event names.
	if err != nil || target.AppID != record.AppID || target.Attachment != "team.events" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	assertConcurrentUnifiedWebhookAdmission(t, ctx, repository, record)
	_, err = pool.Exec(ctx, `DELETE FROM fused_unified_app_results WHERE app_family_id=$1`, record.AppFamilyID)
	// Expired private data is independent of the longer-lived delivery fence.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=$2 WHERE app_family_id=$1`, record.AppFamilyID, secondID)
	// The new version receives only newly admitted events.
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.ReserveUnifiedAppWebhookExecution(ctx, record)
	// Stale subscriptions cannot run a retained prior version after promotion.
	if !errors.Is(err, ErrAppDeactivated) {
		t.Fatalf("stale admission: %v", err)
	}
	record.AppID, record.AppVersion = secondID, "2.0.0"
	_, fresh, err := repository.ReserveUnifiedAppWebhookExecution(ctx, record)
	// A prior event stays consumed even after its result expires and its app version changes.
	if err != nil || fresh {
		t.Fatalf("promoted duplicate fresh=%v err=%v", fresh, err)
	}
	assertNewUnifiedWebhookResult(t, ctx, repository, record)
}

// assertNewUnifiedWebhookResult verifies new-event provenance after a traffic change.
func assertNewUnifiedWebhookResult(t *testing.T, ctx context.Context, repository *postgresStore, record ExecutionResult) {
	t.Helper()
	record.SourceWebhookEventID = uuid.NewString()
	_, fresh, err := repository.ReserveUnifiedAppWebhookExecution(ctx, record)
	// A new event uses the newly promoted code identity.
	if err != nil || !fresh {
		t.Fatalf("new event fresh=%v err=%v", fresh, err)
	}
	stored, err := repository.GetExecutionResult(ctx, record.AccountID, record.AppID, record.ID)
	// Retained results expose event provenance without a fabricated caller-token identity.
	if err != nil || stored.SourceWebhookEventID != record.SourceWebhookEventID || stored.AppTokenID != uuid.Nil {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

// assertConcurrentUnifiedWebhookAdmission proves database uniqueness across independent consumers.
func assertConcurrentUnifiedWebhookAdmission(t *testing.T, ctx context.Context, repository *postgresStore, record ExecutionResult) {
	t.Helper()
	var created atomic.Int32
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		// Independent replicas race on the shared SQL uniqueness constraint, not an in-process lock.
		go func() {
			defer group.Done()
			candidate := record
			candidate.ID = uuid.New()
			_, fresh, err := repository.ReserveUnifiedAppWebhookExecution(ctx, candidate)
			// Every contender either wins once or finds that same durable admission.
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			// Only the winning transaction is allowed to start an execution.
			if fresh {
				created.Add(1)
			}
		}()
	}
	group.Wait()
	// Exactly one execution may own provider effects for this family and event.
	if created.Load() != 1 {
		t.Fatalf("created %d runs", created.Load())
	}
}

// TestUnifiedWebhookPostgresRollbackAndRemoval proves malformed results cannot consume events and removed targets lose authority.
func TestUnifiedWebhookPostgresRollbackAndRemoval(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	invalid := record
	invalid.Input = json.RawMessage(`not json`)
	_, _, err := repository.ReserveUnifiedAppWebhookExecution(ctx, invalid)
	// A failed result insert must roll its delivery fence back in the same transaction.
	if !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("invalid admission: %v", err)
	}
	_, created, err := repository.ReserveUnifiedAppWebhookExecution(ctx, record)
	// The corrected envelope remains eligible because the failed reservation owned no effects.
	if err != nil || !created {
		t.Fatalf("rollback consumed event: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=NULL WHERE app_family_id=$1`, record.AppFamilyID)
	// Deactivation clears traffic without selecting an older retained version.
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.GetUnifiedAppWebhookTarget(ctx, record.AppFamilyID)
	// Per-delivery admission observes removal even before the periodic consumer scan.
	if !errors.Is(err, ErrAppRuntimeNotFound) {
		t.Fatalf("removed target: %v", err)
	}
}
