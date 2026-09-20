import { useCallback, useEffect, useState } from "react";
import { ShieldCheck } from "lucide-react";

import { useToast } from "~/components/Toast";
import { api, type ManagedAuthStatusValue } from "~/lib/api";

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

const STATUS_LABEL: Record<ManagedAuthStatusValue, string> = {
  disabled: "Disabled",
  ready: "Ready",
  enrollment_required: "Enrollment required",
  temporarily_unavailable: "Temporarily unavailable",
};

const STATUS_BADGE_CLASS: Record<ManagedAuthStatusValue, string> = {
  disabled: "bg-slate-50 text-slate-600 border-slate-200",
  ready: "bg-emerald-50 text-emerald-700 border-emerald-200",
  enrollment_required: "bg-slate-50 text-slate-600 border-slate-200",
  temporarily_unavailable: "bg-amber-50 text-amber-700 border-amber-200",
};

// ManagedAuthCard is the one explicit opt-in surface for enrolling this
// installation with Fused's managed-auth broker
// (internal/engine/managedauthclient). Enrolling only makes Fused Managed
// App available as a choice when a service's auth.ref explicitly selects
// it (${fused.bucket.auth.<service>.<authName>}) -- it never changes how
// an existing, workspace-owned OAuth connection resolves.
export function ManagedAuthCard() {
  const toast = useToast();
  const [status, setStatus] = useState<ManagedAuthStatusValue | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [enabling, setEnabling] = useState(false);
  const [disabling, setDisabling] = useState(false);
  const [revocationPending, setRevocationPending] = useState(false);

  // refresh reads both saved intent and any remote cleanup still pending.
  const refresh = useCallback(async () => {
    const response = await api.managedAuth.get();
    setStatus(response.status);
      setRevocationPending(response.revocation_pending);
  }, []);

  useEffect(() => {
    let active = true;
    setLoading(true);
    refresh()
      .catch((error: unknown) => {
        if (active) setLoadError(errorMessage(error, "Failed to load managed-auth status."));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [refresh]);

  // handleEnable saves explicit opt-in before the Engine attempts enrollment.
  async function handleEnable() {
    setEnabling(true);
    try {
      const response = await api.managedAuth.enable();
      setStatus(response.status);
      setRevocationPending(response.revocation_pending);
      setLoadError("");
      toast.success("Fused Managed Auth is enrolled.");
    } catch (error: unknown) {
      toast.error(errorMessage(error, "Enrollment is unavailable; retry shortly."));
    } finally {
      setEnabling(false);
    }
  }

  // handleDisable acknowledges local opt-out while showing any remote revocation still awaiting retry.
  async function handleDisable() {
    setDisabling(true);
    try {
      const response = await api.managedAuth.disable();
      setStatus(response.status);
      setRevocationPending(response.revocation_pending);
      toast.success("Fused Managed Auth is disabled.");
    } catch (error: unknown) {
      toast.error(errorMessage(error, "Could not save the managed-auth preference."));
    } finally {
      setDisabling(false);
    }
  }

  return (
    <section className="bg-white border border-slate-200 rounded-xl shadow-sm p-6">
      <h2 className="flex items-center gap-2 font-semibold text-slate-900">
        <ShieldCheck className="h-4 w-4" /> Fused Managed Auth
      </h2>
      <p className="mt-1 text-sm text-slate-500">
        Use Fused's registered OAuth applications to connect services. Turning this off stops new
        managed connections and token refreshes. Your saved connections remain available if you
        enable it again.
      </p>
      {loading && <p className="mt-4 text-sm text-slate-500">Loading…</p>}
      {!loading && loadError && (
        <p className="mt-4 text-sm text-rose-600" role="alert">
          {loadError}
        </p>
      )}
      {/* Controls appear only after a trustworthy status read. */}
      {!loading && !loadError && status && (
        <ManagedAuthControls status={status} enabling={enabling} disabling={disabling}
          revocationPending={revocationPending} onEnable={handleEnable} onDisable={handleDisable} />
      )}
    </section>
  );
}

// ManagedAuthControls keeps enrollment actions and pending revocation feedback together.
function ManagedAuthControls({ status, enabling, disabling, revocationPending, onEnable, onDisable }: {
  status: ManagedAuthStatusValue; enabling: boolean; disabling: boolean; revocationPending: boolean;
  onEnable: () => Promise<void>; onDisable: () => Promise<void>;
}) {
  const busy = enabling || disabling;
  return <>
    <div className="mt-4 flex items-center gap-3">
      <span className={`inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-medium ${STATUS_BADGE_CLASS[status]}`}>
        {STATUS_LABEL[status]}
      </span>
      {/* Explicit enable repairs unavailable enrollment or reverses a saved opt-out. */}
      {status !== "ready" && <button type="button" onClick={onEnable} disabled={busy}
        className="rounded bg-slate-900 px-3 py-1.5 text-xs font-semibold text-white hover:bg-slate-700 disabled:opacity-50">
        {enabling ? "Enabling…" : "Enable Fused Managed Auth"}
      </button>}
      {/* Disable stays available during enrollment and broker outages. */}
      {status !== "disabled" && <button type="button" onClick={onDisable} disabled={busy}
        className="rounded border border-slate-300 px-3 py-1.5 text-xs font-semibold text-slate-700 disabled:opacity-50">
        {disabling ? "Disabling…" : "Disable"}
      </button>}
    </div>
    {/* Pending cleanup does not reverse the Engine's saved opt-out. */}
    {status === "disabled" && revocationPending && <p className="mt-3 text-sm text-slate-500">
      Disabled on this Engine. Broker revocation is pending and will retry automatically.
    </p>}
  </>;
}
