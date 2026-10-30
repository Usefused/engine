package bridgehost

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// oauthBridgeActor issues a real consent/code/token and uses its authenticated principal for pairing.
func oauthBridgeActor(t *testing.T, repo *Repository, owner accesscontrol.Actor) (accesscontrol.Actor, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	repository := store.NewPostgresStore(repo.Pool).(store.OAuthClientStore)
	actor := consentMutationActor(t, repo, owner)
	registered, err := repository.CreateOAuthClient(ctx, store.OAuthClientRegistration{
		Name: "Bridge consent fixture", ClientType: store.OAuthClientPublic, ClientID: "foc_" + uuid.NewString(),
		RedirectURIs: []string{"http://127.0.0.1:8976/callback"}, AllowedScopes: []string{"bridge.manage"}, Actor: actor,
	})
	require.NoError(t, err)
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	code := uuid.NewString()
	scope := []string{"bridge.manage"}
	require.NoError(t, repository.RecordOAuthConsentAndIssueCode(ctx,
		store.OAuthConsentGrant{ClientID: registered.Client.ID, SubjectID: owner.SubjectID, GrantedScope: scope},
		store.OAuthAuthorizationCodeIssue{ID: uuid.New(), ClientID: registered.Client.ID, SubjectID: owner.SubjectID,
			RedirectURI: registered.Client.RedirectURIs[0], Scope: scope, CodeHash: accesscontrol.HashControlCredential(code),
			CodeChallenge: base64.RawURLEncoding.EncodeToString(digest[:]), ExpiresAt: time.Now().Add(time.Minute)},
	))
	access := uuid.NewString()
	_, err = repository.ExchangeOAuthAuthorizationCode(ctx, store.OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(code), ClientID: registered.Client.ID,
		RedirectURI: registered.Client.RedirectURIs[0], CodeVerifier: verifier,
		Issue: store.OAuthTokenIssue{AccessTokenHash: accesscontrol.HashControlCredential(access),
			RefreshTokenHash: accesscontrol.HashControlCredential(uuid.NewString()), AccessExpiresAt: time.Now().Add(time.Hour),
			RefreshExpiresAt: time.Now().Add(24 * time.Hour)},
	}, time.Now())
	require.NoError(t, err)
	authenticator, err := accesscontrol.NewAuthenticator(store.NewPostgresStore(repo.Pool).(accesscontrol.PrincipalLoader), 1, accesscontrol.AuthenticatorOptions{})
	require.NoError(t, err)
	delegated, err := authenticator.AuthenticateControlCredential(ctx, access)
	require.NoError(t, err)
	return delegated, registered.Client.ID
}

// bridgeConsentTransport starts the same revocation listener and gRPC handler used by an Engine replica.
func bridgeConsentTransport(t *testing.T, repo *Repository) (*Server, bridgev1.BridgeServiceClient) {
	t.Helper()
	host := New(repo.Pool, nil)
	require.NoError(t, host.StartRouting(t.Context()))
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	bridgev1.RegisterBridgeServiceServer(server, host)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bridge", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return host, bridgev1.NewBridgeServiceClient(conn)
}

// connectConsentBridge completes a real installation handshake with a bounded stream lifetime.
func connectConsentBridge(t *testing.T, client bridgev1.BridgeServiceClient, p Pairing, secret string) (grpc.BidiStreamingClient[bridgev1.BridgeFrame, bridgev1.EngineFrame], Pairing) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
	stream, err := client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Hello{Hello: &bridgev1.Hello{
		ProtocolMajor: 1, ProtocolMinor: 1, PairingId: p.ID, InstallationId: p.InstallationID,
	}}}))
	frame, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, frame.GetWelcome())
	p.Generation = frame.GetWelcome().Generation
	require.NoError(t, stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Inventory{Inventory: inventoryFixture(t, "sensors")}}))
	frame, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, frame.GetInventoryAccepted())
	return stream, p
}

