package capability

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Usefused/engine/internal/shared/models"
)

func TestKeysCapturesCapabilitySurfaceWithoutInjectionValues(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	endpointID, webhookID := uuid.New(), uuid.New()
	raw, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID,
		EndpointIDs: []uuid.UUID{endpointID}, OperationNames: []string{"list"},
		WebhookIDs: []uuid.UUID{webhookID}, WebhookNames: []string{"created"},
		SelectAll: true, WebhookSelectAll: true,
		AuthType: "oauth2", AuthName: "oauth", ConnectScopes: []string{"read"},
		Injections: []models.SDKInjectionConfig{{
			Location: "header", Name: "X-Tenant", Mode: "required", Value: "secret://tenant",
		}},
	}})
	require.NoError(t, err)

	keys, err := Keys(raw)
	require.NoError(t, err)
	prefix := "service:" + serviceID.String() + ":" + versionID.String()
	assert.Contains(t, keys, prefix+":endpoint:"+endpointID.String())
	assert.Contains(t, keys, prefix+":operation:list")
	assert.Contains(t, keys, prefix+":webhook:"+webhookID.String())
	assert.Contains(t, keys, prefix+":connect-scope:read")
	assert.Contains(t, keys, prefix+":auth:oauth2:oauth")
	assert.Contains(t, keys, prefix+":injection:header:X-Tenant:required")
	for _, key := range keys {
		assert.NotContains(t, key, "secret://tenant")
	}
}

func TestKeysIsStableAcrossInputOrdering(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	first := selectionJSON(t, serviceID, versionID, []string{"create", "list"})
	second := selectionJSON(t, serviceID, versionID, []string{"list", "create"})

	firstKeys, err := Keys(first)
	require.NoError(t, err)
	secondKeys, err := Keys(second)
	require.NoError(t, err)
	assert.Equal(t, firstKeys, secondKeys)
}

func selectionJSON(t *testing.T, serviceID, versionID uuid.UUID, operations []string) []byte {
	t.Helper()
	raw, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: operations,
	}})
	require.NoError(t, err)
	return raw
}
