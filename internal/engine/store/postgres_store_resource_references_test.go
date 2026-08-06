package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestPostgresResourceReferencesResolveHumanKeysAndEnforceScope(t *testing.T) {
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
	if _, err := repository.BootstrapWorkspace(ctx, accountID, "Reference workspace"); err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	teamID, subjectID := uuid.New(), uuid.New()
	bucketID, hiddenBucketID, serviceID := uuid.New(), uuid.New(), uuid.New()
	serviceVersionID := uuid.New()
	sdkV1ID, sdkV2ID, mcpV1ID := uuid.New(), uuid.New(), uuid.New()
	sdkFamilyID, mcpFamilyID := uuid.New(), uuid.New()
	credentialID := uuid.New()
	if _, err := pool.Exec(ctx, `WITH inserted_team AS (
		INSERT INTO fused_teams (id, name, slug) VALUES ($1, 'Platform', 'platform') RETURNING id
	), inserted_subject AS (
		INSERT INTO fused_subjects (id, kind, display_name) VALUES ($2, 'user', 'Ada') RETURNING id
	), inserted_user AS (
		INSERT INTO fused_users (subject_id, email_normalized, email_display)
		SELECT id, 'ada@example.test', 'Ada@Example.test' FROM inserted_subject RETURNING subject_id
	), inserted_buckets AS (
		INSERT INTO fused_buckets (id, name) VALUES ($3, 'Company Credentials'), ($4, 'Hidden') RETURNING id
	), inserted_service AS (
		INSERT INTO fused_workspace_services (service_id, service_slug, service_name) VALUES ($5, 'github', 'GitHub') RETURNING service_id
	), inserted_version AS (
		INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version)
		SELECT service_id, $11, 'v1' FROM inserted_service RETURNING service_version_id
	), inserted_credential AS (
		INSERT INTO fused_control_credentials (id, subject_id, key_hash, key_prefix, name)
		SELECT $6, subject_id, 'reference-hash', 'test', 'Laptop' FROM inserted_user RETURNING id
	)
	SELECT 1
	FROM inserted_credential
	`, teamID, subjectID, bucketID, hiddenBucketID, serviceID, credentialID, accountID, sdkV1ID, sdkV2ID, mcpV1ID, serviceVersionID); err != nil {
		t.Fatalf("seed references: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_subject_id)
		VALUES ($1, $2, 'sdk', 'support', 'Support', 'typescript', $3),
		       ($4, $2, 'mcp', 'support', 'Support', NULL, $3);
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key, source_hash, status, selections)
		VALUES ($5, $1, $2, '1.0.0', 'sdk:support:1.0.0', 'sdk-v1', 'active', '[]'),
		       ($6, $1, $2, '2.0.0', 'sdk:support:2.0.0', 'sdk-v2', 'active',
		        jsonb_build_array(jsonb_build_object('service_id', $8::text, 'service_version_id', $9::text,
		        'endpoint_ids', jsonb_build_array($5::text), 'webhook_ids', '[]'::jsonb))),
		       ($7, $4, $2, '2.0.0', 'mcp:support:2.0.0', 'mcp-v2', 'active', '[]')
	`, sdkFamilyID, accountID, subjectID, mcpFamilyID, sdkV1ID, sdkV2ID, mcpV1ID, serviceID, serviceVersionID); err != nil {
		t.Fatalf("seed app references: %v", err)
	}

	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceTeam, Value: "PLATFORM", AllowedAll: true}, teamID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceUser, Value: "ada@example.test", AllowedAll: true}, subjectID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceBucket, Value: "company credentials", AllowedIDs: []uuid.UUID{bucketID}}, bucketID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceService, Value: "github", AllowedAll: true}, serviceID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceCredential, Value: "laptop", ParentID: subjectID, AllowedAll: true}, credentialID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceApp, Value: "support", AppVersion: "2.0.0", AppKind: "sdk", AllowedAll: true}, sdkV2ID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceApp, Value: sdkV2ID.String(), AppKind: "sdk", AllowedAll: true}, sdkV2ID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceAppFamily, Value: "support", AppKind: "sdk", AllowedAll: true}, sdkFamilyID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceAppFamily, Value: "support", AppKind: "mcp", AllowedAll: true}, mcpFamilyID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceAppFamily, Value: "support", AllowedIDs: []uuid.UUID{mcpFamilyID}}, mcpFamilyID)
	_, err := repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceAppFamily, Value: "support", AllowedAll: true})
	if !errors.Is(err, ErrResourceReferenceAmbiguous) {
		t.Fatalf("cross-kind name error = %v, want ambiguous", err)
	}

	_, err = repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceBucket, Value: "hidden", AllowedIDs: []uuid.UUID{bucketID}})
	if !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("hidden bucket error = %v, want not found", err)
	}
	_, err = repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceApp, Value: sdkV2ID.String(), AppKind: "mcp", AllowedAll: true})
	if !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("cross-kind UUID error = %v, want not found", err)
	}

	artifacts, total, err := repository.ListAuthorizedAppRuntimesByAccount(ctx, accountID, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{sdkFamilyID, mcpFamilyID}}, "", 20, 0)
	if err != nil || total != 3 || len(artifacts) != 3 {
		t.Fatalf("scoped app page = %d/%d, %v; want 3/3", len(artifacts), total, err)
	}
	services, err := repository.ListAuthorizedAppServiceSummaries(ctx, accountID, sdkV2ID, accesscontrol.AuthorizedScope{All: true})
	if err != nil || len(services) != 1 || services[0].ServiceSlug != "github" || services[0].Version != "v1" || services[0].EndpointCount != 1 {
		t.Fatalf("artifact services = %#v, %v", services, err)
	}
}

func assertReferenceID(t *testing.T, ctx context.Context, resolver ResourceReferenceResolver, query ResourceReferenceQuery, want uuid.UUID) {
	t.Helper()
	got, err := resolver.ResolveResourceReference(ctx, query)
	if err != nil || got != want {
		t.Fatalf("ResolveResourceReference(%s, %q) = %s, %v; want %s", query.Kind, query.Value, got, err, want)
	}
}
