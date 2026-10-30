package bridgehost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// databaseFixture confines every test to a disposable schema in the explicitly selected test server.
func databaseFixture(t *testing.T) (*Repository, accesscontrol.Actor) {
	t.Helper()
	dsn := os.Getenv("BRIDGE_TEST_DATABASE_URL")
	// Never fall back to a developer's production DATABASE_URL.
	if dsn == "" {
		t.Skip("BRIDGE_TEST_DATABASE_URL not set")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := "bridge_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	account := uuid.New()
	workspace, err := store.NewPostgresStore(pool).BootstrapWorkspace(ctx, account, "Bridge fixture")
	require.NoError(t, err)
	// Bootstrap the actual authorization catalogue rather than assuming workspace creation grants ownership.
	bootstrap, err := accesscontrol.BootstrapOwner(ctx, store.NewPostgresStore(pool).(accesscontrol.BootstrapRepository), account, "bridge-test-bootstrap-credential")
	require.NoError(t, err)
	subject := bootstrap.SubjectID
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, accesscontrol.Grant{Permission: accesscontrol.PermissionBridgeManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}})
	require.NoError(t, err)
	return &Repository{Pool: pool}, accesscontrol.Actor{AccountID: account, WorkspaceID: workspace, SubjectID: subject, Authorization: snapshot}
}

// eventManifest creates an event contract with the same exact-byte rules as a real Bridge.
func eventManifest(t *testing.T, revision string, historical bool) *bridgev1.Manifest {
	t.Helper()
	contract := `{"kind":"event","input_schema":{"type":"object"},"output_schema":{"type":"integer"}}`
	doc := ManifestDocument{Version: 1, ProfileID: "sensors", Revision: revision, Operations: []Operation{{ConnectionID: "mqtt", OperationID: "messages", ContractHash: Digest([]byte(contract)), ContractJSON: contract}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	return &bridgev1.Manifest{ProfileId: doc.ProfileID, Revision: revision, Hash: Digest(raw), ManifestJson: raw, Historical: historical}
}

// pairingFixture grants one profile and claims one session generation before storage operations.
func pairingFixture(t *testing.T, repo *Repository, actor accesscontrol.Actor) (Pairing, string) {
	t.Helper()
	p, secret, err := repo.Create(t.Context(), actor, uuid.NewString(), "Fixture", "workspace")
	require.NoError(t, err)
	p.Generation, err = repo.Claim(t.Context(), p, "fixture")
	require.NoError(t, err)
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "sensors")))
	return p, secret
}

// TestPostgresAcceptanceReplayAndRevocation verifies the storage boundary that permits Bridge deletion.
func TestPostgresAcceptanceReplayAndRevocation(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, secret := pairingFixture(t, repo, actor)
	manifest := eventManifest(t, "1", false)
	if err := repo.PutManifest(t.Context(), p, manifest); err != nil {
		t.Fatal(err)
	}
	var doc ManifestDocument
	_ = json.Unmarshal(manifest.ManifestJson, &doc)
	event := &bridgev1.Event{EventId: uuid.NewString(), ProfileId: "sensors", ConnectionId: "mqtt", OperationId: "messages", ManifestHash: manifest.Hash, ContractHash: doc.Operations[0].ContractHash, PayloadJson: []byte("42"), CapturedUnixMs: time.Now().UnixMilli(), DestinationId: p.DestinationID}
	if err := repo.AcceptEvent(t.Context(), p, event); err != nil {
		t.Fatal(err)
	}
	// A new repository instance proves replay uses durable state rather than process memory.
	restarted := &Repository{Pool: repo.Pool}
	if err := restarted.AcceptEvent(t.Context(), p, event); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repo.Pool.QueryRow(t.Context(), `SELECT count(*) FROM fused_bridge_events WHERE pairing_id=$1`, p.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate persistence: %d %v", count, err)
	}
	event.PayloadJson = []byte("43")
	if err := repo.AcceptEvent(t.Context(), p, event); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed-content ID reuse: %v", err)
	}
	event.EventId = uuid.NewString()
	event.DestinationId = uuid.NewString()
	if err := repo.AcceptEvent(t.Context(), p, event); !errors.Is(err, ErrContract) {
		t.Fatalf("foreign destination: %v", err)
	}
	_, err := repo.Pool.Exec(t.Context(), `UPDATE fused_bridge_pairings SET revoked_at=now() WHERE id=$1`, p.ID)
	require.NoError(t, err)
	if _, err = repo.Authenticate(t.Context(), secret); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked credential accepted")
	}
	if err = repo.PutManifest(t.Context(), p, manifest); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked session published manifest")
	}
}

