package managedauthbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/shared/managedpublication"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrProviderAppNotFound = errors.New("published OAuth registration unavailable")

// Registration selects existing Engine-owned material; publication never copies credentials or provider policy.
type Registration struct {
	ApplicationID    string                        `json:"managed_application_id,omitempty"`
	Name             string                        `json:"name,omitempty"`
	OwnerAccountID   *uuid.UUID                    `json:"owner_account_id,omitempty"`
	AllowAllEnrolled *bool                         `json:"allow_all_enrolled,omitempty"`
	AllowedConsumers []managedpublication.Audience `json:"allowed_consumers,omitempty"`
	BucketID         uuid.UUID                     `json:"bucket_id"`
	ServiceVersionID uuid.UUID                     `json:"service_version_id"`
	FlowName         string                        `json:"flow_name"`
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
	// Validate identity and audience before resolving any credential material.
	if err := validateRegistration(registration); err != nil {
		return err
	}
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
	// Legacy defaults retain enrollment-wide access; new named apps default to no consumers.
	allowAll := registration.ApplicationID == ""
	if registration.AllowAllEnrolled != nil {
		allowAll = *registration.AllowAllEnrolled
	}
	audience, err := json.Marshal(registration.AllowedConsumers)
	// Audience serialization must complete before changing an operator grant.
	if err != nil {
		return err
	}
	result, err := s.database.Exec(ctx, `INSERT INTO fused_oauth_publications(service_id,auth_name,bucket_id,service_version_id,flow_name,registration_hash,application_id,name,owner_account_id,allow_all_enrolled,allowed_consumers)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,COALESCE(NULLIF($11::jsonb,'null'::jsonb),'[]'::jsonb))
 ON CONFLICT(service_id,auth_name,application_id) DO UPDATE SET name=EXCLUDED.name,allow_all_enrolled=EXCLUDED.allow_all_enrolled,allowed_consumers=EXCLUDED.allowed_consumers
 WHERE fused_oauth_publications.bucket_id=EXCLUDED.bucket_id AND fused_oauth_publications.service_version_id=EXCLUDED.service_version_id
 AND fused_oauth_publications.flow_name=EXCLUDED.flow_name AND fused_oauth_publications.registration_hash=EXCLUDED.registration_hash
 AND fused_oauth_publications.owner_account_id IS NOT DISTINCT FROM EXCLUDED.owner_account_id`, serviceID, authName, registration.BucketID, registration.ServiceVersionID, registration.FlowName, fingerprint, registration.ApplicationID, registration.Name, registration.OwnerAccountID, allowAll, audience)
	// Repeated publication is idempotent, but must never retarget an existing consumer grant.
	if err != nil || result.RowsAffected() != 1 {
		return ErrProviderAppNotFound
	}
	return nil
}

// GetProviderApp resolves only an explicit publication and fails closed if its contract or client identity has changed.
func (s *CatalogStore) GetProviderApp(ctx context.Context, serviceID uuid.UUID, authName string, key []byte, applicationIDs ...string) (ProviderApp, error) {
	applicationID, err := managedpublication.Selector(applicationIDs)
	// An invalid selector must never degrade to the default application.
	if err != nil {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	installation, ok := ctx.Value(installationContextKey{}).(Installation)
	// Internal callers must carry the same authenticated installation as HTTP callers.
	if !ok {
		return ProviderApp{}, ErrProviderAppNotFound
	}
	var registration Registration
	var expected string
	err = s.database.QueryRow(ctx, `SELECT p.bucket_id,p.service_version_id,p.flow_name,p.registration_hash FROM fused_oauth_publications p
 JOIN fused_managed_auth_installations i ON i.id=$4
 WHERE p.service_id=$1 AND p.auth_name=$2 AND p.application_id=$3
 AND i.revoked_at IS NULL AND i.access_expires_at>NOW() AND `+managedpublication.AudienceSQL, serviceID, authName, applicationID, installation.ID).Scan(&registration.BucketID, &registration.ServiceVersionID, &registration.FlowName, &expected)
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

// validateRegistration bounds operator metadata and rejects ambiguous consumer grants without inventing provider-specific policy.
func validateRegistration(r Registration) error {
	_, err := managedpublication.Selector([]string{r.ApplicationID})
	// Named applications require accountable ownership and a human-readable name.
	if err != nil || !validRegistrationOwner(r) {
		return ErrProviderAppNotFound
	}
	// Keep publication writes and audience checks bounded even for operator mistakes.
	if len(r.AllowedConsumers) > 64 {
		return ErrProviderAppNotFound
	}
	for _, audience := range r.AllowedConsumers {
		// Grants always identify both a verified account and Engine, never a wildcard.
		if audience.AccountID == uuid.Nil || audience.EngineInstallationID == uuid.Nil {
			return ErrProviderAppNotFound
		}
	}
	return nil
}

// validRegistrationOwner requires accountable metadata for named apps while preserving existing default registrations.
func validRegistrationOwner(r Registration) bool {
	// Metadata is bounded even on the legacy default publication.
	if len(r.Name) > 200 {
		return false
	}
	return r.ApplicationID == "" || (strings.TrimSpace(r.Name) != "" && r.OwnerAccountID != nil && *r.OwnerAccountID != uuid.Nil)
}

// RevokeRegistrationAccess withdraws every consumer grant without depending on the availability of credential material.
func (s *CatalogStore) RevokeRegistrationAccess(ctx context.Context, serviceID uuid.UUID, authName, applicationID string) error {
	_, err := managedpublication.Selector([]string{applicationID})
	// Revocation requires one exact identity; omission means the reserved default, never all applications.
	if err != nil || serviceID == uuid.Nil || authName == "" {
		return ErrProviderAppNotFound
	}
	result, err := s.database.Exec(ctx, `UPDATE fused_oauth_publications SET allow_all_enrolled=false,allowed_consumers='[]'::jsonb WHERE service_id=$1 AND auth_name=$2 AND application_id=$3`, serviceID, authName, applicationID)
	// Preserve the identity tombstone while making repeated withdrawal idempotent.
	if err != nil || result.RowsAffected() != 1 {
		return ErrProviderAppNotFound
	}
	return nil
}
