import { Clock, RefreshCw } from "lucide-react";
import { sessionMetadata, sessionTime, type McpSession } from "~/lib/mcp-sessions";
import { useMcpSessionsPage } from "./useMcpSessionsPage";

/** Mounts bounded session history only inside the exact-version Activity permission boundary. */
export function McpSessionsPanel({ appId }: { appId: string }) {
  const history = useMcpSessionsPage(appId);
  return (
    <section aria-label="MCP session history" aria-busy={history.loading} className="min-w-0 overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
      <header className="flex items-start justify-between gap-3 border-b border-slate-100 px-5 py-4">
        <div className="min-w-0">
          <h3 className="flex items-center gap-2 font-semibold text-slate-900"><Clock className="h-4 w-4 shrink-0 text-blue-500" />Sessions</h3>
          <p className="mt-1 text-xs text-slate-500">Newest first. Client identity is self-reported, not verified.</p>
        </div>
        <button type="button" onClick={history.refresh} disabled={history.loading} aria-label="Refresh session history" className="rounded-lg border border-slate-200 p-2 text-slate-500 hover:bg-slate-50 disabled:opacity-50"><RefreshCw className="h-4 w-4" /></button>
      </header>
      <McpSessionPageContent history={history} />
      <McpSessionPagination history={history} />
    </section>
  );
}

type SessionHistory = ReturnType<typeof useMcpSessionsPage>;

/** Separates loading and failures from an honestly empty historical result. */
export function McpSessionPageContent({ history }: { history: SessionHistory }) {
  // Pending pages do not relabel previously loaded rows with a new page number.
  if (history.loading) return <p role="status" className="px-6 py-12 text-center text-sm text-slate-500">Loading sessions...</p>;
  // A failed read is retryable and must never masquerade as an empty history.
  if (history.error) return <div role="alert" className="space-y-3 px-6 py-12 text-center text-sm text-slate-500"><p>{history.error}</p><button type="button" onClick={history.retry} className="rounded-lg border border-slate-200 px-3 py-2 text-slate-700 hover:bg-slate-50">Try again</button></div>;
  // The absence of rows, including an expired cursor window, is an explicit empty state.
  if (!history.page?.items.length) return <p className="px-6 py-12 text-center text-sm text-slate-500">No sessions recorded on this page.</p>;
  return <McpSessionRows sessions={history.page.items} />;
}

/** Cursor navigation stays operable on narrow screens without implying an unavailable total. */
export function McpSessionPagination({ history }: { history: SessionHistory }) {
  const canAdvance = !history.loading && !!history.page?.has_more && !!history.page.next_cursor;
  return (
    <nav aria-label="Session history pages" className="flex flex-wrap items-center justify-between gap-3 border-t border-slate-100 px-5 py-4 text-sm">
      <p aria-live="polite" className="text-slate-500">Page {history.pageNumber}</p>
      <div className="flex gap-2">
        <button type="button" onClick={history.previous} disabled={history.loading || history.pageNumber === 1} className="rounded-lg border border-slate-200 px-3 py-2 text-slate-700 hover:bg-slate-50 disabled:opacity-50">Previous</button>
        <button type="button" onClick={history.next} disabled={!canAdvance} className="rounded-lg border border-slate-200 px-3 py-2 text-slate-700 hover:bg-slate-50 disabled:opacity-50">Next</button>
      </div>
    </nav>
  );
}

/** Keeps session metadata available as cards on mobile and a comparative table on desktop. */
export function McpSessionRows({ sessions }: { sessions: McpSession[] }) {
  return (
    <>
      <div className="divide-y divide-slate-100 md:hidden">{sessions.map((session) => <McpSessionCard key={session.id} session={session} />)}</div>
      <div className="hidden overflow-x-auto md:block">
        <table className="w-full min-w-[1050px] text-left text-sm">
          <caption className="sr-only">MCP session history, newest first</caption>
          <thead className="bg-slate-50/50 text-xs uppercase tracking-wider text-slate-500"><tr><th scope="col" className="px-5 py-3 font-medium">Session</th><th scope="col" className="px-5 py-3 font-medium">Client (self-reported)</th><th scope="col" className="px-5 py-3 font-medium">Started</th><th scope="col" className="px-5 py-3 font-medium">Last activity</th><th scope="col" className="px-5 py-3 font-medium">Ended</th><th scope="col" className="px-5 py-3 font-medium">Status</th><th scope="col" className="px-5 py-3 font-medium">Connection</th></tr></thead>
          <tbody className="divide-y divide-slate-100 text-slate-700">{sessions.map((session) => <McpSessionTableRow key={session.id} session={session} />)}</tbody>
        </table>
      </div>
    </>
  );
}

