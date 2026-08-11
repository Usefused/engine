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
		RequiredAuth: []models.SDKRequiredAuth{{
			AuthType: "oauth2", AuthName: "oauth",
		}, {
			AuthType: "mtls", AuthName: "clientCertificate",
		}},
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
	assert.Contains(t, keys, prefix+":required-auth:oauth2:oauth")
	assert.Contains(t, keys, prefix+":required-auth:mtls:clientCertificate")
	assert.Contains(t, keys, prefix+":injection:header:X-Tenant:required")
	for _, key := range keys {
		assert.NotContains(t, key, "secret://tenant")
	}
}

func TestKeysAndHashIncludesRequiredAuthDeterministically(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	first := requiredAuthSelectionJSON(t, serviceID, versionID, []models.SDKRequiredAuth{{
		AuthType: "api_key", AuthName: "apiKey",
	}, {
		AuthType: "basic", AuthName: "apiToken", BasicPasswordMode: "empty",
	}})
	second := requiredAuthSelectionJSON(t, serviceID, versionID, []models.SDKRequiredAuth{{
		AuthType: "basic", AuthName: "apiToken", BasicPasswordMode: "empty",
	}, {
		AuthType: "api_key", AuthName: "apiKey",
	}})

	firstKeys, firstHash, err := KeysAndHash(first)
	require.NoError(t, err)
	secondKeys, secondHash, err := KeysAndHash(second)
	require.NoError(t, err)
	assert.Equal(t, firstKeys, secondKeys)
	assert.Equal(t, firstHash, secondHash)

	changed := requiredAuthSelectionJSON(t, serviceID, versionID, []models.SDKRequiredAuth{{
		AuthType: "api_key", AuthName: "apiKey",
	}})
	_, changedHash, err := KeysAndHash(changed)
	require.NoError(t, err)
	assert.NotEqual(t, firstHash, changedHash)
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

func TestKeysAndHashIsStableAcrossInputOrdering(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	first := selectionJSON(t, serviceID, versionID, []string{"create", "list"})
	second := selectionJSON(t, serviceID, versionID, []string{"list", "create"})

	firstKeys, firstHash, err := KeysAndHash(first)
	require.NoError(t, err)
	secondKeys, secondHash, err := KeysAndHash(second)
	require.NoError(t, err)

	assert.Equal(t, firstKeys, secondKeys)
	assert.Equal(t, firstHash, secondHash)
	assert.Len(t, firstHash, 64)
}

func TestCapabilityHashUsesUnambiguousKeyEncoding(t *testing.T) {
	first := hashKeys([]string{"a\nb", "c"})
	second := hashKeys([]string{"a", "b\nc"})
	if first == second {
		t.Fatal("distinct capability key sets must not collide at delimiters")
	}
}

func selectionJSON(t *testing.T, serviceID, versionID uuid.UUID, operations []string) []byte {
	t.Helper()
	raw, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: operations,
	}})
	require.NoError(t, err)
	return raw
}

func requiredAuthSelectionJSON(t *testing.T, serviceID, versionID uuid.UUID, required []models.SDKRequiredAuth) []byte {
	t.Helper()
	raw, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, RequiredAuth: required,
	}})
	require.NoError(t, err)
	return raw
}
