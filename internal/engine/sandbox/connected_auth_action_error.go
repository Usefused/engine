package sandbox

import (
	"fmt"

	"github.com/Usefused/engine/internal/engine/store"
)

// ConnectionRequiredError identifies the exact Engine-owned connection slot a
// caller must establish, without carrying provider credentials.
type ConnectionRequiredError struct {
	Code       string `json:"code"`
	BucketID   string `json:"bucket_id"`
	ServiceID  string `json:"service_id"`
	EndUserRef string `json:"end_user_ref"`
}

// Error returns a bounded diagnostic that excludes auth names and credentials.
func (e *ConnectionRequiredError) Error() string {
	return fmt.Sprintf("%s: service %s", e.Code, e.ServiceID)
}

// Unwrap preserves existing sentinel checks used by lower-level resolver tests.
func (e *ConnectionRequiredError) Unwrap() error {
	return store.ErrAuthConnectionNotFound
}

// newConnectionRequiredError derives connection routing from trusted execution
// scope after an exact lookup finds no grant.
func newConnectionRequiredError(bucketID, serviceID, endUserRef string) *ConnectionRequiredError {
	return &ConnectionRequiredError{
		Code: "connection_required", BucketID: bucketID,
		ServiceID: serviceID, EndUserRef: endUserRef,
	}
}

// ResourceSelectionRequiredError identifies a connected grant whose provider
// tenant/resource must be selected before dispatch.
type ResourceSelectionRequiredError struct {
	Code         string `json:"code"`
	BucketID     string `json:"bucket_id"`
	ServiceID    string `json:"service_id"`
	EndUserRef   string `json:"end_user_ref"`
	ConnectionID string `json:"connection_id"`
	Reason       string `json:"reason"`
}

// Error returns only the bounded action code and Engine connection identity.
func (e *ResourceSelectionRequiredError) Error() string {
	return fmt.Sprintf("%s: connection %s", e.Code, e.ConnectionID)
}

// newResourceSelectionRequiredError captures trusted routing identifiers and a
// stable reason after connection-scoped resource lookup.
func newResourceSelectionRequiredError(conn *store.AuthConnection, reason string) *ResourceSelectionRequiredError {
	return &ResourceSelectionRequiredError{
		Code: "resource_selection_required", BucketID: conn.BucketID.String(),
		ServiceID: conn.ServiceID.String(), EndUserRef: conn.EndUserRef,
		ConnectionID: conn.ID.String(), Reason: reason,
	}
}
