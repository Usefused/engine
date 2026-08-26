import { useEffect, useState } from "react";
import { api } from "~/lib/api";
import { appActivityIssue } from "~/lib/app-activity-error";
import { MCP_SESSION_PAGE_SIZE, MCP_SESSIONS_QUERY, type McpSessionPage } from "~/lib/mcp-sessions";

/** Keeps only the visible page, retaining opaque cursors for backwards navigation. */
export function useMcpSessionsPage(appId: string) {
  const [cursors, setCursors] = useState<string[]>([""]);
  const [revision, setRevision] = useState(0);
  const [page, setPage] = useState<McpSessionPage | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const after = cursors[cursors.length - 1];

  // This component mounts only beneath the exact app/audit permission gate; a
  // key on appId ensures a different immutable version cannot inherit cursors.
  useEffect(() => {
    let active = true;
    setPage(null);
    setLoading(true);
    setError("");
    api.mcpGraphql<{ mcpSessions: McpSessionPage }>(MCP_SESSIONS_QUERY, {
      appId, after, first: MCP_SESSION_PAGE_SIZE,
    }).then((result) => {
      // Late responses from a previous page or unmounted permission scope must be ignored.
      if (active) setPage(result.mcpSessions);
    }).catch((cause) => {
      // Reuse the shared inactive-app translation without exposing raw transport or store errors.
      if (active) setError(appActivityIssue(cause, "mcp").message);
    }).finally(() => {
      // An obsolete request must not clear the current page's loading indicator.
      if (active) setLoading(false);
    });
    return () => { active = false; };
  }, [appId, after, revision]);

  /** Advances only with the server's cursor, never an offset inferred from displayed rows. */
  function next() {
    // Defensive guards also cover keyboard events arriving during a page transition.
    if (loading || !page?.has_more || !page.next_cursor) return;
    setCursors((previous) => [...previous, page.next_cursor]);
  }

  /** Replays the previous cursor without retaining all previously fetched session data. */
  function previous() {
    // The first page has no predecessor and pending reads cannot be navigated twice.
    if (loading || cursors.length === 1) return;
    setCursors((previousCursors) => previousCursors.slice(0, -1));
  }

  /** Refresh starts at newest history so newly opened sessions are discoverable. */
  function refresh() {
    setCursors([""]);
    setRevision((previousRevision) => previousRevision + 1);
  }

  /** Retry preserves the failed cursor instead of silently moving to another page. */
  function retry() {
    setRevision((previousRevision) => previousRevision + 1);
  }

  return { page, pageNumber: cursors.length, loading, error, next, previous, refresh, retry };
}
