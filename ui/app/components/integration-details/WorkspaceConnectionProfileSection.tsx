import { useEffect, useMemo, useState } from "react";
import { Code2, Loader2, Pencil, RotateCcw, Save, Shield, X } from "lucide-react";
import { api, type AuthConfig, type WorkspaceConnectionProfile } from "~/lib/api";
import { useToast } from "~/components/Toast";

interface Props {
  serviceId: string;
  serviceVersionId?: string;
  serviceVersion?: string;
  authConfigs: AuthConfig[];
  isOwner: boolean;
}

// WorkspaceConnectionProfileSection is deliberately independent of buckets:
// one override affects every artifact selecting this service/version/auth tuple.
export function WorkspaceConnectionProfileSection({
  serviceId,
  serviceVersionId,
  serviceVersion,
  authConfigs,
  isOwner,
}: Props) {
  const toast = useToast();
  const authType = useMemo(() => profileAuthType(authConfigs), [authConfigs]);
  const [profile, setProfile] = useState<WorkspaceConnectionProfile | null>(null);
  const [source, setSource] = useState("");
  const [editing, setEditing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!serviceVersionId || !authType) return;
    setError("");
    api.workspace.getWorkspaceConnectionProfile({ serviceId, serviceVersionId, authType })
      .then((value) => {
        setProfile(value);
        setSource(JSON.stringify(value?.profile || { auth_type: authType }, null, 2));
      })
      .catch((reason) => setError(reason instanceof Error ? reason.message : "Failed to load connection profile"));
  }, [serviceId, serviceVersionId, authType]);

  if (!serviceVersionId || !serviceVersion || !authType) return null;

  const save = async () => {
    setSaving(true);
    setError("");
    try {
      const updated = await api.workspace.setWorkspaceConnectionProfile({
        serviceId,
        serviceVersionId,
        version: serviceVersion,
        authType,
        profile: JSON.parse(source) as Record<string, unknown>,
      });
      setProfile(updated);
      setSource(JSON.stringify(updated.profile, null, 2));
      setEditing(false);
      toast.success("Workspace connection profile saved");
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Failed to save connection profile");
    } finally {
      setSaving(false);
    }
  };

  const reset = async () => {
    if (!profile?.has_workspace_override) return;
    const confirmed = await toast.confirm(
      "Reset this workspace customization? Every artifact selecting this service version and auth type will use the provider default."
    );
    if (!confirmed) return;
    setSaving(true);
    setError("");
    try {
      const updated = await api.workspace.resetWorkspaceConnectionProfile({ serviceId, serviceVersionId, authType });
      setProfile(updated);
      setSource(JSON.stringify(updated?.profile || { auth_type: authType }, null, 2));
      setEditing(false);
      toast.success("Workspace customization reset");
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Failed to reset connection profile");
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4 sm:p-5">
      <div className="flex items-start gap-2.5">
        <Shield className="mt-0.5 h-4 w-4 text-emerald-600" />
        <div>
          <h2 className="text-sm font-semibold text-slate-900">Connection profile</h2>
          <p className="mt-0.5 text-xs text-slate-500">{profileSource(profile?.provenance)}</p>
        </div>
      </div>
      {error && <p className="mt-3 text-sm text-red-600">{error}</p>}
      <details className="group mt-4 border-t border-slate-200 pt-4">
        <summary className="flex cursor-pointer list-none items-center gap-2 text-sm font-medium text-slate-800 [&::-webkit-details-marker]:hidden">
          <Code2 className="h-4 w-4" />
          Advanced connection settings
        </summary>
        <textarea
          value={source}
          onChange={(event) => setSource(event.target.value)}
          readOnly={!editing}
          rows={12}
          spellCheck={false}
          className="mt-3 w-full resize-y rounded-md border border-slate-200 bg-slate-50 p-3 font-mono text-xs text-slate-700 focus:border-blue-500 focus:ring-blue-500"
        />
        {isOwner && (
          <div className="mt-2 flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:justify-end">
            {editing ? (
              <>
                <button type="button" onClick={() => { setSource(JSON.stringify(profile?.profile || { auth_type: authType }, null, 2)); setEditing(false); }} disabled={saving} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-slate-200 px-2.5 py-1.5 text-xs font-medium text-slate-600 sm:w-auto">
                  <X className="h-3.5 w-3.5" /> Cancel
                </button>
                {profile?.has_workspace_override && (
                  <button type="button" onClick={reset} disabled={saving} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-slate-200 px-2.5 py-1.5 text-xs font-medium text-slate-600 sm:w-auto">
                    <RotateCcw className="h-3.5 w-3.5" /> Reset to provider default
                  </button>
                )}
                <button type="button" onClick={save} disabled={saving} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md bg-slate-950 px-2.5 py-1.5 text-xs font-medium text-white hover:bg-slate-800 disabled:opacity-50 sm:w-auto">
                  {saving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Save className="h-3.5 w-3.5" />} Save workspace customization
                </button>
              </>
            ) : (
              <button type="button" onClick={() => setEditing(true)} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-slate-200 px-2.5 py-1.5 text-xs font-medium text-slate-700 sm:w-auto">
                <Pencil className="h-3.5 w-3.5" /> Customize for workspace
              </button>
            )}
          </div>
        )}
      </details>
    </section>
  );
}

function profileAuthType(authConfigs: AuthConfig[]): string {
  const auth = authConfigs.find((item) => ["oauth", "oidc", "oauth2", "openIdConnect"].includes(item.type));
  if (!auth) return "";
  return auth.type === "openIdConnect" ? "oidc" : auth.type === "oauth2" ? "oauth" : auth.type;
}

function profileSource(provenance?: WorkspaceConnectionProfile["provenance"]): string {
  return ({ provider: "Provider default", fused: "Fused default", workspace: "Workspace customization" }[provenance || "workspace"]);
}
