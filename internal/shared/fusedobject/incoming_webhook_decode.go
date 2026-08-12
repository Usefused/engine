package fusedobject

import (
	"encoding/json"
	"errors"
)

// UnmarshalJSON is the snapshot-side compatibility guard: legacy plaintext is
// detected and rejected without ever entering the canonical runtime object.
func (config *IncomingWebhookConfig) UnmarshalJSON(data []byte) error {
	type canonical IncomingWebhookConfig
	var wire struct {
		canonical
		SigningSecret string `json:"signing_secret"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.SigningSecret != "" {
		return errors.New("plaintext webhook signing_secret is unsupported; configure secret_ref")
	}
	*config = IncomingWebhookConfig(wire.canonical)
	return nil
}
