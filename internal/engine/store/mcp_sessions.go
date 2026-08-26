package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MCPSessionPage is one keyset-paginated, exact-version history view.
type MCPSessionPage struct {
	Items      []models.MCPSession
	NextCursor string
	HasMore    bool
}

// MCPSessionReader keeps live history out of the configuration cache.
type MCPSessionReader interface {
	ListMCPSessions(context.Context, uuid.UUID, uuid.UUID, string, int) (MCPSessionPage, error)
}

type mcpSessionCursor struct {
	AppID     uuid.UUID `json:"app"`
	StartedAt time.Time `json:"at"`
	ID        uuid.UUID `json:"id"`
}

// validateMCPSessionPage bounds every query and rejects cursors from a different exact app.
func validateMCPSessionPage(appID uuid.UUID, after string, first int) (*mcpSessionCursor, int, error) {
	// Omitted limits use a compact history page, not the entire session collection.
	if first == 0 {
		first = 25
	}
	// The public page bound prevents an audit reader from forcing an unbounded allocation.
	if first < 1 || first > 100 || len(after) > 512 {
		return nil, 0, errors.New("invalid MCP session pagination")
	}
	// The first page has no keyset lower boundary.
	if after == "" {
		return nil, first, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(after)
	// Opaque cursors are transport encoding, never authorization evidence.
	if err != nil {
		return nil, 0, errors.New("invalid MCP session cursor")
	}
	var cursor mcpSessionCursor
	// A cursor must carry the complete stable ordering key for this exact app.
	if json.Unmarshal(raw, &cursor) != nil || !validMCPSessionCursor(cursor, appID) {
		return nil, 0, errors.New("invalid MCP session cursor")
	}
	return &cursor, first, nil
}

// validMCPSessionCursor requires the exact app plus a complete deterministic ordering key.
func validMCPSessionCursor(cursor mcpSessionCursor, appID uuid.UUID) bool {
	return cursor.AppID == appID && cursor.ID != uuid.Nil && !cursor.StartedAt.IsZero()
}

// mcpSessionPageQuery keeps ownership and cursor filtering in one indexed SQL statement.
func mcpSessionPageQuery(accountID, appID uuid.UUID, cursor *mcpSessionCursor, limit int) (string, []any) {
	query := `SELECT session.id, session.app_id, COALESCE(session.app_token_id, '00000000-0000-0000-0000-000000000000'::uuid),
	 session.session_id, session.protocol_version, session.started_at, session.last_activity_at, session.ended_at,
	 COALESCE(session.end_reason, ''), session.client_name, session.client_version, COALESCE(host(session.initial_client_ip), '')
	 FROM fused_mcp_sessions session
	 JOIN fused_apps app ON app.app_id = session.app_id
	 WHERE app.account_id = $1 AND session.app_id = $2`
	args := []any{accountID, appID, limit + 1}
	// Both timestamp and ID are required to avoid skipping tied session starts.
	if cursor != nil {
		query += ` AND (session.started_at, session.id) < ($4::timestamptz, $5::uuid)`
		args = append(args, cursor.StartedAt, cursor.ID)
	}
	return query + ` ORDER BY session.started_at DESC, session.id DESC LIMIT $3`, args
}

// ListMCPSessions reads one bounded page without per-session lookups or in-memory filtering.
func (s *postgresStore) ListMCPSessions(ctx context.Context, accountID, appID uuid.UUID, after string, first int) (MCPSessionPage, error) {
	cursor, limit, err := validateMCPSessionPage(appID, after, first)
	// Invalid pagination must not issue a database query.
	if err != nil {
		return MCPSessionPage{}, err
	}
	query, args := mcpSessionPageQuery(accountID, appID, cursor, limit)
	rows, err := s.db.Query(ctx, query, args...)
	// Database errors remain behind the API's fixed public error boundary.
	if err != nil {
		return MCPSessionPage{}, err
	}
	defer rows.Close()
	items, err := collectMCPSessionRows(rows)
	// Partial history is not a valid page and must never produce a continuation cursor.
	if err != nil {
		return MCPSessionPage{}, err
	}
	return completeMCPSessionPage(appID, items, limit), nil
}

// collectMCPSessionRows projects only the explicitly selected audit fields.
func collectMCPSessionRows(rows pgx.Rows) ([]models.MCPSession, error) {
	items := make([]models.MCPSession, 0)
	for rows.Next() {
		var session models.MCPSession
		// A decoding failure invalidates the whole bounded page instead of silently dropping a row.
		if err := rows.Scan(&session.ID, &session.AppID, &session.AppTokenID, &session.SessionID,
			&session.ProtocolVersion, &session.StartedAt, &session.LastActivityAt, &session.EndedAt, &session.EndReason,
			&session.ClientName, &session.ClientVersion, &session.InitialClientIP); err != nil {
			return nil, err
		}
		items = append(items, session)
	}
	return items, rows.Err()
}

// completeMCPSessionPage removes only the SQL pagination sentinel, not application-filtered rows.
func completeMCPSessionPage(appID uuid.UUID, items []models.MCPSession, limit int) MCPSessionPage {
	page := MCPSessionPage{Items: items, HasMore: len(items) > limit}
	// A cursor exists only when SQL returned evidence of another page.
	if page.HasMore {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		raw, _ := json.Marshal(mcpSessionCursor{AppID: appID, StartedAt: last.StartedAt, ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page
}

// ListMCPSessions bypasses the configuration cache so a lifecycle update is visible on the next page read.
func (s *cachedStore) ListMCPSessions(ctx context.Context, accountID, appID uuid.UUID, after string, first int) (MCPSessionPage, error) {
	reader, ok := s.Store.(MCPSessionReader)
	// A delegate without durable history cannot substitute process-local sessions.
	if !ok {
		return MCPSessionPage{}, errors.New("MCP session history is unavailable")
	}
	return reader.ListMCPSessions(ctx, accountID, appID, after, first)
}