// consentManifest provides an invocable read and an event so revocation covers both directions.
func consentManifest(t *testing.T) (*bridgev1.Manifest, Pin, *bridgev1.Event) {
	t.Helper()
	read := `{"kind":"read","input_schema":{"type":"object"},"output_schema":{"type":"integer"}}`
	event := `{"kind":"event","input_schema":{"type":"object"},"output_schema":{"type":"integer"}}`
	doc := ManifestDocument{Version: 1, ProfileID: "sensors", Revision: "1", Operations: []Operation{
		{ConnectionID: "device", OperationID: "read", ContractHash: Digest([]byte(read)), ContractJSON: read},
		{ConnectionID: "device", OperationID: "changed", ContractHash: Digest([]byte(event)), ContractJSON: event},
	}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	manifest := &bridgev1.Manifest{ProfileId: doc.ProfileID, Revision: doc.Revision, Hash: Digest(raw), ManifestJson: raw}
	pin := Pin{ProfileID: doc.ProfileID, ManifestHash: manifest.Hash, ConnectionID: "device", OperationID: "read", ContractHash: doc.Operations[0].ContractHash}
	captured := &bridgev1.Event{EventId: uuid.NewString(), ProfileId: doc.ProfileID, ManifestHash: manifest.Hash, ConnectionId: "device", OperationId: "changed", ContractHash: doc.Operations[1].ContractHash, PayloadJson: []byte(`42`), CapturedUnixMs: time.Now().UnixMilli()}
	return manifest, pin, captured
}

// TestOAuthConsentRevocationClosesBridgeSessions exercises actual consent removal against two live Engine replicas.
func TestOAuthConsentRevocationClosesBridgeSessions(t *testing.T) {
	repo, owner := databaseFixture(t)
	delegated, clientID := oauthBridgeActor(t, repo, owner)
	p, secret := pairingFixture(t, repo, delegated)
	idle, idleSecret := pairingFixture(t, repo, delegated)
	otherActor, _ := oauthBridgeActor(t, repo, owner)
	_, otherSecret := pairingFixture(t, repo, otherActor)
	_, controlSecret := pairingFixture(t, repo, owner)
	host, client := bridgeConsentTransport(t, repo)
	_, idleClient := bridgeConsentTransport(t, repo)
	stream, p := connectConsentBridge(t, client, p, secret)
	idleStream, _ := connectConsentBridge(t, idleClient, idle, idleSecret)
	manifest, pin, event := consentManifest(t)
	pin.PairingID = p.ID
	event.DestinationId = p.DestinationID
	require.NoError(t, stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Manifest{Manifest: manifest}}))
	accepted, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, accepted.GetManifestAccepted())
	require.NoError(t, repo.AcceptEvent(t.Context(), p, event))
	pending := make(chan error, 1)
	// The command is already dispatched before revocation, so its eventual outcome must remain uncertain.
	go func() { _, err := host.Invoke(t.Context(), p.AccountID, pin, []byte(`{}`)); pending <- err }()
	frame, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, frame.GetCall())
	liveClosed, idleClosed := make(chan error, 1), make(chan error, 1)
	go func() { _, err := stream.Recv(); liveClosed <- err }()
	go func() { _, err := idleStream.Recv(); idleClosed <- err }()
	repository := store.NewPostgresStore(repo.Pool).(store.OAuthClientStore)
	_, err = repository.RevokeOAuthConnectedApp(t.Context(), clientID, owner.SubjectID, consentMutationActor(t, repo, owner))
	require.NoError(t, err)
	// No retry, heartbeat, expiry or polling is allowed before checking that durable access is gone.
	_, err = repo.Authenticate(t.Context(), secret)
	require.ErrorIs(t, err, ErrDenied)
	_, err = repo.Authenticate(t.Context(), idleSecret)
	require.ErrorIs(t, err, ErrDenied)
	require.ErrorIs(t, repo.Renew(t.Context(), p.ID, p.Generation), ErrDenied)
	require.ErrorIs(t, repo.PutManifest(t.Context(), p, manifest), ErrDenied)
	event.EventId = uuid.NewString()
	require.ErrorIs(t, repo.AcceptEvent(t.Context(), p, event), ErrDenied)
	_, err = host.Invoke(t.Context(), p.AccountID, pin, []byte(`{}`))
	require.Error(t, err)
	eventPin := pin
	eventPin.OperationID = event.OperationId
	eventPin.ContractHash = event.ContractHash
	require.ErrorIs(t, host.AuthorizeEventPin(t.Context(), p.AccountID, eventPin), ErrDenied)
	// The one-second bound proves idle closure does not wait for the ten-second heartbeat.
	for _, closed := range []<-chan error{liveClosed, idleClosed} {
		select {
		case err := <-closed:
			require.Equal(t, codes.PermissionDenied, status.Code(err))
		case <-time.After(time.Second):
			t.Fatal("consent revocation did not close an active session promptly")
		}
	}
	select {
	case err := <-pending:
		var failure *InvocationError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, "unknown", failure.Outcome)
	case <-time.After(time.Second):
		t.Fatal("outstanding invocation did not terminate with its session")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reconnect, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret))
	require.NoError(t, err)
	_, err = reconnect.Recv()
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	// Revoking one client/user grant must leave other consent and direct-control pairings usable.
	_, err = repo.Authenticate(t.Context(), otherSecret)
	require.NoError(t, err)
	_, err = repo.Authenticate(t.Context(), controlSecret)
	require.NoError(t, err)
	_, _, err = repo.Create(t.Context(), delegated, uuid.NewString(), "Stale actor", "workspace")
	require.ErrorIs(t, err, ErrDenied)
	// Fresh consent cannot revive an old installation credential after a user explicitly disconnected it.
	_, err = repo.Pool.Exec(t.Context(), `INSERT INTO fused_oauth_user_consents(client_id,subject_id,granted_scope) VALUES($1,$2,ARRAY['bridge.manage'])`, clientID, owner.SubjectID)
	require.NoError(t, err)
	_, err = repo.Authenticate(t.Context(), secret)
	require.ErrorIs(t, err, ErrDenied)
}

