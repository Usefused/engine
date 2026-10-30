package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestBridgePostgresHTTPPlanApplyExport exercises production handlers, PostgreSQL commit, token issuance and export together.
func TestBridgePostgresHTTPPlanApplyExport(t *testing.T) {
	url := os.Getenv("FUSED_BRIDGE_TEST_DATABASE_URL")
	// This destructive-isolation fixture requires its own explicitly selected empty test database.
	if url == "" {
		t.Skip("FUSED_BRIDGE_TEST_DATABASE_URL must name a fresh isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, url)
	require.NoError(t, err)
	defer pool.Close()
	var workspaces int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM fused_workspaces`).Scan(&workspaces))
	require.Zero(t, workspaces, "integration fixture requires an empty isolated database")
	s := store.NewPostgresStore(pool)
	configs := store.NewPostgresConfigRepository(pool)
	actor := seedBridgePostgresActor(t, ctx, s)
	ref := seedBridgePostgresManifest(t, ctx, pool, actor)
	ctx = accesscontrol.ContextWithActor(ctx, actor)
	body, err := json.Marshal(map[string]any{"source_hash": "postgres-bridge-flow", "config_key": "sdk:echo-db-test:1.0.0", "config": map[string]any{
		"apiVersion": "fused/v1", "kind": "sdk", "name": "echo-db-test", "version": "1.0.0", "language": "typescript", "bucket": "default", "generate": false, "bridges": map[string]models.BridgeReference{"echo_device": ref},
	}})
	require.NoError(t, err)
	registry := &generationPlanningRegistryTrap{RegistryClient: &mockRegistryClient{}, t: t}
	planned := httptest.NewRecorder()
	SDKConfigPlanHandler(configs, s, registry)(planned, httptest.NewRequest(http.MethodPost, "/sdk-config/plan", bytes.NewReader(body)).WithContext(ctx))
	require.Equal(t, http.StatusOK, planned.Code, planned.Body.String())
	var receipt struct {
		PlanID uuid.UUID `json:"plan_id"`
	}
	require.NoError(t, json.Unmarshal(planned.Body.Bytes(), &receipt))
	plan, err := configs.GetConfigPlan(ctx, receipt.PlanID)
	require.NoError(t, err)
	applied := applyBridgePostgresPlan(t, ctx, s, configs, plan, registry)
	app, err := s.GetApp(ctx, applied.AppID)
	require.NoError(t, err)
	contracts, ok := s.(appOpenAPIContractStore)
	require.True(t, ok)
	router := chi.NewRouter()
	router.Get("/apps/{app_id}/openapi", AppOpenAPIHandler(s, contracts))
	exported := httptest.NewRecorder()
	router.ServeHTTP(exported, httptest.NewRequest(http.MethodGet, "/apps/"+app.AppID.String()+"/openapi", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, exported.Code, exported.Body.String())
	document := exported.Body.Bytes()
	parsed, err := openapi3.NewLoader().LoadFromData(document)
	require.NoError(t, err)
	require.NoError(t, parsed.Validate(ctx))
	require.Contains(t, string(document), "bridge:echo_device")
}

// seedBridgePostgresActor creates real ownership records while using the test's explicit owner permission snapshot.
func seedBridgePostgresActor(t *testing.T, ctx context.Context, s store.Store) accesscontrol.Actor {
	t.Helper()
	account := uuid.New()
	workspace, err := s.BootstrapWorkspace(ctx, account, "Bridge database test")
	require.NoError(t, err)
	repository, ok := s.(accesscontrol.BootstrapRepository)
	require.True(t, ok)
	owner, err := accesscontrol.BootstrapOwner(ctx, repository, account, "fsk_bridge_db_test_"+uuid.NewString())
	require.NoError(t, err)
	actor := controlTestOwnerActor(account)
	actor.WorkspaceID, actor.SubjectID, actor.CredentialID = workspace, owner.SubjectID, owner.CredentialID
	grants := make([]accesscontrol.Grant, 0, len(accesscontrol.AllPermissions()))
	for _, permission := range accesscontrol.AllPermissions() {
		grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}})
	}
	actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(1, grants...)
	require.NoError(t, err)
	return actor
}

// seedBridgePostgresManifest stores one validated local schema through the exact tables used by production reference resolution.
func seedBridgePostgresManifest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, actor accesscontrol.Actor) models.BridgeReference {
	t.Helper()
	pairing := uuid.New()
	contract := `{"kind":"read","input_schema":{"type":"object"},"output_schema":{"type":"boolean"}}`
	raw, err := json.Marshal(bridgehost.ManifestDocument{Version: 1, ProfileID: "echo-live", Revision: "1", Operations: []bridgehost.Operation{{ConnectionID: "speaker", OperationID: "device.info", ContractHash: bridgehost.Digest([]byte(contract)), ContractJSON: contract}}})
	require.NoError(t, err)
	hash := bridgehost.Digest(raw)
	_, err = bridgehost.ValidateManifest(&bridgev1.Manifest{ProfileId: "echo-live", Revision: "1", Hash: hash, ManifestJson: raw})
	require.NoError(t, err)
	// This fixture represents a direct control pairing with explicit authorization provenance.
	_, err = pool.Exec(ctx, `INSERT INTO fused_bridge_pairings(id,account_id,workspace_id,subject_id,installation_id,destination_id,name,credential_hash,scope,credential_source) VALUES($1,$2,$3,$4,$5,$6,'test','test-credential','workspace','control')`, pairing, actor.AccountID, actor.WorkspaceID, actor.SubjectID, uuid.New(), uuid.New())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_bridge_manifests(pairing_id,profile_id,revision,hash,document) VALUES($1,'echo-live','1',$2,$3)`, pairing, hash, raw)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_bridge_profiles(pairing_id,profile_id,manifest_hash,state) VALUES($1,'echo-live',$2,'online')`, pairing, hash)
	require.NoError(t, err)
	return models.BridgeReference{PairingID: pairing.String(), ProfileID: "echo-live", ManifestHash: hash, ConnectionID: "speaker", OperationID: "device.info"}
}

type bridgePostgresApplyResult struct {
	AppID          uuid.UUID `json:"app_id"`
	ExecutionToken string    `json:"execution_token"`
}

// applyBridgePostgresPlan verifies one-time token issuance and durable plan state after the real publication transaction.
func applyBridgePostgresPlan(t *testing.T, ctx context.Context, s store.Store, configs store.ConfigRepository, plan *store.ConfigPlan, registry *generationPlanningRegistryTrap) bridgePostgresApplyResult {
	t.Helper()
	body, err := json.Marshal(map[string]any{"plan_id": plan.ID, "source_hash": plan.SourceHash})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/apply", bytes.NewReader(body)).WithContext(ContextWithAuthorizedPlanRevision(ctx, plan.Revision))
	SDKConfigApplyHandler(configs, s, nil, registry)(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var applied bridgePostgresApplyResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &applied))
	require.NotEqual(t, uuid.Nil, applied.AppID)
	require.NotEmpty(t, applied.ExecutionToken)
	projection, err := s.AuthorizeApp(ctx, applied.AppID, auth.HashToken(applied.ExecutionToken))
	require.NoError(t, err)
	require.Equal(t, applied.AppID, projection.AppID)
	stored, err := configs.GetConfigPlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, store.ConfigPlanStatusApplied, stored.Status)
	return applied
}
