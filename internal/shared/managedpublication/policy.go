// Package managedpublication defines the provider-independent identity and audience of a broker publication.
package managedpublication

import (
	"errors"

	"github.com/google/uuid"
)

// Audience names a Registry-verified account and Engine, never a caller-supplied tenant label.
type Audience struct {
	AccountID            uuid.UUID `json:"account_id"`
	EngineInstallationID uuid.UUID `json:"engine_installation_id"`
}

// Selector preserves the immutable legacy default while requiring canonical UUIDs for named applications.
func Selector(ids []string) (string, error) {
	// Older callers select the permanently reserved default publication.
	if len(ids) == 0 {
		return "", nil
	}
	// Multiple selectors would make the credential identity ambiguous.
	if len(ids) != 1 {
		return "", errors.New("one managed application is required")
	}
	// Empty selects the legacy default; it never means any available application.
	if ids[0] == "" {
		return "", nil
	}
	id, err := uuid.Parse(ids[0])
	// Canonical nonzero IDs keep SQL, config hashes and HTTP selectors consistent.
	if err != nil || id == uuid.Nil || id.String() != ids[0] {
		return "", errors.New("managed_application_id must be a canonical nonzero UUID")
	}
	return ids[0], nil
}

// AudienceSQL rechecks live operator grants against authenticated installation i and publication p.
const AudienceSQL = `(p.allow_all_enrolled OR p.allowed_consumers @> jsonb_build_array(jsonb_build_object('account_id',i.registry_account_id::text,'engine_installation_id',i.engine_installation_id::text)))`

// WebhookPublicationSQL binds a webhook to its exact application, preserving omitted legacy defaults.
const WebhookPublicationSQL = `p.service_id=w.service_id AND p.auth_name=w.relay_config->'publish'->>'auth_name' AND p.application_id=COALESCE(w.relay_config->'publish'->>'managed_application_id','')`
