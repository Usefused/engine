import { useEffect, useState } from 'react';
import { Loader2, MessageSquare, Search, Trash2 } from 'lucide-react';
import { harnest, sessionTitle, type HarnestSession } from '~/lib/fused-agent-transport';

/** Adapts Threadify's conversation chooser to the narrow docked and popup chat layouts. */
export function FusedAgentHistory({ currentId, disabled, onSelect, onDelete }: {
  currentId: string | null; disabled: boolean; onSelect: (id: string) => Promise<void>; onDelete: (id: string) => Promise<void>;
}) {
  const [sessions, setSessions] = useState<HarnestSession[]>([]);
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [deleting, setDeleting] = useState('');
  const [pendingDelete, setPendingDelete] = useState('');
  // Harnest filters this list by the verified actor; unmounted choosers cannot publish stale history.
  useEffect(() => {
    const controller = new AbortController();
    harnest.listSessions(controller.signal).then(setSessions).catch((cause) => {
      // Cancellation belongs to navigation rather than a connection error.
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : 'Could not load conversations.');
    }).finally(() => { /* Ignore completion after closing the chooser. */ if (!controller.signal.aborted) setLoading(false); });
    return () => { controller.abort(); };
  }, []);
  /** Deletes only an explicitly confirmed conversation and keeps failures available for retry. */
  async function remove(id: string) {
    setDeleting(id); setError('');
    try { await onDelete(id); setSessions(previous => previous.filter(session => session.id !== id)); setPendingDelete(''); }
    catch (cause) { setError(cause instanceof Error ? cause.message : 'Could not delete this conversation.'); }
    finally { setDeleting(''); }
  }
  const filtered = sessions.filter(session => sessionTitle(session).toLowerCase().includes(search.toLowerCase()));
  return <section aria-label="Conversation history" className="min-h-0 flex-1 overflow-y-auto p-4">
    <h3 className="mb-3 text-sm font-semibold">Conversations</h3>
    <label className="mb-4 flex items-center gap-2 rounded-lg border border-slate-200 bg-white px-3 py-2"><Search className="h-4 w-4 text-slate-400" /><input aria-label="Search conversations" value={search} onChange={event => setSearch(event.target.value)} className="min-w-0 flex-1 bg-transparent text-sm outline-none" placeholder="Search conversations" /></label>
    {error && <p role="alert" className="mb-3 text-sm text-red-700">{error}</p>}
    {/* Loading, empty results and populated history are distinct states rather than a misleading blank list. */}
    {loading ? <p role="status" className="flex items-center gap-2 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading conversations…</p> : filtered.length ? <ul className="space-y-2">{filtered.map(session => <li key={session.id} className={`rounded-xl border p-2 ${session.id === currentId ? 'border-violet-200 bg-violet-50' : 'border-slate-200 bg-white'}`}>
      <div className="flex items-center gap-1"><button type="button" disabled={disabled || Boolean(deleting)} onClick={() => void onSelect(session.id)} aria-current={session.id === currentId ? 'true' : undefined} className="flex min-w-0 flex-1 items-center gap-2 rounded-lg p-2 text-left text-sm disabled:opacity-50"><MessageSquare className="h-4 w-4 shrink-0 text-slate-400" /><span className="truncate">{sessionTitle(session)}</span></button><button type="button" aria-label={`Delete conversation ${sessionTitle(session)}`} disabled={disabled || Boolean(deleting)} onClick={() => setPendingDelete(session.id)} className="rounded-lg p-2 text-slate-400 hover:bg-slate-50 hover:text-slate-950"><Trash2 className="h-4 w-4" /></button></div>
      {/* A second explicit action prevents a history click from deleting a conversation accidentally. */}
      {pendingDelete === session.id && <div className="space-y-2 border-t border-slate-200 px-2 pt-2 text-xs"><p>Delete this conversation permanently?</p><div className="flex gap-4"><button type="button" disabled={Boolean(deleting)} onClick={() => void remove(session.id)} className="font-semibold text-slate-950">{deleting === session.id ? 'Deleting…' : 'Delete conversation'}</button><button type="button" onClick={() => setPendingDelete('')}>Cancel</button></div></div>}
    </li>)}</ul> : <p className="text-sm text-slate-500">{search ? 'No matching conversations.' : 'No conversations yet.'}</p>}
  </section>;
}
