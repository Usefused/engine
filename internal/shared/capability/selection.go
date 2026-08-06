package capability

import (
	"encoding/json"
	"sort"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

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
	if selection.AuthType != "" {
		keys[prefix+":auth:"+selection.AuthType+":"+selection.AuthName] = struct{}{}
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
