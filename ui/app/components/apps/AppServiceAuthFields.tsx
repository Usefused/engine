import { useEffect, useState } from "react";
import { api, type AuthConfig } from "~/lib/api";
import { unifiedAuthOptions } from "~/lib/unified-app-auth";
import { Select } from "~/components/forms/Select";

export interface AppAuthSelection { type: string; name: string; ref?: string }
export interface AppAuthService { key: string; service_id: string; version: string; auth?: AppAuthSelection }

/** Presents provider authentication as compact rows, reserving selectors for actual configuration choices. */
export function AppServiceAuthFields({ services, disabled, onChange }: { services: AppAuthService[]; disabled?: boolean; onChange: (key: string, auth?: AppAuthSelection) => void }) {
  // Event-only apps have no outbound auth preference to select.
  if (!services.length) return null;
  return <section className="space-y-2" aria-label="Service authentication">
    <h3 className="text-sm font-medium text-slate-900">Authentication</h3>
    <div className="divide-y divide-slate-100 rounded-lg border border-slate-200 bg-white">
      {services.map((service) => <ServiceAuthField key={`${service.key}:${service.service_id}:${service.version}`} service={service} disabled={disabled} onChange={onChange} />)}
    </div>
    <p className="text-xs font-normal text-slate-500">Credentials come from the selected bucket.</p>
  </section>;
}

/** Loads exact-version names and schemes without replacing saved authentication when metadata is unavailable. */
function ServiceAuthField({ service, disabled, onChange }: { service: AppAuthService; disabled?: boolean; onChange: (key: string, auth?: AppAuthSelection) => void }) {
  const [displayName, setDisplayName] = useState(service.key);
  const [auths, setAuths] = useState<AuthConfig[]>([]);
  const [status, setStatus] = useState("Loading auth schemes…");
  useEffect(() => {
    let active = true;
    api.graphql<{ service: { name: string; current_service_version: string; auth_configs: AuthConfig[] } | null }>(`query AppServiceAuth($id: String!, $version: String!) { service(id: $id, version: $version) { name current_service_version auth_configs { name type scheme deprecated } } }`, { id: service.service_id, version: service.version }).then((result) => {
      // A stale version response must never replace the current selector's options.
      if (!active) return;
      // Registry's fallback version must never supply auth metadata for a different contract.
      if (!result.service || result.service.current_service_version !== service.version) throw new Error("Service version unavailable");
      // Missing display metadata retains the exact qualified identity rather than guessing a provider name.
      setDisplayName(result.service.name || service.key);
      setAuths(result.service.auth_configs ?? []); setStatus("");
    }).catch(() => {
      // An unavailable catalogue cannot silently clear a reviewed auth choice.
      if (active) setStatus("Authentication options unavailable. Your saved selection is preserved.");
    });
    return () => { active = false; };
  }, [service.service_id, service.version]);
  const options = unifiedAuthOptions(auths);
  const selected = service.auth ? JSON.stringify([service.auth.type, service.auth.name]) : "";
  /** Changing schemes clears scheme-specific credential references before the next Engine plan. */
  function choose(value: string) {
    const option = options.find((item) => item.key === value);
    // The empty option lets Engine select a unique compatible credential during planning.
    onChange(service.key, option ? { type: option.type, name: option.name } : undefined);
  }
  // Automatic is the only choice when the contract has no named schemes and no saved override.
  const automaticOnly = !selected && !status && options.length === 0;
  return <div className="space-y-2 px-3 py-3">
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
      <div className="min-w-0 flex-1" title={service.key}>
        <p className="truncate text-sm font-medium text-slate-700">{displayName}</p>
        <p className="mt-0.5 text-xs font-normal text-slate-400">Version {service.version}</p>
      </div>
      {/* Read-only automatic routing needs a status label rather than a dropdown with no alternatives. */}
      {automaticOnly ? <span className="rounded-md bg-slate-100 px-2.5 py-1 text-xs font-medium text-slate-600">Automatic</span> : <Select aria-label={`Authentication for ${displayName}`} value={selected} disabled={disabled || Boolean(status)} onChange={(event) => choose(event.target.value)} className="max-w-full rounded-md border-slate-200 bg-white text-xs sm:max-w-64" density="compact">
        <option value="">Automatic</option>
        {/* Keep an unavailable saved scheme visible until the user explicitly replaces it. */}
        {selected && !options.some((option) => option.key === selected) ? <option value={selected}>{service.auth!.type} · {service.auth!.name} (saved)</option> : null}
        {options.map((option) => <option key={option.key} value={option.key}>{option.label}</option>)}
      </Select>}
    </div>
    {/* Loading and failure remain explicit without crowding the normal, settled row. */}
    {status ? <p role="status" className="text-xs font-normal text-slate-500">{status}</p> : null}
  </div>;
}
