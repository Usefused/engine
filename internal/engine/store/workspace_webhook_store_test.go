package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUpsertWorkspaceWebhooksUsesOneStatementForLargeBatch(t *testing.T) {
	const batchSize = 512
	ownerKey := "webhook:batch"
	registrations := make([]WorkspaceWebhook, batchSize)
	persisted := make([]WorkspaceWebhook, batchSize)
	for i := range registrations {
		registration := WorkspaceWebhook{
			ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Label: "batch",
			Slug: uuid.NewString(), OwningConfigKey: ownerKey,
		}
		registrations[i] = registration
		persisted[i] = registration
		persisted[i].ID = uuid.New()
		persisted[i].VerificationHeaders = []string{}
		persisted[i].CreatedAt = time.Now().UTC()
		persisted[i].UpdatedAt = persisted[i].CreatedAt
	}
	queryer := &countingWebhookBatchQueryer{rows: persisted}

	got, err := upsertWorkspaceWebhooks(context.Background(), queryer, registrations)
	if err != nil {
		t.Fatalf("upsertWorkspaceWebhooks: %v", err)
	}
	if queryer.calls != 1 {
		t.Fatalf("batch query calls = %d, want 1 for %d registrations", queryer.calls, batchSize)
	}
	if len(got) != batchSize || got[0].ServiceID != registrations[0].ServiceID || got[batchSize-1].ServiceID != registrations[batchSize-1].ServiceID {
		t.Fatalf("batch result did not preserve input size/order: len=%d", len(got))
	}
	var encoded []workspaceWebhookBatchRow
	if err := json.Unmarshal([]byte(queryer.payload), &encoded); err != nil {
		t.Fatalf("decode batch query payload: %v", err)
	}
	if len(encoded) != batchSize || encoded[0].VerificationHeaders == nil {
		t.Fatalf("batch payload = %d rows, first headers %#v", len(encoded), encoded[0].VerificationHeaders)
	}
}

func TestUpsertWorkspaceWebhooksDetectsOwnerConflict(t *testing.T) {
	registrations := []WorkspaceWebhook{{ServiceID: uuid.New()}, {ServiceID: uuid.New()}}
	queryer := &countingWebhookBatchQueryer{rows: registrations[:1]}
	_, err := upsertWorkspaceWebhooks(context.Background(), queryer, registrations)
	if !errors.Is(err, ErrWorkspaceWebhookOwnerConflict) {
		t.Fatalf("upsertWorkspaceWebhooks error = %v, want owner conflict", err)
	}
}

func TestValidateWebhookRegistrationRequiresCompleteImmutableSecretBinding(t *testing.T) {
	bucketID := uuid.New()
	base := WorkspaceWebhook{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Label: "secure", Slug: uuid.NewString(),
		OwningConfigKey: "webhook:secure",
	}
	valid := base
	valid.SecretRef, valid.SecretBucketID = "${bucket.production.secret.signing}", &bucketID
	if err := validateWebhookRegistration(valid, valid.OwningConfigKey); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	for _, invalid := range []WorkspaceWebhook{
		func() WorkspaceWebhook { value := base; value.SecretRef = valid.SecretRef; return value }(),
		func() WorkspaceWebhook { value := base; value.SecretBucketID = &bucketID; return value }(),
		func() WorkspaceWebhook {
			value := valid
			value.SecretRef = "${bucket.production.env.signing}"
			return value
		}(),
	} {
		if err := validateWebhookRegistration(invalid, invalid.OwningConfigKey); !errors.Is(err, ErrWorkspaceWebhookNotFound) {
			t.Fatalf("invalid binding accepted: %#v, %v", invalid, err)
		}
	}
}

type countingWebhookBatchQueryer struct {
	calls   int
	payload string
	rows    []WorkspaceWebhook
}

func (queryer *countingWebhookBatchQueryer) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	queryer.calls++
	queryer.payload, _ = args[0].(string)
	return &webhookBatchRows{rows: queryer.rows, current: -1}, nil
}

