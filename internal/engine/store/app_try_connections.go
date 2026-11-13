package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// AppTryConnectionRequest identifies one exact grant without exposing credential material.
type AppTryConnectionRequest struct {
	BucketID   uuid.UUID `json:"bucket_id"`
	ServiceID  uuid.UUID `json:"service_id"`
	AuthName   string    `json:"auth_name"`
	AuthType   string    `json:"auth_type"`
	EndUserRef string    `json:"end_user_ref"`
}

// AppTryConnectionStore projects connection health in one bounded query, never loading tokens.
type AppTryConnectionStore interface {
	ReadAppTryConnections(context.Context, []AppTryConnectionRequest) ([]string, error)
}

// ReadAppTryConnections returns statuses in request order and preserves missing grants as explicit absence.
func (s *postgresStore) ReadAppTryConnections(ctx context.Context, requests []AppTryConnectionRequest) ([]string, error) {
	// No connection requirements must not cause a database scan.
	if len(requests) == 0 {
		return []string{}, nil
	}
	payload, err := json.Marshal(requests)
	// Serialization failure cannot be interpreted as healthy credentials.
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, appTryConnectionsSQL, payload)
	// Store outages remain unknown rather than reporting missing connections.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0, len(requests))
	for rows.Next() {
		var state string
		// Incomplete rows cannot provide a trustworthy readiness result.
		if err := rows.Scan(&state); err != nil {
			return nil, err
		}
		result = append(result, state)
	}
	return result, rows.Err()
}

// ReadAppTryConnections bypasses caches because grant health can change while the sidebar is open.
func (s *cachedStore) ReadAppTryConnections(ctx context.Context, requests []AppTryConnectionRequest) ([]string, error) {
	repository, ok := s.Store.(AppTryConnectionStore)
	// An older backing store cannot fabricate connection readiness.
	if !ok {
		return nil, errors.New("connection readiness unavailable")
	}
	return repository.ReadAppTryConnections(ctx, requests)
}

// Only metadata predicates cross this boundary; expired refreshable tokens remain eligible for normal Engine refresh.
const appTryConnectionsSQL = `
 SELECT CASE
 WHEN request.value->>'end_user_ref'='' THEN 'user_required'
 WHEN connection.id IS NULL THEN 'missing'
 WHEN connection.refresh_state='reconnect_required' THEN 'reconnect_required'
 WHEN connection.expires_at<=NOW() AND
   (COALESCE(connection.refresh_token,'')='' OR connection.refresh_token_expires_at<=NOW()) THEN 'reconnect_required'
 ELSE 'ready' END
 FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY request(value,position)
 LEFT JOIN fused_auth_connections connection ON connection.bucket_id=(request.value->>'bucket_id')::uuid
 AND connection.service_id=(request.value->>'service_id')::uuid
 AND connection.auth_name=request.value->>'auth_name'
 AND connection.auth_type=request.value->>'auth_type'
 AND connection.end_user_ref=request.value->>'end_user_ref'
 ORDER BY request.position`
