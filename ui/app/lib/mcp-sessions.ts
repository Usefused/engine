/** Credential-free metadata returned only by the permission-gated session history query. */
export interface McpSession {
  id: string;
  app_token_id?: string;
  session_id: string;
  protocol_version: string;
  started_at: string;
  last_activity_at: string;
  ended_at?: string;
  end_reason?: string;
  client_name?: string;
  client_version?: string;
  initial_client_ip?: string;
}

export interface McpSessionPage {
  items: McpSession[];
  next_cursor: string;
  has_more: boolean;
}

export const MCP_SESSION_PAGE_SIZE = 25;

// The Engine owns filtering and cursor ordering; the browser requests one bounded page.
export const MCP_SESSIONS_QUERY = `
  query McpSessions($appId: String!, $after: String, $first: Int!) {
    mcpSessions(app_id: $appId, after: $after, first: $first) {
      items {
        id app_token_id session_id protocol_version
        started_at last_activity_at ended_at end_reason
        client_name client_version initial_client_ip
      }
      next_cursor
      has_more
    }
  }
`;

/** Historical sessions remain explicitly incomplete rather than acquiring guessed metadata. */
export function sessionMetadata(value?: string): string {
  return value || "Not recorded";
}

/** Missing timestamps must not imply that a historical session was never active. */
export function sessionTime(value?: string): string {
  return value ? new Date(value).toLocaleString() : "Not recorded";
}
