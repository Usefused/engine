import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { KeyRound, Loader2, Plus, X } from "lucide-react";
import { createPortal } from "react-dom";
import { useFusedAgent } from "~/components/agent/FusedAgentContext";
import { api } from "~/lib/api";
import { hasResourcePermission } from "~/lib/current-actor-access";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { CopyValue } from "~/components/CopyValue";
import { Select } from "~/components/forms/Select";
import { AppExecutionTokenList } from "./AppExecutionTokenList";

type TokenAppKind = "sdk" | "api" | "mcp" | "unified_app";
const secondary = "inline-flex items-center justify-center gap-2 rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50";
// Match Engine's maximum whole-second Go duration rather than sending values it cannot represent.
const maxExpirySeconds = 9223372036;

/** Converts a positive whole-number duration to Engine seconds without truncation or overflow. */
export function customTokenExpirySeconds(amount: string, unit: string): number | null {
  const count = Number(amount);
  const multiplier = Number(unit);
  // Only rendered units and exact positive integers are accepted; empty input must never mean no expiry.
  if (!amount.trim() || !Number.isSafeInteger(count) || count <= 0 || ![60, 3600, 86400].includes(multiplier)) return null;
  const seconds = count * multiplier;
  // Keep boundary validation identical to the Engine duration parser.
  return Number.isSafeInteger(seconds) && seconds <= maxExpirySeconds ? seconds : null;
}

/** Gates both the launcher and its mounted panel on the exact family and app type. */
export function AppExecutionTokens({ familyID, kind }: { familyID: string; kind: TokenAppKind }) {
  const { access, loading, failed } = useCurrentActorAccess();
  const canManage = hasResourcePermission(access, `app.${kind}.tokens.manage`, "APP", familyID);
  // Permission loss destroys the panel and its one-time secret; another app starts with a closed launcher.
  return canManage ? <TokenPanelLauncher key={`${access?.subject_id}:${kind}:${familyID}`} familyID={familyID} /> : <p className="text-xs text-slate-500">{loading ? "Checking token permissions…" : failed ? "Could not check token permissions. Refresh to try again." : "You need permission to manage execution tokens for this app."}</p>;
}

/** Uses a concise token launcher and loads credential metadata only when its panel opens. */
function TokenPanelLauncher({ familyID }: { familyID: string }) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const id = useId();
  /** Returns keyboard users to the same action after dismissing token management. */
  function close() { setOpen(false); trigger.current?.focus({ preventScroll: true }); }
  return <div className="flex justify-end">
    <button ref={trigger} type="button" onClick={() => setOpen(true)} aria-haspopup="dialog" aria-expanded={open} aria-controls={open ? id : undefined} className={secondary}><KeyRound className="h-4 w-4" aria-hidden="true" />Tokens</button>
    {/* Portaling keeps the fixed panel outside any transformed or clipped overview container. */}
    {open && createPortal(<TokenPanel familyID={familyID} id={id} onClose={close} />, document.body)}
  </div>;
}

