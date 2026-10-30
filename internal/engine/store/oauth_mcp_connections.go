package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// OAuthMCPConnectionChoice contains display and binding identities only, never provider credentials.
type OAuthMCPConnectionChoice struct {
	ServiceID                          uuid.UUID
	ConnectionID                       uuid.UUID
	ResourceID                         *uuid.UUID
	AuthName, EndUserRef, Label, Value string
}

// OAuthMCPConnections reads a bounded, exact-version account picker in one set-based query after delegation authorization.
func (s *postgresStore) OAuthMCPConnections(ctx context.Context, appID uuid.UUID) ([]OAuthMCPConnectionChoice, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT c.service_id,c.id,r.id,c.auth_name,c.end_user_ref,
	 c.end_user_ref || CASE WHEN COALESCE(r.display_name,'')<>'' THEN ' — ' || r.display_name ELSE '' END,
	 c.id::text || ':' || COALESCE(r.id::text,'')
	 FROM fused_apps a
	 CROSS JOIN LATERAL jsonb_array_elements(a.selections) selection
	 JOIN fused_auth_connections c ON c.service_id=(selection->>'service_id')::uuid
	 LEFT JOIN fused_connection_resources r ON r.connection_id=c.id AND r.is_active
	 WHERE a.app_id=$1 AND c.auth_type IN ('oauth','oidc')
	 AND c.bucket_id=(SELECT bucket_id FROM fused_app_family_buckets b WHERE b.app_family_id=a.app_family_id
	   AND (b.service_id=c.service_id OR b.service_id IS NULL) ORDER BY b.service_id NULLS LAST LIMIT 1)
	 AND (selection->>'auth_name'=c.auth_name OR (COALESCE(selection->>'auth_name','')='' AND EXISTS (
	   SELECT 1 FROM jsonb_array_elements(COALESCE(selection->'required_auth','[]'::jsonb)) auth WHERE auth->>'auth_name'=c.auth_name AND auth->>'auth_type' IN ('oauth','oidc'))))
	 ORDER BY c.service_id,c.id,r.id LIMIT 1001`, appID)
	// Database failure must not look like an account with no connections.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	choices := make([]OAuthMCPConnectionChoice, 0)
	// One bounded result includes all services and active resources; no per-service lookups occur.
	for rows.Next() {
		var choice OAuthMCPConnectionChoice
		// Incomplete identity rows must never become selectable accounts.
		if err := rows.Scan(&choice.ServiceID, &choice.ConnectionID, &choice.ResourceID, &choice.AuthName, &choice.EndUserRef, &choice.Label, &choice.Value); err != nil {
			return nil, err
		}
		choices = append(choices, choice)
	}
	// Truncation cannot silently hide a previously selected account during POST validation.
	if len(choices) > 1000 {
		return nil, errors.New("MCP connection catalogue exceeds its limit")
	}
	return choices, rows.Err()
}

// OAuthMCPConnections preserves exact live connection choices through the shared store wrapper.
func (s *cachedStore) OAuthMCPConnections(ctx context.Context, appID uuid.UUID) ([]OAuthMCPConnectionChoice, error) {
	repository, ok := s.Store.(interface {
		OAuthMCPConnections(context.Context, uuid.UUID) ([]OAuthMCPConnectionChoice, error)
	})
	// Unsupported storage cannot fall back to accepting arbitrary connection references.
	if !ok {
		return nil, errors.New("MCP connection catalogue unavailable")
	}
	return repository.OAuthMCPConnections(ctx, appID)
}
