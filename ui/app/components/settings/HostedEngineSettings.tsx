import { useEffect, useRef, useState } from "react";
import { api } from "~/lib/api";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { useToast } from "~/components/Toast";
import { readUpgradeIntent, upgradeStatusMessage, upgradeCompleted, upgradeInProgress, type HostedEngineState, type EngineUpgradeIntent } from "~/lib/hosted-engine-upgrade";
import { SettingsDisclosureCard } from "./SettingsDisclosureCard";

// useHostedEngineUpdate preserves one update intent across reconnects and reloads without retaining credentials.
function useHostedEngineUpdate(active: boolean) {
  const { access } = useCurrentActorAccess();
  const workspace = access?.workspace_id;
  const storageKey = `fused:engine-upgrade:${workspace}`;
  const toast = useToast();
  const [state, setState] = useState<HostedEngineState | null>(null);
  const [version, setVersion] = useState("");
  const [canManage, setCanManage] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [intent, setIntent] = useState<EngineUpgradeIntent | null>(null);
  const intentRef = useRef<EngineUpgradeIntent | null>(null);
  const submitting = useRef(false);

  // remember scopes retry identity to this workspace and tolerates browsers that disable session storage.
  function remember(next: EngineUpgradeIntent | null) {
    intentRef.current = next;
    setIntent(next);
    try {
      // Completing an operation removes its replay metadata; pending work survives a reload.
      if (next) sessionStorage.setItem(storageKey, JSON.stringify(next));
      else sessionStorage.removeItem(storageKey);
    } catch { /* In-memory retry identity remains available when browser storage is disabled. */ }
  }

  useEffect(() => {
    // Wait for authenticated workspace identity before reading scoped retry state or polling.
    if (!active || !workspace) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    try {
      const saved = readUpgradeIntent(sessionStorage.getItem(storageKey));
      intentRef.current = saved;
      setIntent(saved);
    } catch { /* Storage restrictions do not prevent reading live update status. */ }
    // poll continues through expected restart outages and never replays a mutation automatically.
    async function poll() {
      try {
        const [next, local] = await Promise.all([api.hostedEngine.state(), api.hostedEngine.access()]);
        // A hidden tab or unmounted component must not publish an obsolete response.
        if (cancelled) return;
        setState(next); setVersion(local.running_version); setCanManage(local.can_manage);
        const pending = intentRef.current;
        // Only observed readiness for the requested release can report success.
        if (pending && upgradeCompleted(next, pending)) {
          remember(null); setMessage("Engine updated successfully.");
        } else {
          setMessage(upgradeStatusMessage(next, pending));
        }
      } catch {
        // Network errors during rollout are not evidence of either success or failure.
        if (!cancelled) setMessage("Engine status is temporarily unavailable. Reconnecting…");
      } finally {
        // Serial polling prevents slow requests from piling up during a restart.
        if (!cancelled) timer = setTimeout(poll, 3000);
      }
    }
    void poll();
    // Cleanup ends this polling session while preserving the workspace's accepted update intent.
    return () => { cancelled = true; clearTimeout(timer); };
  }, [active, workspace, storageKey]);

  // update confirms downtime once and reuses the exact same request after an uncertain response or failure.
  async function update() {
    // The server enforces this grant again; the local guard prevents duplicate interactions.
    if (!canSubmitUpgrade(state, canManage, submitting.current)) return;
    submitting.current = true; setBusy(true);
    try {
      let pending = intentRef.current;
      // A new operation needs a fully observed release and explicit user confirmation.
      if (!pending) {
        pending = await confirmUpgrade(state, toast.confirm);
        // Dismissing confirmation must have no server-side effect.
        if (!pending) return;
        remember(pending);
      }
      const next = await api.hostedEngine.upgrade(pending);
      setState(next); setMessage("Update accepted. Waiting for Engine to restart and become ready…");
    } catch (error) {
      // Keep the intent after an uncertain response so Retry cannot allocate another generation.
      setMessage(error instanceof Error ? `${error.message} Refresh status or retry the same update.` : "Could not confirm the update. Reconnecting…");
    } finally { submitting.current = false; setBusy(false); }
  }

  // A separate operator may supersede a saved intent; discarding it is local and never cancels accepted work.
  function dismissIntent() { remember(null); setMessage(""); }

  return { state, version, canManage, busy, message, intent, update, dismissIntent };
}