/** Matches Fused detail sidebars, including full-width mobile and assistant popout detection. */
function TokenPanel({ familyID, id, onClose }: { familyID: string; id: string; onClose: () => void }) {
  const agent = useFusedAgent();
  const [busy, setBusy] = useState(false);
  const [promptOpen, setPromptOpen] = useState(false);
  const [headerActions, setHeaderActions] = useState<HTMLDivElement | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // Opening should announce the panel before users tab into generation or token actions.
  useEffect(() => { heading.current?.focus({ preventScroll: true }); }, []);
  /** Prevents an in-flight issuance from losing its one-time credential through accidental dismissal. */
  function close() { if (!busy && !promptOpen) onClose(); }
  return <>
    <div aria-hidden="true" className="fixed inset-0 z-40 bg-slate-900/20" onClick={close} />
    <aside id={id} data-fused-detail-sidebar role="dialog" aria-modal={!agent?.isOpen && !promptOpen} aria-labelledby={`${id}-title`} className="fixed inset-y-0 right-0 z-50 flex h-dvh w-full max-w-full flex-col border-l border-slate-200 bg-white shadow-2xl sm:max-w-lg" onKeyDown={event => {
      // Escape applies only inside this panel, leaving shared toast prompts and the assistant independent.
      if (event.key === "Escape") { event.stopPropagation(); close(); }
    }}>
      <header className="flex items-center gap-2 border-b border-slate-100 px-5 py-4">
        <h2 ref={heading} tabIndex={-1} id={`${id}-title`} className="min-w-0 flex-1 text-base font-semibold leading-5 text-slate-900 outline-none">Execution tokens</h2>
        {/* The generator owns its action state while the panel supplies one stable header location. */}
        <div ref={setHeaderActions} className="shrink-0" />
        <button type="button" onClick={close} disabled={busy || promptOpen} aria-label="Close execution tokens" className="rounded-lg p-1.5 text-slate-400 hover:bg-slate-50 hover:text-slate-700 disabled:opacity-40"><X className="h-5 w-5" aria-hidden="true" /></button>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-5"><TokenGenerator familyID={familyID} onBusyChange={setBusy} onPromptChange={setPromptOpen} headerActions={headerActions} /></div>
    </aside>
  </>;
}

