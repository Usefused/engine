package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

// KeysAndHash returns the canonical, non-secret capability surface and its
// stable digest. Every app publication path must use this function so
// semantically identical selection documents cannot acquire different hashes
// because of JSON formatting or input ordering.
func KeysAndHash(raw []byte) ([]string, string, error) {
	keys, err := Keys(raw)
	if err != nil {
		return nil, "", err
	}
	return keys, hashKeys(keys), nil
}

func hashKeys(keys []string) string {
	encoded := make([]byte, 0)
	for _, key := range keys {
		// Length-prefixing makes the set encoding unambiguous even when provider
		// operation names contain delimiters or control-like characters.
		encoded = strconv.AppendInt(encoded, int64(len([]byte(key))), 10)
		encoded = append(encoded, ':')
		encoded = append(encoded, key...)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

// Keys reduces an SDK/MCP selection document to its non-secret authorization
// surface. Injection values are deliberately excluded because they may carry
// Engine-local secret references and do not change what a family token grants.
func Keys(raw []byte) ([]string, error) {
	var selections []models.SDKSelection
	if err := json.Unmarshal(raw, &selections); err != nil {
		return nil, err
	}
	keys := make(map[string]struct{})
	for _, selection := range selections {
		addSelection(keys, selection)
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result, nil
}

func addSelection(keys map[string]struct{}, selection models.SDKSelection) {
	prefix := "service:" + selection.ServiceID.String() + ":" + selection.ServiceVersionID.String()
	keys[prefix] = struct{}{}
	addSelectAll(keys, prefix, selection)
	addUUIDs(keys, prefix+":endpoint:", selection.EndpointIDs)
	addUUIDs(keys, prefix+":webhook:", selection.WebhookIDs)
	addStrings(keys, prefix+":operation:", selection.OperationNames)
	addStrings(keys, prefix+":webhook-name:", selection.WebhookNames)
	addStrings(keys, prefix+":connect-scope:", selection.ConnectScopes)
	addAuth(keys, prefix, selection)
	addInjections(keys, prefix, selection.Injections)
}

func addSelectAll(keys map[string]struct{}, prefix string, selection models.SDKSelection) {
	if selection.SelectAll {
		keys[prefix+":operations:*"] = struct{}{}
	}
	if selection.WebhookSelectAll {
		keys[prefix+":webhooks:*"] = struct{}{}
	}
}

func addAuth(keys map[string]struct{}, prefix string, selection models.SDKSelection) {
	// Credential routing changes provider identity even though it grants no additional operation capability.
	if selection.CredentialSourceServiceID != uuid.Nil || selection.CredentialSourceAuthType != "" || selection.CredentialSourceAuthName != "" {
		keys[prefix+":credential-source:"+selection.CredentialSourceServiceID.String()+":"+selection.CredentialSourceAuthType+":"+selection.CredentialSourceAuthName] = struct{}{}
	}
	// The selected target scheme remains part of dispatch authority independently of its application credential source.
	if selection.AuthType != "" {
		keys[prefix+":auth:"+selection.AuthType+":"+selection.AuthName] = struct{}{}
	}
	for _, required := range selection.RequiredAuth {
		// Required AND-members constrain publication readiness even when no
		// singular runtime selector exists, so they are part of immutable identity.
		key := prefix + ":required-auth:" + required.AuthType + ":" + required.AuthName
		if required.BasicPasswordMode != "" {
			key += ":basic-password-mode:" + string(required.BasicPasswordMode)
		}
		keys[key] = struct{}{}
	}
}

func addInjections(keys map[string]struct{}, prefix string, injections []models.SDKInjectionConfig) {
	for _, injection := range injections {
		key := prefix + ":injection:" + injection.Location + ":" + injection.Name + ":" + injection.Mode
		keys[key] = struct{}{}
	}
}

func addUUIDs(keys map[string]struct{}, prefix string, values []uuid.UUID) {
	for _, value := range values {
		keys[prefix+value.String()] = struct{}{}
	}
}

func addStrings(keys map[string]struct{}, prefix string, values []string) {
	for _, value := range values {
		keys[prefix+value] = struct{}{}
	}
}