type UpgradeControls = ReturnType<typeof useHostedEngineUpdate>;

// canSubmitUpgrade prevents duplicate clicks while preserving explicit retries after a failed rollout.
function canSubmitUpgrade(state: HostedEngineState | null, canManage: boolean, busy: boolean): state is HostedEngineState {
  return canManage && Boolean(state) && !busy && !upgradeInProgress(state);
}

// confirmUpgrade captures the exact approved release before any asynchronous restart request.
async function confirmUpgrade(state: HostedEngineState | null, confirm: (message: string) => Promise<boolean>): Promise<EngineUpgradeIntent | null> {
  // An incomplete status cannot become an update intent.
  if (!state?.update_available || !state.target_image || !state.desired_generation) return null;
  const confirmed = await confirm("Update your hosted Engine? The dashboard and API may briefly disconnect while Engine restarts. Your data and credentials will be preserved.");
  // Cancellation does not allocate an operation or persist retry metadata.
  if (!confirmed) return null;
  return { id: crypto.randomUUID(), expected_generation: state.desired_generation, target_image: state.target_image };
}

// HostedEngineSettings displays the serving version separately from the approved deployment image.
export function HostedEngineSettings({ active }: { active: boolean }) {
  const controls = useHostedEngineUpdate(active);
  return <SettingsDisclosureCard id="hosted-engine-settings" title="Engine updates" description="Update your hosted Engine to the release approved by Fused." defaultExpanded>
    <div className="space-y-4">
      <p className="text-sm text-slate-700">Running version: <span className="font-mono">{controls.version || "Checking…"}</span></p>
      <HostedRelease controls={controls} />
      <p role="status" aria-live="polite" className="text-sm text-slate-600">{controls.message}</p>
    </div>
  </SettingsDisclosureCard>;
}

// HostedRelease keeps self-hosted and unconfigured installations outside hosted update controls.
function HostedRelease({ controls }: { controls: UpgradeControls }) {
  const { state } = controls;
  // Hosting capability comes from Registry placement, never from the browser's hostname.
  if (!state) return <p className="text-sm text-slate-500">Checking hosted Engine status…</p>;
  if (!state.hosted) return <p className="text-sm text-slate-500">{state.available ? "This Engine is self-hosted. Update it through your deployment tooling." : "Hosted Engine updates are not configured."}</p>;
  return <>
    <p className="break-all text-sm text-slate-600">Approved release: <span className="font-mono">{state.target_image}</span></p>
    <p className="text-sm text-slate-600">Status: {state.status}{state.status === "ready" && !state.update_available ? " · Up to date" : ""}</p>
    <p className="text-sm text-slate-500">Updating briefly restarts Engine. Your data and credentials are preserved.</p>
    <UpgradeActions controls={controls} state={state} />
  </>;
}

// UpgradeActions exposes explicit mutation and retry controls only to an authorized account manager.
function UpgradeActions({ controls, state }: { controls: UpgradeControls; state: HostedEngineState }) {
  const { canManage, busy, intent, update, dismissIntent } = controls;
  // Read-only actors can inspect release status but cannot trigger a restart.
  if (!canManage) return <p className="text-sm text-slate-500">Ask your account owner to update Engine.</p>;
  const updating = busy || upgradeInProgress(state);
  const disabled = updating || (!intent && !state.update_available);
  // Saved intent means Retry preserves the original generation and request key.
  const idleLabel = intent ? "Retry update" : "Update Engine";
  return <div className="flex flex-wrap gap-3">
    <button type="button" data-track="upgrade_hosted_engine" onClick={update} disabled={disabled} className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-50">{updating ? "Updating…" : idleLabel}</button>
    {/* Clearing local retry metadata never cancels already accepted work. */}
    {intent && !updating && <button type="button" onClick={dismissIntent} className="text-sm text-slate-600 underline">Clear saved retry</button>}
  </div>;
}
