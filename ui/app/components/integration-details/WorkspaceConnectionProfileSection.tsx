import { useEffect, useMemo, useState } from "react";
import { Code2, Loader2, Pencil, RotateCcw, Save, Shield, X } from "lucide-react";
import { api, type AuthConfig, type WorkspaceConnectionProfile } from "~/lib/api";
import { useToast } from "~/components/Toast";

interface Props {
  serviceId: string;
  serviceVersionId?: string;
  serviceVersion?: string;
  authConfigs: AuthConfig[];
  canRead: boolean;
  canManage: boolean;
}

// WorkspaceConnectionProfileSection is deliberately independent of buckets:
// one override affects every app selecting this service/version/auth tuple.
export function WorkspaceConnectionProfileSection({
  serviceId,
  serviceVersionId,
  serviceVersion,
  authConfigs,
  canRead,
  canManage,
}: Props) {
  const toast = useToast();
  const authTypes = useMemo(() => profileAuthTypes(authConfigs), [authConfigs]);
  const [authType, setAuthType] = useState(authTypes[0] || "");
  const ambiguousAuthType = profileAuthConfigs(authConfigs, authType).length > 1;
  const [profile, setProfile] = useState<WorkspaceConnectionProfile | null>(null);
  const [source, setSource] = useState("");
  const [editing, setEditing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  // Keep the selection valid when a version exposes a different set of auth families.
  useEffect(() => {
    if (!authTypes.includes(authType)) setAuthType(authTypes[0] || "");
  }, [authType, authTypes]);

  // Reads the protected profile only after both required grants are known.
  useEffect(() => {
    if (!canRead || !serviceVersionId || !authType || ambiguousAuthType) return;
    setError("");
    api.workspace.getWorkspaceConnectionProfile({ serviceId, serviceVersionId, authType })
      .then((value) => {
        setProfile(value);
        setSource(JSON.stringify(value?.profile || { auth_type: authType }, null, 2));
      })
      .catch((reason) => setError(reason instanceof Error ? reason.message : "Failed to load connection profile"));
  }, [canRead, serviceId, serviceVersionId, authType, ambiguousAuthType]);

  if (!hasProfileIdentity(serviceVersionId, serviceVersion, authType)) return null;
  // The guard above establishes the exact tuple before callbacks capture it.
  const exactVersionID = serviceVersionId as string;
  const exactVersion = serviceVersion as string;
  if (!canRead) {
    return (
      <section className="rounded-lg border border-slate-200 bg-white p-4 text-sm text-slate-500">
        Connection profile access is not available for your account.
      </section>
    );
  }
  if (ambiguousAuthType) {
    return (
      <section className="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800">
        <AuthTypeSelect authTypes={authTypes} authType={authType} setAuthType={setAuthType} />
        <p className="mt-3">
          This version defines multiple {authType.toUpperCase()} schemes. Workspace connection profiles are currently auth-type scoped, so an explicit per-scheme profile cannot be edited here safely.
        </p>
      </section>
    );
  }

  // save validates and replaces the selected auth-family override.
  const save = async () => {
    setSaving(true);
    setError("");
    try {
      const updated = await api.workspace.setWorkspaceConnectionProfile({
        serviceId,
        serviceVersionId: exactVersionID,
        version: exactVersion,
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

  // reset removes the selected auth-family override after explicit confirmation.
  const reset = async () => {
    if (!profile?.has_workspace_override) return;
    const confirmed = await toast.confirm(
      "Reset this workspace customization? Every app selecting this service version and auth type will use the provider default."
    );
    if (!confirmed) return;
    setSaving(true);
    setError("");
    try {
      const updated = await api.workspace.resetWorkspaceConnectionProfile({ serviceId, serviceVersionId: exactVersionID, authType });
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
      <AuthTypeSelect authTypes={authTypes} authType={authType} setAuthType={setAuthType} />
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
        {canManage && (
          <ConnectionProfileActions
            editing={editing}
            saving={saving}
            hasOverride={Boolean(profile?.has_workspace_override)}
            onCancel={() => { setSource(JSON.stringify(profile?.profile || { auth_type: authType }, null, 2)); setEditing(false); }}
            onEdit={() => setEditing(true)}
            onReset={reset}
            onSave={save}
          />
        )}
      </details>
    </section>
  );
}

// hasProfileIdentity keeps the UI from issuing partial profile tuple reads.
function hasProfileIdentity(serviceVersionId?: string, serviceVersion?: string, authType?: string): boolean {
  return Boolean(serviceVersionId && serviceVersion && authType);
}

interface ConnectionProfileActionsProps {
  editing: boolean;
  saving: boolean;
  hasOverride: boolean;
  onCancel: () => void;
  onEdit: () => void;
  onReset: () => void;
  onSave: () => void;
}

// ConnectionProfileActions keeps edit-state controls separate from profile data flow.
function ConnectionProfileActions({ editing, saving, hasOverride, onCancel, onEdit, onReset, onSave }: ConnectionProfileActionsProps) {
  if (!editing) {
    return (
      <div className="mt-2 flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:justify-end">
        <button type="button" onClick={onEdit} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-slate-200 px-2.5 py-1.5 text-xs font-medium text-slate-700 sm:w-auto">
          <Pencil className="h-3.5 w-3.5" /> Customize for workspace
        </button>
      </div>
    );
  }
  return (
    <div className="mt-2 flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:justify-end">
      <button type="button" onClick={onCancel} disabled={saving} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-slate-200 px-2.5 py-1.5 text-xs font-medium text-slate-600 sm:w-auto">
        <X className="h-3.5 w-3.5" /> Cancel
      </button>
      {hasOverride && (
        <button type="button" onClick={onReset} disabled={saving} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-slate-200 px-2.5 py-1.5 text-xs font-medium text-slate-600 sm:w-auto">
          <RotateCcw className="h-3.5 w-3.5" /> Reset to provider default
        </button>
      )}
      <button type="button" onClick={onSave} disabled={saving} className="inline-flex w-full items-center justify-center gap-1.5 rounded-md bg-slate-950 px-2.5 py-1.5 text-xs font-medium text-white hover:bg-slate-800 disabled:opacity-50 sm:w-auto">
        {saving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Save className="h-3.5 w-3.5" />} Save workspace customization
      </button>
    </div>
  );
}

// AuthTypeSelect switches between Engine-supported profile families when necessary.
function AuthTypeSelect({ authTypes, authType, setAuthType }: {
  authTypes: string[];
  authType: string;
  setAuthType: (value: string) => void;
}) {
  if (authTypes.length < 2) return null;
  return (
    <label className="mt-4 block text-xs font-medium text-slate-700">
      Authentication family
      <select
        value={authType}
        onChange={(event) => setAuthType(event.target.value)}
        className="mt-1 block w-full rounded-md border border-slate-200 bg-white px-3 py-2 text-sm"
      >
        {authTypes.map((value) => <option key={value} value={value}>{value.toUpperCase()}</option>)}
      </select>
    </label>
  );
}

// canonicalProfileAuthType maps Registry spellings onto Engine profile families.
function canonicalProfileAuthType(auth: AuthConfig): string {
  if (auth.type === "openIdConnect") return "oidc";
  if (auth.type === "oauth2") return "oauth";
  return ["oauth", "oidc"].includes(auth.type) ? auth.type : "";
}

// profileAuthTypes returns each editable Engine profile family once.
function profileAuthTypes(authConfigs: AuthConfig[]): string[] {
  return Array.from(new Set(authConfigs.map(canonicalProfileAuthType).filter(Boolean)));
}

// profileAuthConfigs identifies ambiguity the auth-type-only Engine store cannot represent.
function profileAuthConfigs(authConfigs: AuthConfig[], authType: string): AuthConfig[] {
  return authConfigs.filter((auth) => canonicalProfileAuthType(auth) === authType);
}

// profileSource converts stored provenance into a concise UI label.
function profileSource(provenance?: WorkspaceConnectionProfile["provenance"]): string {
  return ({ provider: "Provider default", fused: "Fused default", workspace: "Workspace customization" }[provenance || "workspace"]);
}
