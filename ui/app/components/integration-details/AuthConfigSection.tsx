import { FormEvent } from "react";
import { Pencil, X } from "lucide-react";
import { type AuthConfig, type OAuth2FlowContract, type OAuth2FlowName, type Service } from "~/lib/api";
import {
  emptyOAuth2Flow,
  firstAvailableOAuth2Flow,
  oauth2FlowEntries,
  oauth2FlowNames,
  oauth2FlowNeedsAuthorizationURL,
  oauth2FlowNeedsTokenURL,
  oauth2ScopeNames,
  removeOAuth2Flow,
  renameOAuth2Flow,
  replaceOAuth2Scopes,
  updateOAuth2Flow,
} from "~/lib/oauth2-flows";

function scopeGroup(scope: string): string {
  try {
    const url = new URL(scope);
    const parts = url.pathname.replace(/^\//, "").split("/");
    const servicesIdx = parts.indexOf("services");
    if (servicesIdx !== -1 && parts[servicesIdx + 1]) return parts[servicesIdx + 1];
    const meaningful = parts.filter(p => p && p.length > 1).slice(-2, -1)[0];
    return meaningful || parts[0] || "other";
  } catch {
    const colon = scope.indexOf(":");
    if (colon !== -1) return scope.slice(0, colon);
    const slash = scope.indexOf("/");
    if (slash !== -1) return scope.slice(0, slash);
    return scope || "other";
  }
}

function groupScopesByProduct(scopes: string[]): Record<string, string[]> {
  const groups: Record<string, string[]> = {};
  for (const s of scopes) {
    const g = scopeGroup(s);
    if (!groups[g]) groups[g] = [];
    groups[g].push(s);
  }
  return groups;
}

// isOAuthConfigType keeps new selections on public auth families while still
// rendering existing imported OAuth metadata until services are re-saved.
function isOAuthConfigType(type?: string): boolean {
  return type === "oauth" || type === "oauth2";
}

// isOIDCConfigType mirrors the OAuth helper for imported OIDC metadata without
// reintroducing imported names into the select options.
function isOIDCConfigType(type?: string): boolean {
  return type === "oidc" || type === "openIdConnect";
}

function isHTTPConfigType(type?: string): boolean {
  return ["http", "bearer", "basic"].includes(type ?? "");
}

function isAPIKeyConfigType(type?: string): boolean {
  return ["apiKey", "api_key"].includes(type ?? "");
}

function authSchemeSummary(auth: AuthConfig): string | undefined {
  if (auth.scheme) return auth.scheme;
  return auth.type === "http" ? "bearer" : auth.type;
}

interface OAuth2FlowEditorProps {
  auth: AuthConfig;
  onChange: (auth: AuthConfig) => void;
}

function OAuth2FlowEditor({ auth, onChange }: OAuth2FlowEditorProps) {
  const entries = oauth2FlowEntries(auth);
  const available = firstAvailableOAuth2Flow(auth);
  return (
    <div className="flex flex-col gap-3 mt-2">
      {entries.map(([name, flow]) => (
        <OAuth2FlowFields key={name} auth={auth} name={name} flow={flow} onChange={onChange} />
      ))}
      {available && (
        <button
          type="button"
          onClick={() => onChange(updateOAuth2Flow(auth, available, emptyOAuth2Flow()))}
          className="self-start text-xs font-medium text-blue-600 hover:text-blue-700 hover:underline"
        >
          + Add OAuth2 flow
        </button>
      )}
      {entries.length === 0 && <p className="text-xs text-amber-700">Add the documented OAuth2 flow before saving.</p>}
    </div>
  );
}

interface OAuth2FlowFieldsProps extends OAuth2FlowEditorProps {
  name: OAuth2FlowName;
  flow: OAuth2FlowContract;
}

function OAuth2FlowFields({ auth, name, flow, onChange }: OAuth2FlowFieldsProps) {
  const scopes = oauth2ScopeNames(flow);
  const update = (next: OAuth2FlowContract) => onChange(updateOAuth2Flow(auth, name, next));
  return (
    <div className="flex flex-col gap-3 rounded-md border border-slate-200 bg-white p-3">
      <div className="flex gap-3">
        <div className="flex-1">
          <label className="block text-xs font-medium text-slate-600 mb-1">OAuth2 Flow</label>
          <select
            value={name}
            onChange={(event) => onChange(renameOAuth2Flow(auth, name, event.target.value as OAuth2FlowName))}
            className="w-full text-sm border border-slate-200 rounded-md px-3 py-2"
          >
            {oauth2FlowNames.map((candidate) => (
              <option key={candidate} value={candidate} disabled={candidate !== name && Boolean(auth.oauth2_flows?.[candidate])}>
                {candidate}
              </option>
            ))}
          </select>
        </div>
        <button type="button" onClick={() => onChange(removeOAuth2Flow(auth, name))} className="self-end p-2 text-slate-400 hover:text-red-500" title="Remove OAuth2 flow">
          <X className="h-4 w-4" />
        </button>
      </div>
      {oauth2FlowNeedsTokenURL(name) && (
        <OAuth2URLField label="Token URL" value={flow.token_url} onChange={(token_url) => update({ ...flow, token_url })} />
      )}
      {oauth2FlowNeedsAuthorizationURL(name) && (
        <OAuth2URLField label="Authorization URL" value={flow.authorization_url} onChange={(authorization_url) => update({ ...flow, authorization_url })} />
      )}
      {name === "deviceAuthorization" && (
        <OAuth2URLField label="Device Authorization URL" value={flow.device_authorization_url} onChange={(device_authorization_url) => update({ ...flow, device_authorization_url })} />
      )}
      <label className="block text-xs font-medium text-slate-600">
        Scopes
        <textarea
          rows={4}
          value={scopes.join("\n")}
          onChange={(event) => update(replaceOAuth2Scopes(flow, event.target.value.split("\n")))}
          placeholder="One scope per line"
          className="mt-1 w-full resize-none rounded-md border border-slate-200 px-3 py-2 font-mono text-xs"
        />
      </label>
    </div>
  );
}

function OAuth2URLField({ label, value, onChange }: { label: string; value?: string; onChange: (value: string) => void }) {
  return (
    <label className="block text-xs font-medium text-slate-600">
      {label}
      <input type="text" value={value ?? ""} onChange={(event) => onChange(event.target.value)} className="mt-1 w-full rounded-md border border-slate-200 px-3 py-2 text-sm" />
    </label>
  );
}

function OAuth2FlowSummary({ auth }: { auth: AuthConfig }) {
  return (
    <div className="flex flex-col gap-2">
      {oauth2FlowEntries(auth).map(([name, flow]) => (
        <div key={name} className="rounded border border-slate-200 bg-white p-2">
          <div className="font-medium text-slate-700">{name}</div>
          {flow.authorization_url && <div className="break-all text-xs"><span className="text-slate-400">Auth URL</span> {flow.authorization_url}</div>}
          {flow.token_url && <div className="break-all text-xs"><span className="text-slate-400">Token URL</span> {flow.token_url}</div>}
          <OAuth2ScopeSummary scopes={oauth2ScopeNames(flow)} />
        </div>
      ))}
    </div>
  );
}

function OAuth2ScopeSummary({ scopes }: { scopes: string[] }) {
  if (scopes.length === 0) return null;
  const groups = groupScopesByProduct(scopes);
  return (
    <div className="mt-1 flex flex-wrap gap-1.5">
      {Object.keys(groups).sort().map((group) => (
        <span key={group} title={groups[group].join("\n")} className="rounded-full bg-slate-100 px-2 py-0.5 text-[11px] text-slate-600">
          {group} <span className="text-slate-400">{groups[group].length}</span>
        </span>
      ))}
    </div>
  );
}

function editorAuthType(type?: string): string {
  if (type === "api_key") return "apiKey";
  if (type === "bearer" || type === "basic") return "http";
  return type || "apiKey";
}

function replaceAuthAt(authConfigs: AuthConfig[], index: number, auth: AuthConfig): AuthConfig[] {
  return authConfigs.map((candidate, candidateIndex) => candidateIndex === index ? auth : candidate);
}

interface EditableAuthConfigProps {
  auth: AuthConfig;
  onChange: (auth: AuthConfig) => void;
  onRemove: () => void;
}

function EditableAuthConfig({ auth, onChange, onRemove }: EditableAuthConfigProps) {
  return (
    <div className="flex flex-col gap-3 p-4 border border-slate-100 bg-slate-50 rounded-lg relative">
      <button data-track="remove_auth_scheme" type="button" onClick={onRemove} className="absolute top-2 right-2 text-slate-400 hover:text-red-500 cursor-pointer" title="Remove scheme">
        <X className="w-4 h-4" />
      </button>
      <div className="flex flex-col sm:flex-row gap-4">
        <div className="flex-1">
          <label className="block text-xs font-medium text-slate-600 mb-1">Type</label>
          <select value={editorAuthType(auth.type)} onChange={(event) => onChange({ ...auth, type: event.target.value })} className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500">
            <option value="apiKey">API Key</option>
            <option value="http">HTTP</option>
            <option value="oauth">OAuth2</option>
            <option value="oidc">OpenID Connect</option>
            <option value="mtls">Mutual TLS</option>
          </select>
        </div>
        {isHTTPConfigType(auth.type) && (
          <div className="flex-1">
            <label className="block text-xs font-medium text-slate-600 mb-1">Scheme</label>
            <input type="text" value={auth.scheme || ""} onChange={(event) => onChange({ ...auth, scheme: event.target.value })} placeholder="e.g. bearer, basic" className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500" />
          </div>
        )}
        {isAPIKeyConfigType(auth.type) && (
          <>
            <div className="flex-1">
              <label className="block text-xs font-medium text-slate-600 mb-1">Location</label>
              <select value={auth.location || "header"} onChange={(event) => onChange({ ...auth, location: event.target.value })} className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500">
                <option value="header">Header</option>
                <option value="query">Query</option>
                <option value="cookie">Cookie</option>
              </select>
            </div>
            <div className="flex-1">
              <label className="block text-xs font-medium text-slate-600 mb-1">Key Name</label>
              <input type="text" value={auth.key_name || ""} onChange={(event) => onChange({ ...auth, key_name: event.target.value })} placeholder="e.g. Authorization" className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500" />
            </div>
          </>
        )}
      </div>
      {isOAuthConfigType(auth.type) && <OAuth2FlowEditor auth={auth} onChange={onChange} />}
      {isOIDCConfigType(auth.type) && (
        <div className="flex flex-col sm:flex-row gap-4 mt-2">
          <div className="flex-1">
            <label className="block text-xs font-medium text-slate-600 mb-1">OpenID Connect URL</label>
            <input type="text" value={auth.open_id_connect_url || ""} onChange={(event) => onChange({ ...auth, open_id_connect_url: event.target.value })} placeholder="https://example.com/.well-known/openid-configuration" className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500" />
          </div>
        </div>
      )}
    </div>
  );
}

function AuthConfigSummary({ auth }: { auth: AuthConfig }) {
  return (
    <div className="flex flex-col gap-2 p-3 bg-slate-50 border border-slate-100 rounded">
      <div className="flex flex-col sm:flex-row sm:gap-6 gap-2">
        <span><span className="text-slate-400">Type</span> {auth.type}</span>
        {isHTTPConfigType(auth.type) && (
          <span className="truncate"><span className="text-slate-400">Scheme</span>{" "}{authSchemeSummary(auth)}</span>
        )}
        {isAPIKeyConfigType(auth.type) && auth.key_name && (
          <span className="truncate"><span className="text-slate-400">Key name</span> {auth.key_name}</span>
        )}
      </div>
      {isOAuthConfigType(auth.type) && <OAuth2FlowSummary auth={auth} />}
      {isOIDCConfigType(auth.type) && auth.open_id_connect_url && (
        <div className="break-all"><span className="text-slate-400">OIDC URL</span> {auth.open_id_connect_url}</div>
      )}
    </div>
  );
}

interface AuthConfigSectionProps {
  srv: Service;
  isAuth: boolean;
  editingAuth: boolean;
  setEditingAuth: (editing: boolean) => void;
  authForm: AuthConfig[] | null;
  setAuthForm: React.Dispatch<React.SetStateAction<AuthConfig[] | null>>;
  savingAuth: boolean;
  handleSaveAuth: (e: FormEvent) => void;
}

export function AuthConfigSection({
  srv,
  isAuth,
  editingAuth,
  setEditingAuth,
  authForm,
  setAuthForm,
  savingAuth,
  handleSaveAuth,
}: AuthConfigSectionProps) {
  return (
    <>
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-3">
          <h2 className="text-sm font-semibold text-slate-800">Auth Configurations</h2>
        </div>
        {!editingAuth && isAuth && (
          <button
            data-track="edit_auth_config"
            onClick={() => {
              setAuthForm(srv.auth_configs || []);
              setEditingAuth(true);
            }}
            className="p-1 text-slate-400 hover:text-slate-600 transition-colors cursor-pointer"
            title="Edit Auth Config"
          >
            <Pencil className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {editingAuth && authForm ? (
        <form 
          onSubmit={handleSaveAuth} 
          className="flex flex-col gap-4"
          toolname="save_auth_config"
          tooldescription="Save the authentication configurations for the service."
        >
          {authForm.map((auth, index) => (
            <EditableAuthConfig
              key={index}
              auth={auth}
              onChange={(next) => setAuthForm(replaceAuthAt(authForm, index, next))}
              onRemove={() => setAuthForm(authForm.filter((_, candidateIndex) => candidateIndex !== index))}
            />
          ))}

          <div>
            <button
              data-track="add_auth_scheme"
              type="button"
              onClick={() =>
                setAuthForm([
                  ...authForm,
                  { type: "apiKey", location: "header", key_name: "" },
                ])
              }
              className="text-xs font-medium text-blue-600 hover:text-blue-700 hover:underline cursor-pointer"
            >
              + Add Auth Scheme
            </button>
          </div>

          <div className="flex justify-end gap-2 mt-2">
            <button
              data-track="cancel_edit_auth"
              type="button"
              onClick={() => setEditingAuth(false)}
              className="px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-50 rounded border border-slate-200 transition-colors cursor-pointer"
            >
              Cancel
            </button>
            <button
              data-track="save_auth_config"
              type="submit"
              disabled={savingAuth}
              className="px-3 py-1.5 text-sm font-medium text-white bg-blue-600 hover:bg-blue-700 rounded disabled:opacity-50 transition-colors"
            >
              {savingAuth ? "Saving..." : "Save"}
            </button>
          </div>
        </form>
      ) : (
        <div className="flex flex-col gap-4 text-sm text-slate-600">
          {srv.auth_configs && srv.auth_configs.length > 0 ? (
            srv.auth_configs.map((auth, idx: number) => <AuthConfigSummary key={idx} auth={auth} />)
          ) : (
            <span className="text-slate-400">No authentication configured.</span>
          )}
        </div>
      )}
    </>
  );
}
