package capability

import (
	"github.com/Usefused/engine/internal/shared/models"
	"sort"
)

// KeysAndHashWithBridges extends the canonical reducer with separately admitted local bindings.
// Physical selection JSON cannot override or masquerade as this server-owned capability list.
func KeysAndHashWithBridges(raw []byte, bindings []models.BridgeBinding, attachments ...models.UnifiedAppBinding) ([]string, string, error) {
	keys, hash, err := KeysAndHash(raw, attachments...)
	// Ordinary apps retain exactly their historical keys and digest, including error behavior.
	if err != nil || len(bindings) == 0 {
		return keys, hash, err
	}
	for _, b := range bindings {
		identity := hashKeys([]string{b.PairingID, b.ProfileID, b.ManifestHash, b.ConnectionID, b.OperationID, b.ContractHash})
		keys = append(keys, "bridge:"+identity+":operation:bridge:"+b.Alias)
	}
	sort.Strings(keys)
	return keys, hashKeys(keys), nil
}
