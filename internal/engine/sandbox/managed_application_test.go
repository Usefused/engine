package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestManagedApplicationExecutionIdentity prevents SDK/MCP use from silently crossing applications sharing one scheme.
func TestManagedApplicationExecutionIdentity(t *testing.T) {
	service, application := uuid.New(), uuid.NewString()
	connection := &store.AuthConnection{ServiceID: service, AuthName: "oauth", ManagedAuth: true, ManagedApplicationID: application, CredentialSourceServiceID: service, CredentialSourceAuthName: "oauth"}
	selection := models.SDKSelection{ServiceID: service, AuthName: "oauth", ManagedAuth: true, ManagedApplicationID: application, CredentialSourceServiceID: service, CredentialSourceAuthName: "oauth"}
	require.True(t, connectionMatchesManagedSelection(connection, []models.SDKSelection{selection}))
	selection.ManagedApplicationID = uuid.NewString()
	require.False(t, connectionMatchesManagedSelection(connection, []models.SDKSelection{selection}))
	selection.ManagedApplicationID = ""
	require.False(t, connectionMatchesManagedSelection(connection, []models.SDKSelection{selection}))
	connection.ManagedApplicationID = ""
	require.True(t, connectionMatchesManagedSelection(connection, []models.SDKSelection{selection}))
	connection.ManagedAuth = false
	require.False(t, connectionMatchesManagedSelection(connection, []models.SDKSelection{selection}))
}
