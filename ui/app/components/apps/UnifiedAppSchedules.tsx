import { CalendarClock, CalendarDays, Repeat2, Plus, ArrowRight } from "lucide-react";
import { ScheduleInputEditor } from "./ScheduleInputEditor";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "@remix-run/react";
import { api } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { preserveScheduleTime, localScheduleTime, scheduleInput, type ScheduleInput, type UnifiedAppSchedule } from "~/lib/unified-app-schedule";

/** Shows actionable errors without exposing JavaScript class names in the form. */
function scheduleErrorMessage(cause: unknown): string {
  // Parsing failures need a correction prompt rather than the browser's implementation-specific exception.
  if (cause instanceof SyntaxError) return "Input must be valid JSON.";
  // Non-Error rejection values still produce a visible failure instead of a blank alert.
  return cause instanceof Error ? cause.message : String(cause);
}

const field = "mt-2 min-h-11 w-full rounded-lg border border-slate-200 bg-white px-3 py-2.5 text-sm text-slate-950 outline-none transition focus:border-slate-400 focus:ring-4 focus:ring-slate-100";
const button = "inline-flex min-h-10 items-center justify-center gap-2 rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm font-medium text-slate-950 transition hover:bg-slate-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950 disabled:opacity-50";
// Saving remains the primary action; opening a new draft uses the compact secondary button.
const primary = "inline-flex min-h-11 items-center justify-center gap-2 rounded-lg bg-[var(--brand-violet)] px-5 py-2.5 text-sm font-semibold text-white shadow-sm transition hover:brightness-95 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--brand-violet)] disabled:opacity-50";

