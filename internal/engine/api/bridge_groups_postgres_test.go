package api

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Real store resolution must retain workspace isolation, exact reviewed pins and wildcard staleness checks.
func TestBridgeGroupedPostgresResolution(t *testing.T) {
	url := os.Getenv("FUSED_BRIDGE_GROUP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires a fresh isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, url)
	require.NoError(t, err)
	defer pool.Close()
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM fused_workspaces`).Scan(&count))
	require.Zero(t, count)
	s := store.NewPostgresStore(pool)
	actor := seedBridgePostgresActor(t, ctx, s)
	pin := seedBridgePostgresManifest(t, ctx, pool, actor)
	ctx = accesscontrol.ContextWithActor(ctx, actor)
	ref := models.BridgeReference{Bridge: "test", Profile: "echo-live", Connections: map[string]models.BridgeConnectionSelection{"speaker": {Operations: json.RawMessage(`"*"`)}}}
	doc := sdkConfigDocument{Kind: "unified_app", Bridges: map[string]models.BridgeReference{"home": ref}}
	bindings, err := resolveBridgeAttachments(ctx, s, actor.AccountID, doc)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.Equal(t, "home", bindings[0].Group)
	require.Equal(t, pin, bindings[0].BridgeReference)
	payload := appResolvedPayload{Bridges: bindings}
	require.NoError(t, verifyBridgeAttachments(ctx, s, doc, payload))
	// A second active installation of the same name is ambiguous even if it has no matching profile.
	duplicate := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO fused_bridge_pairings(id,account_id,workspace_id,subject_id,installation_id,destination_id,name,credential_hash,scope) VALUES($1,$2,$3,$4,$5,$6,'test',$7,'workspace')`, duplicate, actor.AccountID, actor.WorkspaceID, actor.SubjectID, uuid.New(), uuid.New(), uuid.NewString())
	require.NoError(t, err)
	_, err = resolveBridgeAttachments(ctx, s, actor.AccountID, doc)
	require.ErrorContains(t, err, "ambiguous")
	_, err = pool.Exec(ctx, `UPDATE fused_bridge_pairings SET revoked_at=now() WHERE id=$1`, duplicate)
	require.NoError(t, err)
	wrong := actor
	wrong.WorkspaceID = uuid.New()
	_, err = s.(store.BridgeBindingStore).ResolveBridgeBindings(accesscontrol.ContextWithActor(ctx, wrong), actor.AccountID, doc.Bridges)
	require.ErrorContains(t, err, "not found")
	// A newly advertised operation invalidates the old wildcard plan; it never silently adds authority at apply.
	var raw []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT document FROM fused_bridge_manifests WHERE pairing_id=$1`, pin.PairingID).Scan(&raw))
	var manifest bridgehost.ManifestDocument
	require.NoError(t, json.Unmarshal(raw, &manifest))
	manifest.Revision = "2"
	extra := manifest.Operations[0]
	extra.OperationID = "device.extra"
	manifest.Operations = append(manifest.Operations, extra)
	raw, err = json.Marshal(manifest)
	require.NoError(t, err)
	hash := bridgehost.Digest(raw)
	_, err = pool.Exec(ctx, `INSERT INTO fused_bridge_manifests(pairing_id,profile_id,revision,hash,document) VALUES($1,'echo-live','2',$2,$3)`, pin.PairingID, hash, raw)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_bridge_profiles SET manifest_hash=$2 WHERE pairing_id=$1`, pin.PairingID, hash)
	require.NoError(t, err)
	require.ErrorContains(t, verifyBridgeAttachments(ctx, s, doc, payload), "plan again")
	fresh, err := resolveBridgeAttachments(ctx, s, actor.AccountID, doc)
	require.NoError(t, err)
	require.Len(t, fresh, 2)
	// Legacy explicit pins retain their historical contract even after the catalogue expands.
	doc.Bridges = map[string]models.BridgeReference{"legacy": pin}
	legacy, err := resolveBridgeAttachments(ctx, s, actor.AccountID, doc)
	require.NoError(t, err)
	require.Equal(t, pin, legacy[0].BridgeReference)
}
