package bridgehost

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var ErrDenied = errors.New("bridge access denied")
var ErrConflict = errors.New("bridge identity conflict")
var ErrCapacity = errors.New("bridge delivery capacity exhausted")

// Pairing binds the installation credential to one destination and explicitly consented workspace scope.
type Pairing struct {
	DurableCalls   bool     `json:"-"`
	ID             string   `json:"pairing_id"`
	DestinationID  string   `json:"destination_id"`
	InstallationID string   `json:"installation_id"`
	Name           string   `json:"name"`
	Scope          string   `json:"scope"`
	Profiles       []string `json:"profile_ids"`
	AccountID      string   `json:"account_id"`
	WorkspaceID    string   `json:"workspace_id"`
	Generation     uint64   `json:"generation"`
}

// Repository uses shared database state so replica changes cannot widen identity or replay scope.
type Repository struct{ Pool *pgxpool.Pool }

// ownerAuthorized checks live subject/team/workspace grants in one bounded set-based query.
const ownerAuthorized = `EXISTS (
 SELECT 1 FROM fused_subjects s JOIN fused_role_bindings b ON
 ((b.subject_type='subject' AND b.subject_id=s.id) OR
  (b.subject_type='workspace' AND b.subject_id=p.workspace_id) OR
  (b.subject_type='team' AND b.subject_id IN (SELECT m.team_id FROM fused_team_memberships m JOIN fused_teams t ON t.id=m.team_id AND t.status='active' WHERE m.member_subject_id=s.id)))
 JOIN fused_roles r ON r.id=b.role_id AND r.scope_type=b.resource_type
 JOIN fused_role_permissions permission ON permission.role_id=r.id
 WHERE s.id=p.subject_id AND s.status='active' AND b.resource_type='workspace'
 AND b.resource_id=p.workspace_id AND permission.permission='bridge.manage')`

// Create issues a non-general-purpose credential only after ordinary control authorization.
func (r *Repository) Create(ctx context.Context, actor accesscontrol.Actor, installation, name string, scope string) (Pairing, string, error) {
	// Reject ambiguous identities before issuing irreversible secret material.
	if _, err := uuid.Parse(installation); err != nil || len(name) == 0 || len(name) > 200 || scope != "workspace" {
		return Pairing{}, "", ErrContract
	}
	secretBytes := make([]byte, 32)
	// Entropy failures must never produce predictable installation credentials.
	if _, err := rand.Read(secretBytes); err != nil {
		return Pairing{}, "", err
	}
	secret := "fbc_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	p := Pairing{ID: uuid.NewString(), DestinationID: uuid.NewString(), InstallationID: installation, Name: name, Scope: "workspace", Profiles: []string{}, AccountID: actor.AccountID.String(), WorkspaceID: actor.WorkspaceID.String()}
	_, err := r.Pool.Exec(ctx, `INSERT INTO fused_bridge_pairings(id,account_id,workspace_id,subject_id,installation_id,destination_id,name,credential_hash,profile_ids,scope) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'{}',$9)`, p.ID, p.AccountID, p.WorkspaceID, actor.SubjectID, installation, p.DestinationID, name, Digest([]byte(secret)), scope)
	// Never expose a credential whose pairing failed to commit.
	if err != nil {
		return Pairing{}, "", err
	}
	return p, secret, nil
}

// Authenticate checks revocation and current owner permissions without token caching.
func (r *Repository) Authenticate(ctx context.Context, secret string) (Pairing, error) {
	var p Pairing
	// Reject unrelated control/OAuth credentials at the installation-only boundary.
	if len(secret) < 5 || secret[:4] != "fbc_" {
		return p, ErrDenied
	}
	err := r.Pool.QueryRow(ctx, `SELECT p.id,p.destination_id,p.installation_id,p.name,p.scope,p.profile_ids,p.account_id,p.workspace_id,p.generation FROM fused_bridge_pairings p WHERE p.credential_hash=$1 AND p.scope='workspace' AND p.revoked_at IS NULL AND `+ownerAuthorized, Digest([]byte(secret))).Scan(&p.ID, &p.DestinationID, &p.InstallationID, &p.Name, &p.Scope, &p.Profiles, &p.AccountID, &p.WorkspaceID, &p.Generation)
	// Credential failures intentionally reveal no pairing existence information.
	if err != nil {
		return Pairing{}, ErrDenied
	}
	return p, nil
}

