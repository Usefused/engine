package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestLiveSnapshotFixtures seeds isolated identities for a separately launched production Engine and keeps cleanup ownership.
func TestLiveSnapshotFixtures(t *testing.T) {
	directory := os.Getenv("FUSED_LIVE_SNAPSHOT_DIRECTORY")
	// Ordinary tests must never seed an external daemon or wait for a live-test controller.
	if directory == "" {
		t.Skip("FUSED_LIVE_SNAPSHOT_DIRECTORY enables live daemon fixtures")
	}
	// Only the explicitly disposable live database may receive these fixture identities.
	if !strings.Contains(os.Getenv("DATABASE_URL"), "/fused_snapshot_live_") {
		t.Fatal("live snapshot fixtures require their isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	entries := map[string]map[string]string{}
	for _, mode := range []string{"sdk", "on", "off"} {
		entries[mode] = seedLiveSnapshotFamily(t, ctx, mode, os.Getenv("FUSED_LIVE_PROVIDER_URL"))
	}
	raw, err := json.Marshal(entries)
	recoveryAPIMust(t, err)
	recoveryAPIMust(t, os.WriteFile(filepath.Join(directory, "apps.private.json"), raw, 0600))
	t.Log("Live SDK and snapshot-on/off app fixtures ready; credentials saved privately")
	waitLiveSnapshotController(t, ctx, filepath.Join(directory, "done"))
}

// waitLiveSnapshotController retains fixture cleanup while the external controller performs real HTTP calls and SIGKILL tests.
func waitLiveSnapshotController(t *testing.T, ctx context.Context, done string) {
	t.Helper()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		// A bounded fixture lifetime prevents an abandoned controller from retaining test processes indefinitely.
		case <-ctx.Done():
			t.Fatal("live controller did not finish")
		case <-ticker.C:
			// The completion file is written only after the daemon has stopped using fixture rows.
			if _, err := os.Stat(done); err == nil {
				return
			}
		}
	}
}

// seedLiveSnapshotFamily reuses the production compiler, contract store, bucket setup, and family-token issuer.
func seedLiveSnapshotFamily(t *testing.T, ctx context.Context, mode, providerURL string) map[string]string {
	t.Helper()
	fixture := openMCPAppOperationPostgresFixture(t)
	fixture.ctx = ctx
	recoveryAPIMust(t, fixture.pool.QueryRow(ctx, `SELECT account_id FROM fused_workspaces WHERE singleton_key=1`).Scan(&fixture.accountID))
	// SDK calls use physical operations; both app modes execute the identical authored program.
	if mode == "sdk" {
		seedLiveSnapshotSDK(t, fixture)
	} else {
		source := fmt.Sprintf(`import * as z from "zod/mini";
import {buildUnifiedApp} from "@fused/unified-app";
import {fused} from "@fused/operations";
export default buildUnifiedApp({snapshot:%t,recovery:{mode:"manual",maxAttempts:3,backoffSeconds:1},
input:z.object({case:z.string()}),output:z.object({first:z.number(),second:z.number()}),
// Identical ordinary code makes provider counts reveal the effect of snapshot opt-in.
async execute({input}){const first=z.object({id:z.number()}).parse(await fused.todos.first(input));
const second=z.object({id:z.number()}).parse(await fused.todos.second(input));return {first:first.id,second:second.id};}});`, mode == "on")
		pins := []executionCompilerSelection{
			{Service: "todos", Operation: "first", ServiceID: fixture.serviceID.String(), ServiceVersionID: fixture.serviceVersionID.String(), EndpointID: fixture.listEndpointID.String()},
			{Service: "todos", Operation: "second", ServiceID: fixture.serviceID.String(), ServiceVersionID: fixture.serviceVersionID.String(), EndpointID: fixture.createEndpointID.String()},
		}
		artifact, err := runExecutionCompiler(ctx, source, pins)
		recoveryAPIMust(t, err)
		seedRecoveryApp(t, fixture, artifact)
	}
	seedRecoveryLiveService(t, fixture)
	seedLiveSnapshotProvider(t, fixture, providerURL)
	token := "fused-live-" + uuid.NewString()
	metadata, err := fixture.repository.CreateAppToken(ctx, store.AppTokenIssue{ID: uuid.New(), AppFamilyID: fixture.familyID, TokenHash: auth.HashToken(token), Name: "live-snapshot", BindingMode: store.AppTokenBindingDynamic, Policy: store.AppTokenPolicy{AllowAll: true}})
	recoveryAPIMust(t, err)
	// Remove active tokens before family cleanup so no fixture-owned credentials remain usable.
	t.Cleanup(func() {
		_, err := fixture.repository.RevokeAppToken(context.Background(), fixture.familyID, "live-snapshot")
		recoveryAPIMust(t, err)
	})
	return map[string]string{"app_id": fixture.appID.String(), "family_id": fixture.familyID.String(), "token": token, "token_id": metadata.ID.String()}
}

// seedLiveSnapshotSDK supplies only the immutable app identity required by the normal REST physical executor.
func seedLiveSnapshotSDK(t *testing.T, fixture mcpOperationPostgresFixture) {
	t.Helper()
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_teams(id,name,slug) VALUES($1,'Live snapshot fixture',$2)`, fixture.teamID, "live-"+fixture.teamID.String())
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_team_id) VALUES($1,$2,'sdk',$3,'Live snapshot SDK','typescript',$4)`, fixture.familyID, fixture.accountID, "live-sdk-"+fixture.familyID.String(), fixture.teamID)
	recoveryAPIMust(t, err)
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status,scope_schema_version,selections) VALUES($1,$2,$3,'1.0.0',$4,$5,'active',$6,'[]')`, fixture.appID, fixture.familyID, fixture.accountID, "sdk:live:"+fixture.appID.String(), "sha256:"+strings.Repeat("a", 64), models.AppScopeSchemaVersion)
	recoveryAPIMust(t, err)
}

// seedLiveSnapshotProvider routes case names through declared path parameters to a real local HTTP provider.
func seedLiveSnapshotProvider(t *testing.T, fixture mcpOperationPostgresFixture, baseURL string) {
	t.Helper()
	anonymous := authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
	parameters := fusedobject.Parameters{{Name: "case", In: "path", Required: true, Type: "string", Serialization: fusedobject.ParameterSerialization{Style: "simple"}}}
	// The real REST collector requires an admitted JSON response contract, not merely a JSON Content-Type.
	responses := fusedobject.Responses{"default": {Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/json"}}}}
	snapshot := store.ServiceContractSnapshot{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(), ServiceID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Version: "live-test",
		ServiceMetadata: fusedobject.ServiceMetadata{ID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Name: "Live snapshot provider", BaseURL: baseURL},
		Endpoints: []fusedobject.Endpoint{
			{ID: fixture.listEndpointID, Name: "first", Method: "POST", Path: "/write/{case}", NormalizedPath: "/write/{case}", Parameters: parameters, Responses: responses, SecurityRequirements: anonymous},
			{ID: fixture.createEndpointID, Name: "second", Method: "GET", Path: "/read/{case}", NormalizedPath: "/read/{case}", Parameters: parameters, Responses: responses, SecurityRequirements: anonymous},
		}}
	_, err := fixture.snapshotStore.UpsertServiceContractSnapshot(fixture.ctx, snapshot)
	recoveryAPIMust(t, err)
}
