package managedauthbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrProviderAppNotFound = errors.New("published OAuth registration unavailable")

// Registration selects existing Engine-owned material; publication never copies credentials or provider policy.
type Registration struct {
	BucketID         uuid.UUID `json:"bucket_id"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	FlowName         string    `json:"flow_name"`
}

// registrationRuntime exposes only the existing credential and exact contract readers needed at the broker boundary.
type registrationRuntime interface {
	connectauth.ApplicationCredentialStore
	GetServiceContractMetadata(context.Context, uuid.UUID, uuid.UUID) (*fusedobject.ServiceMetadata, error)
}

type CatalogStore struct {
	database *pgxpool.Pool
	runtime  registrationRuntime
}

// NewCatalogStore reuses the Engine's bucket and snapshot store without owning another secret repository.
func NewCatalogStore(database *pgxpool.Pool) *CatalogStore {
	return &CatalogStore{database: database, runtime: store.NewPostgresStore(database).(registrationRuntime)}
}

// ProviderApp is an ephemeral projection of the canonical bucket pair and pinned service auth contract.
type ProviderApp struct {
	ClientID     string
	ClientSecret string
	Auth         fusedobject.AuthConfig
	Flow         fusedobject.OAuth2FlowContract
}

// PublishRegistration validates canonical material before exposing one exact registration for delegated use.
func (s *CatalogStore) PublishRegistration(ctx context.Context, serviceID uuid.UUID, authName string, registration Registration, key []byte) error {
	app, err := s.resolve(ctx, serviceID, authName, registration, key)
	// Only a complete, approved local registration can be published.
	if err != nil {
		return err
	}
	fingerprint, err := registrationFingerprint(app)
	// Fingerprinting must succeed before publication can acquire a durable identity.
	if err != nil {
		return err
	}
	result, err := s.database.Exec(ctx, `INSERT INTO fused_oauth_publications(service_id,auth_name,bucket_id,service_version_id,flow_name,registration_hash)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(service_id,auth_name) DO UPDATE SET registration_hash=EXCLUDED.registration_hash
 WHERE fused_oauth_publications.bucket_id=EXCLUDED.bucket_id AND fused_oauth_publications.service_version_id=EXCLUDED.service_version_id
 AND fused_oauth_publications.flow_name=EXCLUDED.flow_name AND fused_oauth_publications.registration_hash=EXCLUDED.registration_hash`, serviceID, authName, registration.BucketID, registration.ServiceVersionID, registration.FlowName, fingerprint)
	// Repeated publication is idempotent, but must never retarget an existing consumer grant.
	if err != nil || result.RowsAffected() != 1 {
		return ErrProviderAppNotFound
	}
	return nil
}

// GetProviderApp resolves only an explicit publication and fails closed if its contract or client identity has changed.
func (s *CatalogStore) GetProviderApp(ctx context.Context, serviceID uuid.UUID, authName string, key []byte) (ProviderApp, error) {
	var registration Registration
	var expected string
	err := s.database.QueryRow(ctx, `SELECT bucket_id,service_version_id,flow_name,registration_hash FROM fused_oauth_publications WHERE service_id=$1 AND auth_name=$2`, serviceID, authName).Scan(&registration.BucketID, &registration.ServiceVersionID, &registration.FlowName, &expected)
	// Unpublished services and unavailable storage must never fall back to arbitrary bucket reads.
	if err != nil {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	app, err := s.resolve(ctx, serviceID, authName, registration, key)
	// A missing canonical source cannot be repaired by another storage path.
	if err != nil {
		return ProviderApp{}, err
	}
	actual, err := registrationFingerprint(app)
	// Secret rotation is allowed; provider-client or contract changes require a separate publication identity.
	if err != nil || actual != expected {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	return app, nil
}

// resolve composes existing exact contract and credential resolvers instead of introducing a second OAuth model.
func (s *CatalogStore) resolve(ctx context.Context, serviceID uuid.UUID, authName string, r Registration, key []byte) (ProviderApp, error) {
	// Explicit identities prevent empty selectors from resolving latest or another credential family.
	if r.BucketID == uuid.Nil || r.ServiceVersionID == uuid.Nil || serviceID == uuid.Nil || authName == "" {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	metadata, err := s.runtime.GetServiceContractMetadata(ctx, serviceID, r.ServiceVersionID)
	// Missing or corrupt snapshots cannot be repaired from consumer-supplied metadata.
	if err != nil || metadata == nil {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	auth, flow, err := publishedAuthContract(metadata.AuthConfigs, authName, r.FlowName)
	// A missing canonical source cannot be repaired by another storage path.
	if err != nil {
		return ProviderApp{}, err
	}
	creds, err := connectauth.NewApplicationCredentialResolver(s.runtime, key, "").Resolve(ctx, r.BucketID, serviceID, "oauth", authName)
	// The shared resolver admits only the exact OAuth client pair, never API keys or similarly named values.
	if err != nil {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	return ProviderApp{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, Auth: auth, Flow: flow}, nil
}

// registrationFingerprint pins public client identity and auth policy without preventing ordinary client-secret rotation.
func registrationFingerprint(app ProviderApp) (string, error) {
	payload, err := json.Marshal(struct {
		ClientID string
		Auth     fusedobject.AuthConfig
		Flow     fusedobject.OAuth2FlowContract
	}{app.ClientID, app.Auth, app.Flow})
	// A failed encoding must not yield a reusable default fingerprint.
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