/** Keeps long identifiers and every connection field within the mobile viewport. */
function McpSessionCard({ session }: { session: McpSession }) {
  return (
    <article className="min-w-0 space-y-4 p-4">
      <div className="flex min-w-0 items-start justify-between gap-3"><span className="min-w-0 break-all font-mono text-sm text-slate-700">{session.session_id}</span><SessionStatus live={!session.ended_at} /></div>
      <div><p className="text-xs text-slate-500">Client (self-reported)</p><SessionClient session={session} /></div>
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <div><dt className="text-xs text-slate-500">Started</dt><dd className="mt-0.5 break-words text-slate-700">{sessionTime(session.started_at)}</dd></div>
        <div><dt className="text-xs text-slate-500">Ended</dt><dd className="mt-0.5 break-words text-slate-700">{sessionTime(session.ended_at)}</dd></div>
        <div><dt className="text-xs text-slate-500">Last activity</dt><dd className="mt-0.5 break-words text-slate-700">{sessionTime(session.last_activity_at)}</dd></div>
      </dl>
      <SessionConnectionDetails session={session} />
    </article>
  );
}

/** Desktop rows reuse the same status, client, and connection projection as mobile cards. */
function McpSessionTableRow({ session }: { session: McpSession }) {
  return <tr className="align-top transition-colors hover:bg-slate-50"><td className="max-w-52 break-all px-5 py-4 font-mono text-xs text-slate-600">{session.session_id}</td><td className="max-w-56 px-5 py-4"><SessionClient session={session} /></td><td className="px-5 py-4">{sessionTime(session.started_at)}</td><td className="px-5 py-4">{sessionTime(session.last_activity_at)}</td><td className="px-5 py-4">{sessionTime(session.ended_at)}</td><td className="px-5 py-4"><SessionStatus live={!session.ended_at} /></td><td className="min-w-64 max-w-72 px-5 py-4"><SessionConnectionDetails session={session} /></td></tr>;
}

/** Client-provided values remain plain escaped text, never a trusted product identity. */
function SessionClient({ session }: { session: McpSession }) {
  return <div className="break-all text-sm text-slate-700"><p>{sessionMetadata(session.client_name)}</p><p className="mt-0.5 text-xs text-slate-500">Version: {sessionMetadata(session.client_version)}</p></div>;
}

/** Initial address stays in audited session details, not public analytics or trace attributes. */
function SessionConnectionDetails({ session }: { session: McpSession }) {
  return (
    <details className="min-w-0 text-xs text-slate-500">
      <summary className="cursor-pointer font-medium text-slate-700">Connection details</summary>
      <dl className="mt-3 space-y-3">
        <div><dt>Initial client IP</dt><dd className="mt-0.5 break-all font-mono text-slate-700">{sessionMetadata(session.initial_client_ip)}</dd></div>
        <div><dt>Protocol</dt><dd className="mt-0.5 break-all font-mono text-slate-700">{sessionMetadata(session.protocol_version)}</dd></div>
        <div><dt>Token ID</dt><dd className="mt-0.5 break-all font-mono text-slate-700">{sessionMetadata(session.app_token_id)}</dd></div>
        <div><dt>End reason</dt><dd className="mt-0.5 break-words text-slate-700">{sessionMetadata(session.end_reason).replaceAll("_", " ")}</dd></div>
      </dl>
      <p className="mt-3">The initial address may belong to a proxy, VPN, or shared network, rather than an individual device.</p>
    </details>
  );
}

/** Preserves the existing live/disconnected treatment without inventing a status from client metadata. */
function SessionStatus({ live }: { live: boolean }) {
  // Only an open lifecycle row receives the live treatment and motion-safe pulse.
  if (live) return <span className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-emerald-100 bg-emerald-50 px-2.5 py-1 text-xs font-medium text-emerald-600"><span className="h-1.5 w-1.5 rounded-full bg-emerald-500 motion-safe:animate-pulse" />Live</span>;
  return <span className="inline-flex shrink-0 items-center rounded-full border border-slate-200 bg-slate-100 px-2.5 py-1 text-xs font-medium text-slate-600">Disconnected</span>;
}