/** Manages durable app automation with compact draft actions without persisting inputs or credentials in the browser. */
export function UnifiedAppSchedules({ appID }: { appID: string }) {
  const toast = useToast();
  const [items, setItems] = useState<UnifiedAppSchedule[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [loadError, setLoadError] = useState("");
  const requestGeneration = useRef(0);
  const mutating = useRef(false);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState<ScheduleInput | null>(null);
  const [inputText, setInputText] = useState("{}");
  const [localTime, setLocalTime] = useState("");
  const [recurring, setRecurring] = useState(false);
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;

  // Cancelled views must not receive private schedule data from a late poll.
  useEffect(() => {
    let current = true;
    let pending = false;
    /** Refreshes summaries without replacing the user's unsaved draft or overlapping slow polls. */
    async function poll() {
      // Slow storage should not build an unbounded queue of refresh requests.
      if (pending || mutating.current) return;
      pending = true;
      const generation = ++requestGeneration.current;
      try {
        const result = await api.schedules.list(appID);
        // A departed family page no longer owns this response.
        if (current && generation === requestGeneration.current) { setItems(result.schedules); setLoadError(""); }
      } catch (cause) {
        // Poll failures remain visible rather than fabricating an empty list.
        if (current && generation === requestGeneration.current) setLoadError(scheduleErrorMessage(cause));
      } finally {
        pending = false;
        // Cleanup owns the component's last state transition.
        if (current) setLoading(false);
      }
    }
    void poll();
    const timer = setInterval(poll, 10_000);
    /** Stops polling and discards late responses when navigating between apps. */
    return () => { current = false; requestGeneration.current++; clearInterval(timer); };
  }, [appID]);

  /** Starts an explicit edit while retaining its revision for conflict detection. */
  function edit(item?: UnifiedAppSchedule) {
    // Existing revisions retain their identity; new drafts receive one stable client-generated ID.
    const value: ScheduleInput = item ? scheduleInput(item) : { id: crypto.randomUUID(), name: "", input: {}, cron: "", timezone: zone, run_at: null, enabled: true, revision: 0 };
    setDraft(value);
    setInputText(JSON.stringify(value.input, null, 2));
    setRecurring(Boolean(value.cron));
    // Recurring drafts have no absolute timestamp to project into the local date control.
    setLocalTime(value.run_at ? localScheduleTime(value.run_at) : "");
    setError("");
  }

  /** Reloads authoritative revisions after a mutation, including uncertain responses. */
  async function refresh() {
    const generation = ++requestGeneration.current;
    const result = await api.schedules.list(appID);
    // A newer read or departed view owns the current list.
    if (generation === requestGeneration.current) { setItems(result.schedules); setLoadError(""); }
  }

  /** Invalidates older polls before a mutation so stale lists cannot undo its visible result. */
  function setMutating(value: boolean) {
    mutating.current = value;
    requestGeneration.current++;
    setBusy(value);
  }

  /** Saves the exact input document; Engine validates it against the active app before enabling it. */
  async function save(event: FormEvent) {
    event.preventDefault();
    // Double submission cannot acquire a second client-generated schedule identity.
    if (!draft || busy) return;
    setMutating(true); setError("");
    try {
      const input = JSON.parse(inputText);
      // Preserve seconds from CLI-created dates unless the manager actually changes the displayed time.
      const request = { ...draft, input, cron: recurring ? draft.cron : "", run_at: recurring ? null : preserveScheduleTime(localTime, draft.run_at), timezone: recurring ? draft.timezone : zone };
      await api.schedules.save(appID, request);
      setDraft(null);
      await refresh();
      toast.success("Schedule saved.");
    } catch (cause) { setError(scheduleErrorMessage(cause)); }
    finally { setMutating(false); }
  }

  /** Pauses future runs or resumes recurrence from now without replaying a backlog. */
  async function toggle(item: UnifiedAppSchedule) {
    setMutating(true); setError("");
    try { await api.schedules.save(appID, { ...scheduleInput(item), enabled: !item.enabled }); await refresh(); }
    catch (cause) { setError(scheduleErrorMessage(cause)); }
    finally { setMutating(false); }
  }

  /** Removes only the reviewed schedule revision while retaining its execution records. */
  async function remove(item: UnifiedAppSchedule) {
    const confirmed = await toast.confirm(`Delete schedule “${item.name}”? Its execution history will remain available.`);
    // Cancelling must leave the durable schedule intact.
    if (!confirmed) return;
    setMutating(true); setError("");
    try { await api.schedules.remove(appID, item.id, item.revision); await refresh(); }
    catch (cause) { setError(scheduleErrorMessage(cause)); }
    finally { setMutating(false); }
  }

  return <section className="space-y-6">
    <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <div><h2 className="text-xl font-semibold tracking-tight text-slate-950">Scheduled runs</h2><p className="mt-1 text-sm text-slate-500">Give this app a time to run and the input it needs.</p></div>
      {/* Content width prevents the secondary action from stretching across narrow layouts. */}
      <button type="button" className={`${button} w-fit self-start shrink-0`} disabled={busy || Boolean(draft)} onClick={() => edit()}><Plus className="h-4 w-4" aria-hidden="true" />New schedule</button>
    </div>
    {/* Errors remain visible above the editor while its input and timing sections stay uncluttered. */}
    {(error || loadError) && <p role="alert" className="rounded-lg bg-red-50 p-3 text-sm text-red-800">{error || loadError}</p>}
    {draft && <form onSubmit={save} className="overflow-visible rounded-2xl border border-slate-200 bg-white shadow-sm">
      <div className="border-b border-slate-100 px-5 py-5 sm:px-7"><div className="flex items-center gap-3"><span className="flex h-10 w-10 items-center justify-center rounded-xl bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]"><CalendarClock className="h-5 w-5" aria-hidden="true" /></span><div><h3 className="text-base font-semibold text-slate-950">{draft.revision ? "Edit schedule" : "New schedule"}</h3><p className="mt-0.5 text-sm text-slate-500">Choose when it runs. Set what it receives.</p></div></div>
        <label className="mt-5 block text-sm font-medium text-slate-700">Schedule name<input className={field} required maxLength={128} placeholder="e.g. Weekly customer report" value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></label>
      </div>
      <div className="grid lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        <section aria-label="Schedule timing" className="space-y-5 border-b border-slate-100 p-5 sm:p-7 lg:border-b-0 lg:border-r">
          <div className="flex items-center gap-2.5"><span className="flex h-6 w-6 items-center justify-center rounded-full bg-slate-100 text-xs font-semibold text-slate-500">1</span><h4 className="text-sm font-semibold text-slate-950">When to run</h4></div>
          {/* Violet tint identifies the selected frequency without a heavy solid fill. */}
          <div className="grid grid-cols-2 gap-2" aria-label="Frequency">
            <button type="button" aria-pressed={!recurring} onClick={() => setRecurring(false)} className={`flex items-center justify-center gap-2 rounded-lg border px-3 py-3 text-sm font-medium transition ${!recurring ? "border-[var(--brand-violet)] bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]" : "border-slate-200 bg-white text-slate-500 hover:bg-slate-50"}`}><CalendarDays className="h-4 w-4" aria-hidden="true" />One time</button>
            <button type="button" aria-pressed={recurring} onClick={() => { setRecurring(true); setDraft({ ...draft, cron: draft.cron || "0 9 * * *" }); }} className={`flex items-center justify-center gap-2 rounded-lg border px-3 py-3 text-sm font-medium transition ${recurring ? "border-[var(--brand-violet)] bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]" : "border-slate-200 bg-white text-slate-500 hover:bg-slate-50"}`}><Repeat2 className="h-4 w-4" aria-hidden="true" />Recurring</button>
          </div>
          {/* Named zones keep recurrence explicit; absolute dates use the displayed browser timezone. */}
          {recurring ? <div className="space-y-5"><label className="block text-sm font-medium text-slate-700">Cron expression<input required className={`${field} font-mono`} value={draft.cron} onChange={(event) => setDraft({ ...draft, cron: event.target.value })} /><span className="mt-2 block text-xs font-normal leading-relaxed text-slate-500">Minute · hour · day · month · weekday<br />Example: <code>0 9 * * 1-5</code> runs weekdays at 09:00.</span></label><label className="block text-sm font-medium text-slate-700">Timezone<input required className={field} value={draft.timezone} onChange={(event) => setDraft({ ...draft, timezone: event.target.value })} placeholder="Europe/London" /></label></div> : <label className="block text-sm font-medium text-slate-700">Date and time<input required type="datetime-local" className={field} value={localTime} onChange={(event) => setLocalTime(event.target.value)} /><span className="mt-2 block text-xs font-normal text-slate-500">Your timezone: {zone}</span></label>}
          <label className="flex items-start gap-3 rounded-lg bg-slate-50 p-4"><input type="checkbox" aria-label="Enabled" className="mt-0.5 h-4 w-4 accent-slate-950" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} /><span><span className="block text-sm font-medium text-slate-800">Enable schedule</span><span className="mt-1 block text-xs leading-relaxed text-slate-500">Turn off to save it paused and start it later.</span></span></label>
        </section>
        <div className="min-w-0 p-5 sm:p-7"><ScheduleInputEditor key={draft.id} appID={appID} text={inputText} onChange={setInputText} /></div>
      </div>
      <div className="sticky bottom-0 z-10 flex flex-wrap items-center justify-between gap-3 rounded-b-2xl border-t border-slate-200 bg-white/95 px-5 py-4 backdrop-blur sm:px-7"><p className="text-xs text-slate-500">Runs the active app version.</p><div className="flex gap-2"><button className={button} disabled={busy} type="button" onClick={() => { setDraft(null); setError(""); }}>Cancel</button><button className={primary} disabled={busy} type="submit">{busy ? "Saving…" : "Save schedule"}<ArrowRight className="h-4 w-4" aria-hidden="true" /></button></div></div>
    </form>}
    {/* Empty onboarding yields to the editor as soon as the user starts a schedule. */}
    {loading ? <p role="status" className="text-sm text-slate-500">Loading schedules…</p> : !items.length && !loadError && !draft ? <div className="flex flex-col items-center rounded-2xl border border-dashed border-slate-300 bg-white px-6 py-12 text-center"><span className="mb-4 flex h-12 w-12 items-center justify-center rounded-2xl bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]"><CalendarClock className="h-6 w-6" aria-hidden="true" /></span><h3 className="text-base font-semibold text-slate-900">No scheduled runs yet</h3><p className="mt-2 max-w-sm text-sm leading-relaxed text-slate-500">Run once at a specific time, or repeat on a schedule. Your saved input is ready for every run.</p><button type="button" className={`${button} mt-6 w-fit`} onClick={() => edit()}><Plus className="h-4 w-4" aria-hidden="true" />Create your first schedule</button></div> : null}
    {/* An admitted one-time run has no next occurrence but is still running, not completed. */}
    <div className="space-y-3">{items.map((item) => <article key={item.id} className="rounded-xl border border-slate-200 bg-white p-5">
      <div className="flex flex-wrap justify-between gap-3"><div><h3 className="font-semibold">{item.name}</h3><p className="mt-1 text-sm text-slate-600">{item.cron ? `${item.cron} · ${item.timezone}` : `One time · ${new Date(item.run_at!).toLocaleString()}`}</p><p className="mt-1 text-sm text-slate-600">{item.last_status === "running" ? "Running" : !item.enabled ? "Paused" : item.next_run_at ? `Next run: ${new Date(item.next_run_at).toLocaleString()}` : "Completed"}</p></div><div className="flex flex-wrap gap-2"><button className={button} disabled={busy || item.last_status === "running"} onClick={() => edit(item)}>Edit</button>{item.next_run_at && <button className={button} disabled={busy || item.last_status === "running"} onClick={() => toggle(item)}>{item.enabled ? "Pause" : "Resume"}</button>}<button className={button} disabled={busy || item.last_status === "running"} onClick={() => remove(item)}>Delete</button></div></div>
      {/* Uncertain outcomes require deliberate review rather than an automatic retry of provider effects. */}
      {item.last_status && <p className="mt-3 text-sm text-slate-600">Last run: {item.last_status}{item.last_started_at ? ` · ${new Date(item.last_started_at).toLocaleString()}` : ""}{item.last_app_id && <> · <Link className="underline" to={`/integrations/unified-apps/${item.last_app_id}?tab=requests`}>View requests</Link></>}</p>}
      {item.last_status === "indeterminate" && <p className="mt-2 text-sm text-amber-800">The run's outcome is uncertain. Review provider state before resuming or scheduling another run.</p>}
    </article>)}</div>
    <details className="text-xs text-slate-500"><summary className="cursor-pointer py-2 font-medium">How scheduled runs work</summary><p className="mt-1 max-w-3xl leading-relaxed">Engine must be running. After downtime, missed recurring times produce one catch-up run. Schedules use the active version, continue after their creator signs out, and do not overlap. Failed runs are not automatically retried.</p></details>
  </section>;
}
