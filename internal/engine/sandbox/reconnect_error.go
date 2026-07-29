package sandbox

import (
	"fmt"

	"github.com/Usefused/engine/internal/engine/store"
)

const reconnectRequiredCode = "reconnect_required"

// ReconnectRequiredError is the safe execution contract generated SDKs use to
// hand consent back to the customer application without receiving credentials.
type ReconnectRequiredError struct {
	Code         string `json:"code"`
	BucketID     string `json:"bucket_id"`
	ServiceID    string `json:"service_id"`
	EndUserRef   string `json:"end_user_ref"`
	ConnectionID string `json:"connection_id"`
	Reason       string `json:"reason"`
}

// Error keeps logs readable while the JSON encoder carries the structured
// fields needed to start a replacement Engine connect session.
func (e *ReconnectRequiredError) Error() string {
	return fmt.Sprintf("%s: service %s connection %s", e.Code, e.ServiceID, e.ConnectionID)
}

// newReconnectRequiredError derives routing identifiers from Engine-owned
// state so callers cannot forge a reconnect target through provider errors.
func newReconnectRequiredError(conn *store.AuthConnection, reason string) *ReconnectRequiredError {
	return &ReconnectRequiredError{
		Code:         reconnectRequiredCode,
		BucketID:     conn.BucketID.String(),
		ServiceID:    conn.ServiceID.String(),
		EndUserRef:   conn.EndUserRef,
		ConnectionID: conn.ID.String(),
		Reason:       reason,
	}
}
