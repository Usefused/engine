import { useEffect, useState } from "react";
import { api, type AuthConfig } from "~/lib/api";
import { unifiedAuthOptions } from "~/lib/unified-app-auth";
import { Select } from "~/components/forms/Select";

export interface AppAuthSelection { type: string; name: string; ref?: string }
export interface AppAuthService { key: string; service_id: string; version: string; auth?: AppAuthSelection }

/** Shares exact auth selection across SDK, MCP and Unified App configs without reading credential values. */
export function AppServiceAuthFields({ services, disabled, onChange }: { services: AppAuthService[]; disabled?: boolean; onChange: (key: string, auth?: AppAuthSelection) => void }) {
  // Event-only apps have no outbound auth preference to select.
  if (!services.length) return null;
  return <section className="space-y-3" aria-label="Service authentication">
    <h3 className="text-sm font-semibold text-slate-900">Service authentication</h3>
    <p className="text-xs font-normal text-slate-500">Choose the scheme to use from your bucket. Validation checks it against the selected operations.</p>
    {services.map((service) => <ServiceAuthField key={`${service.key}:${service.service_id}:${service.version}`} service={service} disabled={disabled} onChange={onChange} />)}
  </section>;
}

/** Loads one exact provider version, preserving a saved selection when metadata cannot be fetched. */
function ServiceAuthField({ service, disabled, onChange }: { service: AppAuthService; disabled?: boolean; onChange: (key: string, auth?: AppAuthSelection) => void }) {
  const [auths, setAuths] = useState<AuthConfig[]>([]);
  const [status, setStatus] = useState("Loading auth schemes…");
  useEffect(() => {
    let active = true;
    api.graphql<{ service: { current_service_version: string; auth_configs: AuthConfig[] } | null }>(`query AppServiceAuth($id: String!, $version: String!) { service(id: $id, version: $version) { current_service_version auth_configs { name type scheme deprecated } } }`, { id: service.service_id, version: service.version }).then((result) => {
      // A stale version response must never replace the current selector's options.
      if (!active) return;
      // Registry's fallback version must never supply auth metadata for a different contract.
      if (!result.service || result.service.current_service_version !== service.version) throw new Error("Service version unavailable");
      setAuths(result.service.auth_configs ?? []); setStatus("");
    }).catch(() => {
      // An unavailable catalogue cannot silently clear a reviewed auth choice.
      if (active) setStatus("Auth schemes unavailable. Your existing selection is preserved; auth can also be set in config.");
    });
    return () => { active = false; };
  }, [service.service_id, service.version]);
  const options = unifiedAuthOptions(auths);
  const selected = service.auth ? JSON.stringify([service.auth.type, service.auth.name]) : "";
  /** Changing schemes clears scheme-specific credential references before the next Engine plan. */
  function choose(value: string) {
    const option = options.find((item) => item.key === value);
    // The empty option explicitly restores provider ordering instead of guessing from stored secrets.
    onChange(service.key, option ? { type: option.type, name: option.name } : undefined);
  }
  return <label className="block space-y-2 text-sm font-medium">
    <span className="break-words text-slate-700">{service.key}</span>
    <Select value={selected} disabled={disabled || Boolean(status)} onChange={(event) => choose(event.target.value)} className="w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm">
      <option value="">Provider default (declared order)</option>
      {/* Keep an unavailable saved scheme visible until the user explicitly replaces it. */}
      {selected && !options.some((option) => option.key === selected) ? <option value={selected}>{service.auth!.type} · {service.auth!.name} (saved)</option> : null}
      {options.map((option) => <option key={option.key} value={option.key}>{option.label}</option>)}
    </Select>
    {status ? <span role="status" className="block text-xs font-normal text-slate-500">{status}</span> : null}
  </label>;
}
