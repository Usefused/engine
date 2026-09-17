import { useCallback, useEffect, useState } from "react";
import type { MetaFunction } from "@remix-run/react";
import { KeyRound, Ban, RotateCw } from "lucide-react";
import { useToast } from "~/components/Toast";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { SectionTabs } from "~/components/layout/SectionTabs";
import {
  createOAuthRegistrationKey,
  getOAuthRegistrationKeyStatus,
  revokeOAuthRegistrationKey,
} from "~/lib/oauth-clients";

const ACCESS_TABS = [
  { label: "People", to: "/integrations/access/people" },
  { label: "Teams", to: "/integrations/access/teams" },
  { label: "OAuth Clients", to: "/integrations/access/oauth-clients" },
  { label: "Registration Key", to: "/integrations/access/registration-key" },
  { label: "Connected Apps", to: "/integrations/access/connected-apps" },
];

export const meta: MetaFunction = () => [{ title: "Registration Key - Fused" }];

export default function RegistrationKeyPage() {
  const { access } = useCurrentActorAccess();
  return (
    <>
      <SectionTabs tabs={ACCESS_TABS} />
      <WorkspacePermissionGate permission="access.read" area="OAuth registration key">
        <RegistrationKeyManager canManage={hasWorkspacePermission(access, "access.manage")} />
      </WorkspacePermissionGate>
    </>
  );
}

/** Coordinates viewing, minting, rotating, and revoking the caller's per-user registration key. */
function RegistrationKeyManager({ canManage }: { canManage: boolean }) {
  const toast = useToast();
  const [exists, setExists] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [rawKey, setRawKey] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    const status = await getOAuthRegistrationKeyStatus();
    setExists(status.exists);
  }, []);

  useEffect(() => {
    setLoading(true);
    refresh().catch((error: unknown) => toast.error(errorMessage(error))).finally(() => setLoading(false));
  }, [refresh, toast]);

  async function handleCreate() {
    // Rotation revokes any prior key and issues a new one in a single transaction.
    setSaving(true);
    try {
      const key = await createOAuthRegistrationKey();
      setRawKey(key);
      await refresh();
      toast.success("Registration key created.");
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  async function handleRevoke() {
    const confirmed = await toast.confirm(
      "Revoke your registration key? Existing integrators will no longer be able to register OAuth clients."
    );
    if (!confirmed) return;
    setSaving(true);
    try {
      await revokeOAuthRegistrationKey();
      setRawKey(null);
      await refresh();
      toast.success("Registration key revoked.");
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-6">
      <section className="bg-white border border-slate-200 rounded-xl shadow-sm p-6">
        <h2 className="flex items-center gap-2 font-semibold text-slate-900">
          <KeyRound className="h-4 w-4" /> Registration key
        </h2>
        <p className="mt-1 text-sm text-slate-500">
          A registration key lets a local tool (such as Harnest) dynamically register an ephemeral
          OAuth client so you can connect it to your workspace without copying a client id. The key
          can only register clients — it grants no direct access on its own.
        </p>
        {loading && <p className="mt-4 text-sm text-slate-500">Loading…</p>}
        {!loading && (
          <div className="mt-4 flex items-center gap-3">
            <span className="text-sm font-medium text-slate-700">{exists ? "Active" : "Not set"}</span>
            {canManage && (
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={handleCreate}
                  disabled={saving}
                  className="inline-flex items-center gap-1.5 rounded bg-slate-900 px-3 py-1.5 text-xs font-semibold text-white hover:bg-slate-700 disabled:opacity-50"
                >
                  <RotateCw className="h-4 w-4" /> {exists ? "Rotate" : "Create"}
                </button>
                {exists && (
                  <button
                    type="button"
                    onClick={handleRevoke}
                    disabled={saving}
                    className="inline-flex items-center gap-1.5 rounded border border-rose-300 px-3 py-1.5 text-xs font-semibold text-rose-600 hover:bg-rose-50 disabled:opacity-50"
                  >
                    <Ban className="h-4 w-4" /> Revoke
                  </button>
                )}
              </div>
            )}
          </div>
        )}
      </section>
      {rawKey && <RegistrationKeyNotice rawKey={rawKey} onClear={() => setRawKey(null)} />}
    </div>
  );
}

function RegistrationKeyNotice({ rawKey, onClear }: { rawKey: string; onClear: () => void }) {
  return (
    <div className="rounded-lg border border-amber-300 bg-amber-50 p-4" role="alert">
      <p className="text-sm font-semibold text-amber-900">
        Copy this registration key now. It will not be shown again.
      </p>
      <code className="mt-3 block break-all rounded bg-white p-2 text-xs text-slate-900">{rawKey}</code>
      <div className="mt-3 flex gap-2">
        <button
          type="button"
          onClick={() => navigator.clipboard.writeText(rawKey)}
          className="rounded bg-amber-700 px-3 py-1.5 text-xs font-semibold text-white"
        >
          Copy key
        </button>
        <button
          type="button"
          onClick={onClear}
          className="rounded border border-amber-400 px-3 py-1.5 text-xs font-semibold text-amber-900"
        >
          I've saved it
        </button>
      </div>
    </div>
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}
