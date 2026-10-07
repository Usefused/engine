import { useEffect, useRef, useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { api } from "~/lib/api";
import { Select } from "~/components/forms/Select";
import { useToast } from "~/components/Toast";

type TokenMetadata = {
  id: string;
  name: string;
  status: string;
  allow: string[];
  created_at: string;
  expires_at: string | null;
  last_used_at: string | null;
};

/** Formats optional metadata without mistaking absent usage for a failed date parse. */
function tokenDate(value: string | null, fallback: string) {
  // Missing expiry means unlimited lifetime; missing activity means the token has never been used.
  if (!value) return fallback;
  const date = new Date(value);
  // Preserve readable metadata even when an older Engine omits a valid timestamp.
  return Number.isNaN(date.getTime()) ? fallback : date.toLocaleString();
}

/** Reads credential-free history only inside the parent permission gate and confirms each revocation. */
export function AppExecutionTokenList({ familyID, revision, onRevoked, onPromptChange, separated = true }: {
  familyID: string;
  revision: number;
  onRevoked: (name: string) => void;
  onPromptChange?: (open: boolean) => void;
  separated?: boolean;
}) {
  const toast = useToast();
  const [tokens, setTokens] = useState<TokenMetadata[]>([]);
  const [filter, setFilter] = useState("active");
  const [refresh, setRefresh] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const [revoking, setRevoking] = useState<string | null>(null);
  const pending = useRef(false);
  const mounted = useRef(true);
  // Shared toast prompts live outside the drawer, so its parent must keep them accessible during review.
  useEffect(() => { onPromptChange?.(reviewing); }, [reviewing, onPromptChange]);

  // Permission loss or family navigation unmounts this component, discarding metadata and pending feedback.
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  // Each refresh cancels older reads so issuance or revocation cannot be overwritten by a stale response.
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError("");
    api.mcpGraphql<{ appTokens: TokenMetadata[] }>(`query AppExecutionTokens($familyID: String!) {
      appTokens(app_family_id: $familyID) { id name status allow created_at expires_at last_used_at }
    }`, { familyID }, { signal: controller.signal }).then(data => {
      // Aborted reads may still resolve in a transport cache; never publish them.
      if (!controller.signal.aborted) setTokens(data.appTokens ?? []);
    }).catch(cause => {
      // A failed read is not an empty token set; keep any prior rows and expose a retry.
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load execution tokens.");
    }).finally(() => {
      // Only the current read owns the loading indicator.
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [familyID, revision, refresh]);

  /** Uses the shared toast confirmation before sending one exact-family revocation. */
  async function requestRevocation(token: TokenMetadata) {
    // Lock before opening the prompt so repeated clicks cannot queue duplicate confirmations.
    if (pending.current) return;
    pending.current = true; setReviewing(true);
    try {
      const confirmed = await toast.confirm(`Revoke “${token.name}”? This removes its access to every version of this app and cannot be undone.`, { confirmLabel: "Revoke token", cancelLabel: "Cancel" });
      // Cancellation and navigation while the prompt is open must never cause a later mutation.
      if (!confirmed || !mounted.current) return;
      setRevoking(token.id);
      await api.appTokens.revoke(familyID, token.name);
      // An obsolete page must not publish a success into another app or identity.
      if (!mounted.current) return;
      // Mark only the reviewed token; a later refresh supplies authoritative retained history.
      setTokens(current => current.map(item => item.id === token.id ? { ...item, status: "revoked" } : item));
      toast.success(`Token “${token.name}” revoked.`);
      onRevoked(token.name); setRefresh(current => current + 1);
    } catch (cause) {
      // Use the shared error surface and leave the token available for an explicit retry.
      if (mounted.current) toast.error(cause instanceof Error ? cause.message : "Could not revoke this token.");
    } finally {
      pending.current = false;
      // The old page no longer owns React state after navigation or permission loss.
      if (mounted.current) { setRevoking(null); setReviewing(false); }
    }
  }

  /** Reloads metadata explicitly without altering credentials. */
  function refreshTokens() { setRefresh(current => current + 1); }

  // Engine supplies lifecycle status, including expiry; the filter never infers activity from token usage.
  const visible = tokens.filter(token => filter === "all" || token.status === "active");
  // A divider separates an open form or one-time result; a standalone list starts directly below the header.
  return <div className={separated ? "mt-5 border-t border-slate-100 pt-4" : ""}>
    <div className="flex items-center justify-between gap-3">
      <Select aria-label="Filter execution tokens" value={filter} onChange={event => setFilter(event.target.value)} className="max-w-48 text-sm">
        <option value="active">Active tokens</option><option value="all">All tokens</option>
      </Select>
      <button type="button" onClick={refreshTokens} disabled={loading || reviewing} aria-label="Refresh execution tokens" title="Refresh tokens" className="rounded-lg p-2 text-slate-500 hover:bg-slate-50 disabled:opacity-50"><RefreshCw aria-hidden="true" className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} /></button>
    </div>
    {/* Live feedback distinguishes fetch failures and revocations from a genuinely empty list. */}
    {error && <p role="alert" className="mt-3 text-sm text-red-700">{error} Refresh to try again.</p>}
    {loading && <p role="status" className="mt-3 text-sm text-slate-500">Loading tokens…</p>}
    {!loading && !error && visible.length === 0 && <p className="py-5 text-sm text-slate-500">{filter === "active" ? "No active execution tokens." : "No execution tokens yet."}</p>}
    <ul className="mt-3 divide-y divide-slate-100">
      {/* Rows use retained token IDs internally; names and status are the human-facing identity. */}
      {visible.map(token => <li key={token.id} className="py-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex min-w-0 flex-wrap items-center gap-2"><span className="break-all text-sm font-medium text-slate-900">{token.name}</span><span className={`rounded-md px-2 py-0.5 text-xs capitalize ${token.status === "active" ? "bg-emerald-50 text-emerald-700" : "bg-slate-100 text-slate-500"}`}>{token.status}</span></div>
          {/* Historical credentials cannot be revoked again, and only one review can be in flight. */}
          {token.status === "active" && <button type="button" aria-label={`Revoke ${token.name}`} disabled={reviewing} onClick={() => requestRevocation(token)} className="inline-flex items-center gap-1.5 rounded-lg px-2 py-1 text-xs font-medium text-red-700 hover:bg-red-50 disabled:opacity-50">{revoking === token.id && <Loader2 aria-hidden="true" className="h-3 w-3 animate-spin" />}{revoking === token.id ? "Revoking…" : "Revoke"}</button>}
        </div>
        <dl className="mt-2 grid gap-x-6 gap-y-1 text-xs text-slate-500 sm:grid-cols-3">
          <div><dt className="inline">Created </dt><dd className="inline">{tokenDate(token.created_at, "Unknown")}</dd></div>
          <div><dt className="inline">Expires </dt><dd className="inline">{tokenDate(token.expires_at, "Never")}</dd></div>
          <div><dt className="inline">Last used </dt><dd className="inline">{tokenDate(token.last_used_at, "Never")}</dd></div>
        </dl>
        {/* Exact allowed operations remain available for reviewing restricted tokens without exposing credentials. */}
        <p className="mt-2 break-words text-xs text-slate-500">{token.allow.includes("*") ? "All app operations" : `Operations: ${token.allow.join(", ") || "None"}`}</p>
      </li>)}
    </ul>
  </div>;
}
