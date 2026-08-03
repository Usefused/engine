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
	INSERT INTO fused_artifact_scopes (account_id, artifact_id, owner_subject_id, scope_schema_version, selections, kind, name, version, created_at)
	VALUES ($7, $8, $2, 1, '[]', 'sdk', 'Support', '1.0.0', NOW() - INTERVAL '2 minutes'),
	       ($7, $9, $2, 1, jsonb_build_array(jsonb_build_object('service_id', $5::text, 'service_version_id', $11::text, 'endpoint_ids', jsonb_build_array($8::text), 'webhook_ids', '[]'::jsonb)), 'sdk', 'Support', '2.0.0', NOW()),
	       ($7, $10, $2, 1, '[]', 'mcp', 'Support', '2.0.0', NOW() + INTERVAL '1 minute')
	`, teamID, subjectID, bucketID, hiddenBucketID, serviceID, credentialID, accountID, sdkV1ID, sdkV2ID, mcpV1ID, serviceVersionID); err != nil {
		t.Fatalf("seed references: %v", err)
	}

	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceTeam, Value: "PLATFORM", AllowedAll: true}, teamID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceUser, Value: "ada@example.test", AllowedAll: true}, subjectID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceBucket, Value: "company credentials", AllowedIDs: []uuid.UUID{bucketID}}, bucketID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceService, Value: "github", AllowedAll: true}, serviceID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceCredential, Value: "laptop", ParentID: subjectID, AllowedAll: true}, credentialID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support@2.0.0", ArtifactKind: "sdk", AllowedAll: true}, sdkV2ID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support", ArtifactKind: "sdk", AllowedAll: true}, sdkV2ID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support", ArtifactKind: "mcp", AllowedAll: true}, mcpV1ID)
	assertReferenceID(t, ctx, repository, ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support", AllowedIDs: []uuid.UUID{mcpV1ID}}, mcpV1ID)
	_, err := repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceArtifact, Value: "support", AllowedAll: true})
	if !errors.Is(err, ErrResourceReferenceAmbiguous) {
		t.Fatalf("cross-kind name error = %v, want ambiguous", err)
	}

	_, err = repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceBucket, Value: "hidden", AllowedIDs: []uuid.UUID{bucketID}})
	if !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("hidden bucket error = %v, want not found", err)
	}
	_, err = repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceArtifact, Value: sdkV2ID.String(), ArtifactKind: "mcp", AllowedAll: true})
	if !errors.Is(err, ErrResourceReferenceNotFound) {
		t.Fatalf("cross-kind UUID error = %v, want not found", err)
	}

	artifacts, total, err := repository.ListAuthorizedArtifactScopesByAccount(ctx, accountID, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{sdkV2ID, mcpV1ID}}, "", 20, 0)
	if err != nil || total != 2 || len(artifacts) != 2 {
		t.Fatalf("scoped artifact page = %d/%d, %v; want 2/2", len(artifacts), total, err)
	}
	services, err := repository.ListArtifactServiceSummaries(ctx, sdkV2ID)
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