/** Creates tokens only on submission and retains the returned secret in this mounted form's memory. */
function TokenGenerator({ familyID, onBusyChange, onPromptChange, headerActions }: { familyID: string; onBusyChange: (busy: boolean) => void; onPromptChange: (open: boolean) => void; headerActions: HTMLDivElement | null }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState("2592000");
  const [customDuration, setCustomDuration] = useState("1");
  const [customUnit, setCustomUnit] = useState("3600");
  const customSeconds = customTokenExpirySeconds(customDuration, customUnit);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [issued, setIssued] = useState<{ token: string; name: string; expires_at: string | null } | null>(null);
  const [tokenRevision, setTokenRevision] = useState(0);
  const pending = useRef(false);
  const mounted = useRef(true);
  // Responses for a closed page must not reveal credentials in a subsequently mounted app.
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  // Panel dismissal follows issuance state so a response cannot arrive after its only display was closed.
  useEffect(() => { onBusyChange(busy); }, [busy, onBusyChange]);

  /** Prevents duplicate issuance and displays authoritative Engine failures without automatic retries. */
  async function generate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    // Native validation handles ordinary entry; this guard also covers rapid repeated submissions.
    if (pending.current || !name.trim()) return;
    // Invalid custom input must fail before any credential is issued, even for programmatic submissions.
    if (expiry === "custom" && customSeconds === null) {
      setError("Enter a valid custom duration before generating a token.");
      return;
    }
    pending.current = true; setBusy(true); setError("");
    try {
      // Never is the only choice that omits expiry; custom values are converted to exact seconds above.
      const result = await api.appTokens.generate(familyID, { name: name.trim(), allow: ["*"], ...(expiry === "never" ? {} : { expires_in: expiry === "custom" ? customSeconds! : Number(expiry) }) });
      // Only this still-visible authorized form may receive its one-time value.
      if (mounted.current) { setIssued(result); setOpen(false); setName(""); setTokenRevision(revision => revision + 1); }
    } catch (cause) {
      // Keep the draft editable on failure without guessing whether a retry is safe.
      if (mounted.current) setError(cause instanceof Error ? cause.message : "Could not generate an execution token.");
    } finally {
      pending.current = false;
      // Unmounted forms cannot publish stale loading state.
      if (mounted.current) setBusy(false);
    }
  }

  /** Dismisses the one-time plaintext without revoking the token stored by Engine. */
  function dismissToken() { setIssued(null); }

  /** Discards a displayed one-time credential when that same token has been revoked. */
  function tokenRevoked(name: string) {
    // A different token's revocation must not discard a newly issued credential awaiting copy.
    if (issued?.name === name) setIssued(null);
  }

  return <>
    {/* Keep the action in the header and finish copying the current credential before issuing another. */}
    {headerActions && !open && !issued && createPortal(<button type="button" onClick={() => { setError(""); setOpen(true); }} className={secondary}><Plus className="h-4 w-4" aria-hidden="true" />Generate token</button>, headerActions)}
    {/* A compact two-field form keeps custom expiry within the same control group as presets. */}
    {open && <form onSubmit={generate} className="space-y-3">
      <div className="max-w-lg space-y-3">
        <div>
          <label htmlFor={`${id}-name`} className="mb-1.5 block text-xs font-medium text-slate-600">Token name</label>
          <input id={`${id}-name`} name="tokenName" autoFocus required maxLength={128} disabled={busy} value={name} onChange={event => setName(event.target.value)} placeholder="e.g. backend-production" className="h-9 w-full rounded-lg border border-slate-200 bg-white px-3 text-sm text-slate-900 placeholder:text-slate-400" />
        </div>
        <div>
          <label htmlFor={`${id}-expiry`} className="mb-1.5 block text-xs font-medium text-slate-600">Expires after</label>
          <div className="flex flex-wrap items-center gap-2">
            <Select id={`${id}-expiry`} disabled={busy} value={expiry} onChange={event => setExpiry(event.target.value)} className="h-9 border-slate-200 py-1"><option value="86400">1 day</option><option value="2592000">30 days</option><option value="7776000">90 days</option><option value="31536000">1 year</option><option value="custom">Custom</option><option value="never">Never</option></Select>
            {/* Accessible names replace repeated visible labels; amount and unit read as one duration. */}
            {expiry === "custom" && <div className="flex h-9 items-center rounded-lg border border-slate-200 bg-white focus-within:ring-2 focus-within:ring-violet-200">
              <input id={`${id}-duration`} aria-label="Duration" type="number" inputMode="numeric" required min={1} step={1} max={Math.floor(maxExpirySeconds / Number(customUnit))} disabled={busy} value={customDuration} onChange={event => setCustomDuration(event.target.value)} className="h-full w-20 min-w-0 rounded-l-lg border-0 bg-transparent pl-3 pr-1 text-sm text-slate-900 outline-none" />
              <Select id={`${id}-unit`} aria-label="Duration unit" disabled={busy} value={customUnit} onChange={event => setCustomUnit(event.target.value)} className="h-full rounded-l-none border-0 border-l border-slate-200 py-1"><option value="60">Minutes</option><option value="3600">Hours</option><option value="86400">Days</option></Select>
            </div>}
          </div>
          {/* Explain invalid input without adding permanent help text to an otherwise simple form. */}
          {expiry === "custom" && customSeconds === null && <p role="status" className="mt-1.5 text-xs text-red-700">Enter a whole number from 1 to {Math.floor(maxExpirySeconds / Number(customUnit)).toLocaleString()}.</p>}
        </div>
        <p className="text-xs text-slate-500">All app operations, including future versions.</p>
        {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
        <div className="flex items-center justify-end gap-2 pt-1"><button type="button" disabled={busy} onClick={() => setOpen(false)} className="h-9 rounded-lg px-3 text-sm text-slate-500 hover:bg-slate-50 hover:text-slate-700 disabled:opacity-50">Cancel</button><button type="submit" disabled={busy || !name.trim() || (expiry === "custom" && customSeconds === null)} className="inline-flex h-9 items-center gap-2 rounded-lg bg-slate-950 px-3 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-50">{busy && <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />}{busy ? "Generating…" : "Generate token"}</button></div>
      </div>
    </form>}
    {/* One-time credentials and their controls are excluded from assistant page snapshots. */}
    {issued && <div data-fused-visible="false" className="mt-4 border-t border-slate-100 pt-4">
      <p role="status" className="text-sm font-medium text-slate-900">Token “{issued.name}” created</p>
      <p className="mt-1 text-xs text-slate-500">Copy now. This token is shown only once.</p>
      <CopyValue value={issued.token} label="execution token" sensitive className="mt-3" />
      <div className="mt-3 flex justify-end"><button type="button" onClick={dismissToken} className={secondary}>Done</button></div>
    </div>}
    <AppExecutionTokenList familyID={familyID} revision={tokenRevision} onRevoked={tokenRevoked} onPromptChange={onPromptChange} separated={open || !!issued} />
  </>;
}
