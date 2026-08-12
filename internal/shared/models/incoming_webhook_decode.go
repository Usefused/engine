package models

import (
	"encoding/json"
	"errors"
)

// UnmarshalJSON recognizes the removed plaintext field only to reject it.
// Secret values are never auto-converted into references or retained in the
// canonical runtime object.
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
