package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/capability"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
)

// invokeConsumerFixture publishes an isolated exact app, then calls the real authenticated REST execution route.
func invokeConsumerFixture(ctx context.Context, pool *pgxpool.Pool, repository store.Store, host *bridgehost.Server, actor accesscontrol.Actor, pin bridgehost.Pin, input json.RawMessage) ([]byte, error) {
	id, token, err := publishConsumerFixture(ctx, pool, repository, actor, pin)
	// Invalid or unpublished contract pins cannot reach runtime invocation.
	if err != nil {
		return nil, err
	}
	server := api.NewEngineGRPCServer(repository, nil, nil, nil, nil, auth.NewTokenValidator(repository), nil)
	server.ConfigureBridgeHost(host)
	router := chi.NewRouter()
	api.MountAppExecutionRoute(router, server)
	body, _ := json.Marshal(map[string]any{"operation": "bridge:fixture", "input": input})
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+id.String()+"/executions", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	result := httptest.NewRecorder()
	router.ServeHTTP(result, request)
	// A fixture failure must show the bounded Engine diagnostic, never its private execution credential.
	if result.Code != 200 {
		return nil, fmt.Errorf("app execution status %d: %s", result.Code, result.Body.String())
	}
	return result.Body.Bytes(), nil
}

// publishConsumerFixture atomically publishes accepted Bridge contracts and their canonical app capabilities.
func publishConsumerFixture(ctx context.Context, pool *pgxpool.Pool, repository store.Store, actor accesscontrol.Actor, pin bridgehost.Pin) (uuid.UUID, string, error) {
	bindings, err := repository.(store.BridgeBindingStore).ResolveBridgeBindings(ctx, actor.AccountID, map[string]models.BridgeReference{"fixture": {PairingID: pin.PairingID, ProfileID: pin.ProfileID, ManifestHash: pin.ManifestHash, ConnectionID: pin.ConnectionID, OperationID: pin.OperationID}})
	// Fixture control authority cannot invent operation contracts absent from accepted storage.
	if err != nil {
		return uuid.Nil, "", err
	}
	keys, capabilityHash, err := capability.KeysAndHashWithBridges([]byte(`[]`), bindings)
	// Fixture publication must use the same capability identity as production app apply.
	if err != nil {
		return uuid.Nil, "", err
	}
	payload, _ := json.Marshal(map[string]any{"bridges": bindings})
	family, id, bucket, plan := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	name := "fixture:" + id.String()
	tx, err := pool.Begin(ctx)
	// Publication must be atomic so the token cannot see a partially configured app.
	if err != nil {
		return uuid.Nil, "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, []any{bucket, name}},
		{`INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_subject_id,delivery_mode) VALUES($1,$2,'sdk',$3,'Bridge fixture','typescript',$4,'api')`, []any{family, actor.AccountID, name, actor.SubjectID}},
		{`INSERT INTO fused_app_family_buckets(app_family_id,bucket_id) VALUES($1,$2)`, []any{family, bucket}},
		{`INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status,selections,scope_schema_version,capability_hash) VALUES($1,$2,$3,'1.0.0',$4,'fixture','active','[]',$5,$6)`, []any{id, family, actor.AccountID, name, models.AppScopeSchemaVersion, capabilityHash}},
		{`INSERT INTO fused_app_capabilities(app_id,capability_key) SELECT $1,unnest($2::text[])`, []any{id, keys}},
		{`INSERT INTO fused_config_plans(id,config_key,config_type,source_hash,status,resolved_payload,required_permissions,owner_subject_id) VALUES($1,$2,'sdk','fixture','applied',$3,'[]',$4)`, []any{plan, name, payload, actor.SubjectID}},
	}
	for _, statement := range statements {
		// Stop and roll back all fixture publication state on the first relational failure.
		if _, err = tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			return uuid.Nil, "", err
		}
	}
	// Only a committed exact app can receive a separate family execution credential.
	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, "", err
	}
	token := "fixture_app_" + uuid.NewString()
	_, err = repository.CreateAppToken(ctx, store.AppTokenIssue{ID: uuid.New(), AppFamilyID: family, TokenHash: auth.HashToken(token), Name: "fixture", BindingMode: store.AppTokenBindingDynamic, Policy: store.AppTokenPolicy{AllowedOperations: []string{"bridge:fixture"}}})
	return id, token, err
}