// TestBridgeConsentMigrationFencesUnattributedInstallations verifies upgrade safety and migration idempotence.
func TestBridgeConsentMigrationFencesUnattributedInstallations(t *testing.T) {
	repo, actor := databaseFixture(t)
	legacy, legacySecret := pairingFixture(t, repo, actor)
	_, currentSecret := pairingFixture(t, repo, actor)
	_, err := repo.Pool.Exec(t.Context(), `UPDATE fused_bridge_pairings SET credential_source='legacy' WHERE id=$1`, legacy.ID)
	require.NoError(t, err)
	for range 2 {
		migrated, err := db.InitEnginePostgres(t.Context(), repo.Pool.Config().ConnString())
		require.NoError(t, err)
		migrated.Close()
	}
	_, err = repo.Authenticate(t.Context(), legacySecret)
	require.ErrorIs(t, err, ErrDenied)
	_, err = repo.Authenticate(t.Context(), currentSecret)
	require.NoError(t, err)
}

// consentMutationActor uses the fixture owner's actual control credential for audited consent mutations.
func consentMutationActor(t *testing.T, repo *Repository, owner accesscontrol.Actor) store.MutationActor {
	t.Helper()
	actor := store.MutationActor{SubjectID: owner.SubjectID, RequestID: uuid.NewString()}
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT id FROM fused_control_credentials WHERE subject_id=$1 AND revoked_at IS NULL LIMIT 1`, owner.SubjectID).Scan(&actor.CredentialID))
	return actor
}

// TestBridgeConsentRollbackPreservesSession ensures neither fencing nor notifications escape an aborted transaction.
func TestBridgeConsentRollbackPreservesSession(t *testing.T) {
	repo, owner := databaseFixture(t)
	actor, clientID := oauthBridgeActor(t, repo, owner)
	p, secret := pairingFixture(t, repo, actor)
	_, client := bridgeConsentTransport(t, repo)
	stream, _ := connectConsentBridge(t, client, p, secret)
	tx, err := repo.Pool.Begin(t.Context())
	require.NoError(t, err)
	defer tx.Rollback(t.Context())
	_, err = tx.Exec(t.Context(), `DELETE FROM fused_oauth_user_consents WHERE client_id=$1 AND subject_id=$2`, clientID, owner.SubjectID)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(t.Context()))
	_, err = repo.Authenticate(t.Context(), secret)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&bridgev1.BridgeFrame{Message: &bridgev1.BridgeFrame_Inventory{Inventory: inventoryFixture(t, "sensors")}}))
	frame, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, frame.GetInventoryAccepted())
}

// TestBridgeRevocationListenerLossClosesSessions verifies failure and recovery of the actual PostgreSQL notification connection.
func TestBridgeRevocationListenerLossClosesSessions(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, secret := pairingFixture(t, repo, actor)
	host, client := bridgeConsentTransport(t, repo)
	stream, _ := connectConsentBridge(t, client, p, secret)
	closed := make(chan error, 1)
	go func() { _, err := stream.Recv(); closed <- err }()
	var terminated bool
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, "bridge-revocation-"+host.instance).Scan(&terminated))
	require.True(t, terminated)
	select {
	case err := <-closed:
		require.Equal(t, codes.Unavailable, status.Code(err))
	case <-time.After(time.Second):
		t.Fatal("listener failure left an idle session open")
	}
	host.mu.Lock()
	ready := host.revocationsReady
	host.mu.Unlock()
	require.False(t, ready)
	// The watcher recovers automatically, but all previous sessions have already been cancelled.
	require.Eventually(t, func() bool { host.mu.Lock(); defer host.mu.Unlock(); return host.revocationsReady }, 3*time.Second, 10*time.Millisecond)
	replacement, _ := connectConsentBridge(t, client, p, secret)
	require.NoError(t, replacement.CloseSend())
}

// TestBridgePairingCannotRaceConsentDeletion verifies a stale authenticated actor cannot escape a deleting grant's lock.
func TestBridgePairingCannotRaceConsentDeletion(t *testing.T) {
	repo, owner := databaseFixture(t)
	actor, clientID := oauthBridgeActor(t, repo, owner)
	tx, err := repo.Pool.Begin(t.Context())
	require.NoError(t, err)
	defer tx.Rollback(t.Context())
	_, err = tx.Exec(t.Context(), `DELETE FROM fused_oauth_user_consents WHERE client_id=$1 AND subject_id=$2`, clientID, owner.SubjectID)
	require.NoError(t, err)
	done := make(chan error, 1)
	// The real Create transaction must wait behind the consent deletion instead of minting an unlinked credential.
	go func() {
		_, _, err := repo.Create(t.Context(), actor, uuid.NewString(), "Racing consent deletion", "workspace")
		done <- err
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := repo.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%FOR SHARE OF consent, token, client%')`).Scan(&blocked)
		return err == nil && blocked
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, tx.Commit(t.Context()))
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrDenied)
	case <-time.After(time.Second):
		t.Fatal("pairing did not finish after consent deletion committed")
	}
}