// TestHistoricalManifestDoesNotReplaceCurrent ensures replay metadata cannot downgrade the catalogue.
func TestHistoricalManifestDoesNotReplaceCurrent(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, _ := pairingFixture(t, repo, actor)
	current := eventManifest(t, "2", false)
	old := eventManifest(t, "1", true)
	if err := repo.PutManifest(t.Context(), p, current); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutManifest(t.Context(), p, old); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := repo.Pool.QueryRow(t.Context(), `SELECT manifest_hash FROM fused_bridge_profiles WHERE pairing_id=$1 AND profile_id='sensors'`, p.ID).Scan(&hash); err != nil || hash != current.Hash {
		t.Fatalf("current manifest changed: %s %v", hash, err)
	}
	// Reconnection fences all writes from the old generation.
	if _, err := repo.Claim(t.Context(), p, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutManifest(t.Context(), p, current); !errors.Is(err, ErrDenied) {
		t.Fatal("superseded generation accepted")
	}
}

// TestGRPCSessionAuthenticatesAndAcknowledgesCommittedManifest exercises the actual generated transport.
func TestGRPCSessionAuthenticatesAndAcknowledgesCommittedManifest(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, secret := pairingFixture(t, repo, actor)
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	host := New(repo.Pool, nil)
	require.NoError(t, host.StartRouting(t.Context()))
	bridgev1.RegisterBridgeServiceServer(server, host)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	// In-memory dialing avoids external listeners while preserving protobuf and metadata boundaries.
	client, err := grpc.NewClient("passthrough:///bridge", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
	stream, err := bridgev1.NewBridgeServiceClient(client).Connect(ctx)
	require.NoError(t, err)
	err = stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Hello{Hello: &bridgev1.Hello{ProtocolMajor: 1, ProtocolMinor: 1, PairingId: p.ID, InstallationId: p.InstallationID}}})
	require.NoError(t, err)
	ready, err := stream.Recv()
	if err != nil || ready.GetWelcome() == nil || ready.GetWelcome().DestinationId != p.DestinationID {
		t.Fatalf("handshake: %v %v", ready, err)
	}
	require.NoError(t, stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Inventory{Inventory: inventoryFixture(t, "sensors")}}))
	inventoryAck, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, inventoryAck.GetInventoryAccepted())
	manifest := eventManifest(t, "1", false)
	if err = stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Manifest{Manifest: manifest}}); err != nil {
		t.Fatal(err)
	}
	accepted, err := stream.Recv()
	if err != nil || accepted.GetManifestAccepted().GetHash() != manifest.Hash {
		t.Fatalf("manifest acceptance: %v %v", accepted, err)
	}
	var count int
	if err = repo.Pool.QueryRow(ctx, `SELECT count(*) FROM fused_bridge_manifests WHERE pairing_id=$1`, p.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ACK before persistence: %d %v", count, err)
	}
}

// TestBridgeLiveAuthorityAndProfileWithdrawal checks durable subscriptions against changing workspace grants.
func TestBridgeLiveAuthorityAndProfileWithdrawal(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, secret := pairingFixture(t, repo, actor)
	manifest := eventManifest(t, "1", false)
	require.NoError(t, repo.PutManifest(t.Context(), p, manifest))
	var doc ManifestDocument
	require.NoError(t, json.Unmarshal(manifest.ManifestJson, &doc))
	pin := Pin{PairingID: p.ID, ProfileID: "sensors", ManifestHash: manifest.Hash, ConnectionID: "mqtt", OperationID: "messages", ContractHash: doc.Operations[0].ContractHash}
	host := New(repo.Pool, nil)
	require.NoError(t, host.AuthorizeEventPin(t.Context(), p.AccountID, pin))
	require.ErrorIs(t, host.AuthorizeEventPin(t.Context(), uuid.NewString(), pin), ErrDenied)
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t)))
	require.ErrorIs(t, host.AuthorizeEventPin(t.Context(), p.AccountID, pin), ErrDenied)
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "sensors")))
	require.NoError(t, host.AuthorizeEventPin(t.Context(), p.AccountID, pin))
	_, err := repo.Pool.Exec(t.Context(), `DELETE FROM fused_role_bindings WHERE subject_id=$1`, actor.SubjectID)
	require.NoError(t, err)
	_, err = repo.Authenticate(t.Context(), secret)
	require.ErrorIs(t, err, ErrDenied)
	require.ErrorIs(t, host.AuthorizeEventPin(t.Context(), p.AccountID, pin), ErrDenied)
	require.ErrorIs(t, repo.PutManifest(t.Context(), p, manifest), ErrDenied)
}