type webhookBatchRows struct {
	rows    []WorkspaceWebhook
	current int
	closed  bool
}

func (rows *webhookBatchRows) Close()                                       { rows.closed = true }
func (rows *webhookBatchRows) Err() error                                   { return nil }
func (rows *webhookBatchRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (rows *webhookBatchRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (rows *webhookBatchRows) Values() ([]any, error)                       { return nil, nil }
func (rows *webhookBatchRows) RawValues() [][]byte                          { return nil }
func (rows *webhookBatchRows) Conn() *pgx.Conn                              { return nil }

func (rows *webhookBatchRows) Next() bool {
	rows.current++
	if rows.current >= len(rows.rows) {
		rows.Close()
		return false
	}
	return true
}

func (rows *webhookBatchRows) Scan(dest ...any) error {
	webhook := rows.rows[rows.current]
	*dest[0].(*uuid.UUID), *dest[1].(*uuid.UUID), *dest[2].(*uuid.UUID) = webhook.ID, webhook.ServiceID, webhook.ServiceVersionID
	*dest[3].(*string), *dest[4].(*string) = webhook.Label, webhook.Slug
	*dest[5].(*string), *dest[6].(*string), *dest[7].(*string) = webhook.AuthType, webhook.AuthLocation, webhook.AuthKeyName
	*dest[8].(*string), *dest[9].(*[]string), *dest[10].(*string) = webhook.SignatureHeader, webhook.VerificationHeaders, webhook.EventExtractionPath
	*dest[11].(*string) = webhook.SecretRef
	*dest[12].(**uuid.UUID) = webhook.SecretBucketID
	*dest[13].(*string) = webhook.OwningConfigKey
	*dest[14].(*time.Time), *dest[15].(*time.Time) = webhook.CreatedAt, webhook.UpdatedAt
	return nil
}

// TestWorkspaceWebhookStore groups the DB-backed integration tests for the
// new Engine-owned webhook registration store. Skipped when DATABASE_URL is
// unset, same convention as TestActivationStore in workspace_store_test.go.
//
//	DATABASE_URL=postgres://... go test ./internal/engine/store/...
func TestWorkspaceWebhookStore(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping workspace webhook store tests: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	defer pool.Close()
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, cleanupErr := pool.Exec(cleanupCtx, "DELETE FROM fused_workspace_webhooks"); cleanupErr != nil {
			t.Errorf("clean webhook fixtures: %v", cleanupErr)
		}
	}()

	s := NewPostgresStore(pool)
	accountID := uuid.New()

	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("reset singleton workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspace_webhooks"); err != nil {
		t.Fatalf("reset webhooks: %v", err)
	}
	_, err = s.BootstrapWorkspace(ctx, accountID, "Webhook Store Test Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}

	t.Run("TestWebhookSecretBindingColumnsStayConsistent", func(t *testing.T) {
		bucket, err := s.CreateBucket(ctx, "webhook-check-"+uuid.NewString(), false)
		if err != nil {
			t.Fatalf("create check bucket: %v", err)
		}
		for _, insert := range []struct {
			name     string
			ref      string
			bucketID *uuid.UUID
		}{
			{name: "ref-without-id", ref: "${bucket.any.secret.signing}"},
			{name: "id-without-ref", bucketID: &bucket.ID},
		} {
			_, err := pool.Exec(ctx, `
				INSERT INTO fused_workspace_webhooks
					(service_id, service_version_id, label, slug, secret_ref, secret_bucket_id, owning_config_key)
				VALUES ($1, $2, $3, $4, $5, $6, 'webhook:invalid')
			`, uuid.New(), uuid.New(), insert.name, uuid.NewString(), insert.ref, insert.bucketID)
			if err == nil {
				t.Fatalf("%s bypassed secret binding consistency check", insert.name)
			}
		}
	})

	t.Run("TestGetWorkspaceWebhookBySlug_ResolvesAndMisses", func(t *testing.T) {
		svcID := uuid.New()
		created, err := seedOwnedWorkspaceWebhooks(ctx, s, []WorkspaceWebhook{{
			ServiceID: svcID, ServiceVersionID: uuid.New(), Label: "prod",
			Slug: "ddddddddddddddddddddd", OwningConfigKey: "webhook:prod",
		}})
		if err != nil {
			t.Fatalf("seed owned webhook: %v", err)
		}

		got, err := s.GetWorkspaceWebhookBySlug(ctx, created[0].Slug)
		if err != nil {
			t.Fatalf("GetWorkspaceWebhookBySlug: %v", err)
		}
		if got.Label != "prod" {
			t.Fatalf("expected label %q, got %q", "prod", got.Label)
		}
		// The ingress lookup joins fused_workspaces to resolve AccountID --
		// downstream ingress code (NATS subject, analytics) needs it and this
		// must not require a second round trip.
		if got.AccountID != accountID {
			t.Fatalf("expected AccountID %s resolved via join, got %s", accountID, got.AccountID)
		}

		_, err = s.GetWorkspaceWebhookBySlug(ctx, "does-not-exist")
		if !errors.Is(err, ErrWorkspaceWebhookNotFound) {
			t.Fatalf("expected ErrWorkspaceWebhookNotFound for an unknown slug, got %v", err)
		}
	})

	t.Run("TestListWorkspaceWebhooks_ReturnsAllLabelsForService", func(t *testing.T) {
		svcID := uuid.New()
		registrations := make([]WorkspaceWebhook, 0, 3)
		for _, label := range []string{"repo-a", "repo-b", "staging"} {
			registrations = append(registrations, WorkspaceWebhook{ServiceID: svcID, ServiceVersionID: uuid.New(), Label: label, Slug: label + "-slug-000000000000", OwningConfigKey: "webhook:" + label})
		}
		if _, err := seedOwnedWorkspaceWebhooks(ctx, s, registrations); err != nil {
			t.Fatalf("seed owned webhooks: %v", err)
		}

		list, err := s.ListWorkspaceWebhooks(ctx, svcID)
		if err != nil {
			t.Fatalf("ListWorkspaceWebhooks: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("expected 3 registrations, got %d: %#v", len(list), list)
		}
	})

	t.Run("TestDeleteBucketRejectsWebhookSecretBinding", func(t *testing.T) {
		bucket, err := s.CreateBucket(ctx, "webhook-bound-"+uuid.NewString(), false)
		if err != nil {
			t.Fatalf("create webhook bucket: %v", err)
		}
		bucketID := bucket.ID
		registrations, err := seedOwnedWorkspaceWebhooks(ctx, s, []WorkspaceWebhook{{
			ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Label: "bound", Slug: uuid.NewString(),
			SecretRef: "${bucket." + bucket.Name + ".secret.signing}", SecretBucketID: &bucketID,
			OwningConfigKey: "webhook:bound",
		}})
		if err != nil || len(registrations) != 1 {
			t.Fatalf("seed webhook binding: %#v, %v", registrations, err)
		}
		if err := s.DeleteBucket(ctx, bucket.Name, bucket.ID); !errors.Is(err, ErrBucketBound) {
			t.Fatalf("DeleteBucket(webhook-bound) = %v, want ErrBucketBound", err)
		}
		persisted, err := s.GetWorkspaceWebhookBySlug(ctx, registrations[0].Slug)
		if err != nil || persisted.SecretBucketID == nil || *persisted.SecretBucketID != bucket.ID {
			t.Fatalf("bound delete changed webhook: %#v, %v", persisted, err)
		}
	})
}

func seedOwnedWorkspaceWebhooks(ctx context.Context, s Store, registrations []WorkspaceWebhook) ([]WorkspaceWebhook, error) {
	return upsertWorkspaceWebhooks(ctx, s.(*postgresStore).db, registrations)
}