// Claim fences prior owners by advancing a persisted generation before session readiness.
func (r *Repository) Claim(ctx context.Context, p Pairing, instance string) (uint64, error) {
	var generation uint64
	err := r.Pool.QueryRow(ctx, `UPDATE fused_bridge_pairings SET generation=generation+1,owner_instance=$2,lease_until=now()+interval '30 seconds' WHERE id=$1 AND scope='workspace' AND revoked_at IS NULL RETURNING generation`, p.ID, instance).Scan(&generation)
	return generation, err
}

// Renew never resurrects a revoked or superseded session.
func (r *Repository) Renew(ctx context.Context, pairing string, generation uint64) error {
	result, err := r.Pool.Exec(ctx, `UPDATE fused_bridge_pairings p SET lease_until=now()+interval '30 seconds' WHERE id=$1 AND generation=$2 AND scope='workspace' AND revoked_at IS NULL AND `+ownerAuthorized, pairing, generation)
	// Zero affected rows means the session no longer owns this installation.
	if err != nil || result.RowsAffected() != 1 {
		return ErrDenied
	}
	return nil
}

// PutManifest archives revisions and advances current catalogue only for live declarations.
func (r *Repository) PutManifest(ctx context.Context, p Pairing, frame *bridgev1.Manifest) error {
	doc, err := ValidateManifest(frame)
	// Invalid contracts never reach persistent catalogue state.
	if err != nil {
		return err
	}
	tx, err := r.Pool.Begin(ctx)
	// Archive and current pointer must share one transaction.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Lock authorization against concurrent revocation and inventory changes.
	if err = lockSession(ctx, tx, p, doc.ProfileID); err != nil {
		return err
	}
	var hash string
	err = tx.QueryRow(ctx, `INSERT INTO fused_bridge_manifests(pairing_id,profile_id,revision,hash,document) VALUES($1,$2,$3,$4,$5) ON CONFLICT(pairing_id,profile_id,revision) DO UPDATE SET hash=fused_bridge_manifests.hash RETURNING hash`, p.ID, doc.ProfileID, doc.Revision, frame.Hash, frame.ManifestJson).Scan(&hash)
	// Revision reuse with changed content cannot update an immutable app dependency.
	if err != nil {
		return err
	}
	if hash != frame.Hash {
		return ErrConflict
	}
	// Historical replay must not roll the active catalogue backwards.
	if !frame.Historical {
		_, err = tx.Exec(ctx, `UPDATE fused_bridge_profiles SET state=$4,manifest_hash=CASE WHEN $4='offline' AND $5=0 THEN COALESCE(manifest_hash,$3) ELSE $3 END WHERE pairing_id=$1 AND profile_id=$2`, p.ID, doc.ProfileID, frame.Hash, manifestState(*doc), len(doc.Operations))
		// Failed pointer publication leaves neither change committed.
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// lockSession serializes acceptance with revocation, generation changes and inventory withdrawal.
func lockSession(ctx context.Context, tx pgx.Tx, p Pairing, profile string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM fused_bridge_pairings p WHERE id=$1 AND generation=$2 AND scope='workspace' AND inventory_generation=generation AND revoked_at IS NULL AND lease_until>now() AND EXISTS(SELECT 1 FROM fused_bridge_profiles c WHERE c.pairing_id=p.id AND c.profile_id=$3 AND c.state<>'removed') AND `+ownerAuthorized+` FOR UPDATE`, p.ID, p.Generation, profile).Scan(&id)
	// No matching authorization is a terminal denial rather than a storage retry.
	if err != nil {
		return ErrDenied
	}
	return nil
}

// operation finds an exact immutable contract without consulting the current catalogue pointer.
func operation(doc ManifestDocument, connection, id, hash string) (Contract, error) {
	for _, op := range doc.Operations {
		// Both local connection identity and contract pin must match the authored selection.
		if op.ConnectionID == connection && op.OperationID == id && op.ContractHash == hash {
			c, err := op.Contract()
			if err != nil {
				return Contract{}, err
			}
			return *c, nil
		}
	}
	return Contract{}, ErrContract
}

// AcceptEvent commits deduplication and the pending delivery record before issuing an ACK.
func (r *Repository) AcceptEvent(ctx context.Context, p Pairing, event *bridgev1.Event) error {
	raw, err := eventEnvelope(p, event)
	// Invalid routing or oversized payloads cannot enter durable acceptance.
	if err != nil {
		return err
	}
	tx, err := r.Pool.Begin(ctx)
	// Delivery responsibility can transfer only through a committed database transaction.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// One transaction binds session fencing, deduplication and outbox insertion.
	if err = acceptEventTx(ctx, tx, p, event, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// eventEnvelope binds authenticated destination identity before creating deterministic deduplication bytes.
func eventEnvelope(p Pairing, event *bridgev1.Event) ([]byte, error) {
	// Destination binding comes from authentication, never from untrusted routing metadata.
	if event == nil || event.DestinationId != p.DestinationID || !identifier.MatchString(event.EventId) || len(event.PayloadJson) > MaxPayload {
		return nil, ErrContract
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
	// Deterministic envelope bytes make changed-content ID reuse detectable.
	if err != nil {
		return nil, ErrContract
	}
	return raw, nil
}

// acceptEventTx serializes duplicate checks and capacity accounting under the authenticated pairing lock.
func acceptEventTx(ctx context.Context, tx pgx.Tx, p Pairing, event *bridgev1.Event, raw []byte) error {
	if err := lockSession(ctx, tx, p, event.ProfileId); err != nil {
		return err
	}
	var prior string
	err := tx.QueryRow(ctx, `SELECT content_hash FROM fused_bridge_events WHERE pairing_id=$1 AND destination_id=$2 AND event_id=$3`, p.ID, p.DestinationID, event.EventId).Scan(&prior)
	// A lost ACK is safe to repeat only for the identical event.
	if err == nil {
		if prior != Digest(raw) {
			return ErrConflict
		}
		return nil
	}
	// Unexpected read failures must never be interpreted as a new event.
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err = validateEventTx(ctx, tx, p, event); err != nil {
		return err
	}
	// Capacity must be reserved before the insertion, never recovered by evicting accepted events.
	if err = checkEventCapacity(ctx, tx, p.ID, len(raw)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fused_bridge_events(pairing_id,destination_id,event_id,profile_id,manifest_hash,content_hash,envelope) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.ID, p.DestinationID, event.EventId, event.ProfileId, event.ManifestHash, Digest(raw), raw)
	// The persisted envelope is itself the delivery outbox; there is no broker/database dual write.
	if err != nil {
		return err
	}
	return nil
}

// validateEventTx enforces the original event schema before accepting delivery responsibility.
func validateEventTx(ctx context.Context, tx pgx.Tx, p Pairing, event *bridgev1.Event) error {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT document FROM fused_bridge_manifests WHERE pairing_id=$1 AND profile_id=$2 AND hash=$3`, p.ID, event.ProfileId, event.ManifestHash).Scan(&raw)
	// Unknown historical schemas must be synchronized before replay acceptance.
	if err != nil {
		return ErrContract
	}
	var doc ManifestDocument
	if json.Unmarshal(raw, &doc) != nil {
		return ErrContract
	}
	contract, err := operation(doc, event.ConnectionId, event.OperationId, event.ContractHash)
	// Unary contracts must never be used to smuggle unsolicited events.
	if err != nil || contract.Kind != "event" {
		return ErrContract
	}
	// Reject nonsensical timestamps while allowing genuinely old offline captures.
	if event.CapturedUnixMs <= 0 || event.CapturedUnixMs > time.Now().Add(5*time.Minute).UnixMilli() {
		return ErrContract
	}
	return ValidatePayload(contract.OutputSchema, event.PayloadJson)
}

// checkEventCapacity bounds pending ingress while retaining all accepted events during downstream outages.
func checkEventCapacity(ctx context.Context, tx pgx.Tx, pairing string, size int) error {
	var count, total int64
	err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(envelope)),0) FROM fused_bridge_events WHERE pairing_id=$1 AND delivered_at IS NULL`, pairing).Scan(&count, &total)
	// Reject new work instead of evicting unprocessed events under disk pressure.
	if err != nil {
		return err
	}
	if count >= 100000 || total+int64(size) > 256<<20 {
		return ErrCapacity
	}
	return nil
}
