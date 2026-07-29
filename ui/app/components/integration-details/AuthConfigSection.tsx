import { FormEvent } from "react";
import { Pencil, X } from "lucide-react";
import { AuthConfig } from "~/lib/api";

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

interface AuthConfigSectionProps {
  srv: any;
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
            <div key={index} className="flex flex-col gap-3 p-4 border border-slate-100 bg-slate-50 rounded-lg relative">
              <button
                data-track="remove_auth_scheme"
                type="button"
                onClick={() => setAuthForm(authForm.filter((_, i) => i !== index))}
                className="absolute top-2 right-2 text-slate-400 hover:text-red-500 cursor-pointer"
                title="Remove scheme"
              >
                <X className="w-4 h-4" />
              </button>
              <div className="flex flex-col sm:flex-row gap-4">
                <div className="flex-1">
                  <label className="block text-xs font-medium text-slate-600 mb-1">Type</label>
                  <select
                    value={
                      auth.type === "api_key"
                        ? "apiKey"
                        : auth.type === "bearer" || auth.type === "basic"
                        ? "http"
                        : auth.type || "apiKey"
                    }
                    onChange={(e) => {
                      const updated = [...authForm];
                      updated[index] = { ...auth, type: e.target.value };
                      setAuthForm(updated);
                    }}
                    className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                  >
                    <option value="apiKey">API Key</option>
                    <option value="http">HTTP</option>
                    <option value="oauth">OAuth2</option>
                    <option value="oidc">OpenID Connect</option>
                    <option value="mtls">Mutual TLS</option>
                  </select>
                </div>

                {["http", "bearer", "basic"].includes(auth.type || "") && (
                  <div className="flex-1">
                    <label className="block text-xs font-medium text-slate-600 mb-1">Scheme</label>
                    <input
                      type="text"
                      value={auth.scheme || ""}
                      onChange={(e) => {
                        const updated = [...authForm];
                        updated[index] = { ...auth, scheme: e.target.value };
                        setAuthForm(updated);
                      }}
                      placeholder="e.g. bearer, basic"
                      className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                    />
                  </div>
                )}

                {["apiKey", "api_key"].includes(auth.type || "") && (
                  <>
                    <div className="flex-1">
                      <label className="block text-xs font-medium text-slate-600 mb-1">Location</label>
                      <select
                        value={auth.location || "header"}
                        onChange={(e) => {
                          const updated = [...authForm];
                          updated[index] = { ...auth, location: e.target.value };
                          setAuthForm(updated);
                        }}
                        className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                      >
                        <option value="header">Header</option>
                        <option value="query">Query</option>
                        <option value="cookie">Cookie</option>
                      </select>
                    </div>
                    <div className="flex-1">
                      <label className="block text-xs font-medium text-slate-600 mb-1">Key Name</label>
                      <input
                        type="text"
                        value={auth.key_name || ""}
                        onChange={(e) => {
                          const updated = [...authForm];
                          updated[index] = { ...auth, key_name: e.target.value };
                          setAuthForm(updated);
                        }}
                        placeholder="e.g. Authorization"
                        className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                      />
                    </div>
                  </>
                )}
              </div>

              {isOAuthConfigType(auth.type) && (
                <div className="flex flex-col gap-3 mt-2">
                  {/* Flow selector */}
                  <div className="flex flex-col sm:flex-row gap-4">
                    <div className="flex-1">
                      <label className="block text-xs font-medium text-slate-600 mb-1">OAuth2 Flow</label>
                      <select
                        value={auth.flow || ""}
                        onChange={(e) => {
                          const updated = [...authForm];
                          const newFlow = e.target.value;
                          const needsAuthUrl = newFlow === "authorizationCode" || newFlow === "implicit";
                          updated[index] = {
                            ...auth,
                            flow: newFlow,
                            ...(needsAuthUrl ? {} : { authorization_url: "" }),
                          };
                          setAuthForm(updated);
                        }}
                        className="w-full text-sm border border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500 px-3 py-2"
                      >
                        <option value="">— select flow —</option>
                        <option value="clientCredentials">Client Credentials (server-to-server)</option>
                        <option value="authorizationCode">Authorization Code</option>
                        <option value="password">Password</option>
                        <option value="implicit">Implicit</option>
                      </select>
                    </div>
                    <div className="flex-1">
                      <label className="block text-xs font-medium text-slate-600 mb-1">Token URL</label>
                      <input
                        type="text"
                        value={auth.token_url || ""}
                        onChange={(e) => {
                          const updated = [...authForm];
                          updated[index] = { ...auth, token_url: e.target.value };
                          setAuthForm(updated);
                        }}
                        placeholder="https://example.com/oauth/token"
                        className="w-full text-sm border border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500 px-3 py-2"
                      />
                    </div>
                  </div>

                  {/* Authorization URL */}
                  {(auth.flow === "authorizationCode" || auth.flow === "implicit" || (!auth.flow && auth.authorization_url)) && (
                    <div>
                      <label className="block text-xs font-medium text-slate-600 mb-1">Authorization URL</label>
                      <input
                        type="text"
                        value={auth.authorization_url || ""}
                        onChange={(e) => {
                          const updated = [...authForm];
                          updated[index] = { ...auth, authorization_url: e.target.value };
                          setAuthForm(updated);
                        }}
                        placeholder="https://example.com/oauth/authorize"
                        className="w-full text-sm border border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500 px-3 py-2"
                      />
                    </div>
                  )}

                  {/* Scopes */}
                  <div>
                    <label className="block text-xs font-medium text-slate-600 mb-1">
                      Scopes
                      {(auth.scopes || []).length > 0 && (
                        <span className="ml-2 text-[10px] font-normal text-slate-400">
                          ({(auth.scopes || []).length} declared — edit below)
                        </span>
                      )}
                    </label>

                    {(auth.scopes || []).length > 0 && (() => {
                      const groups = groupScopesByProduct(auth.scopes || []);
                      const groupNames = Object.keys(groups).sort();
                      return (
                        <div className="flex flex-wrap gap-1.5 mb-2">
                          {groupNames.map((g) => (
                            <span
                              key={g}
                              title={groups[g].join("\n")}
                              className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-blue-50 text-blue-700 text-[11px] font-medium cursor-default hover:bg-blue-100 transition-colors"
                            >
                              {g}
                              <span className="text-blue-400 font-normal">{groups[g].length}</span>
                            </span>
                          ))}
                        </div>
                      );
                    })()}

                    <textarea
                      rows={4}
                      value={(auth.scopes || []).join("\n")}
                      onChange={(e) => {
                        const updated = [...authForm];
                        updated[index] = {
                          ...auth,
                          scopes: e.target.value
                            .split("\n")
                            .map((s) => s.trim())
                            .filter(Boolean),
                        };
                        setAuthForm(updated);
                      }}
                      placeholder="One scope per line"
                      className="w-full text-xs font-mono border border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500 px-3 py-2 resize-none"
                    />
                    <p className="text-[10px] text-slate-400 mt-1">
                      These are declared available scopes, not all required simultaneously.
                    </p>
                  </div>
                </div>
              )}

              {isOIDCConfigType(auth.type) && (
                <div className="flex flex-col sm:flex-row gap-4 mt-2">
                  <div className="flex-1">
                    <label className="block text-xs font-medium text-slate-600 mb-1">OpenID Connect URL</label>
                    <input
                      type="text"
                      value={auth.open_id_connect_url || ""}
                      onChange={(e) => {
                        const updated = [...authForm];
                        updated[index] = { ...auth, open_id_connect_url: e.target.value };
                        setAuthForm(updated);
                      }}
                      placeholder="https://example.com/.well-known/openid-configuration"
                      className="w-full text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                    />
                  </div>
                </div>
              )}
            </div>
          ))}

          <div>
            <button
              data-track="add_auth_scheme"
              type="button"
              onClick={() =>
                setAuthForm([
                  ...authForm,
                  { type: "apiKey", flow: "", location: "header", key_name: "", token_url: "", scopes: [] },
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
            srv.auth_configs.map((auth: any, idx: number) => (
              <div key={idx} className="flex flex-col gap-2 p-3 bg-slate-50 border border-slate-100 rounded">
                <div className="flex flex-col sm:flex-row sm:gap-6 gap-2">
                  <span>
                    <span className="text-slate-400">Type</span> {auth.type}
                  </span>
                  {["http", "bearer", "basic"].includes(auth.type || "") && (
                    <span className="truncate">
                      <span className="text-slate-400">Scheme</span>{" "}
                      {auth.scheme || (auth.type === "http" ? "bearer" : auth.type)}
                    </span>
                  )}
                  {["apiKey", "api_key"].includes(auth.type || "") && auth.key_name && (
                    <span className="truncate">
                      <span className="text-slate-400">Key name</span> {auth.key_name}
                    </span>
                  )}
                </div>
                {isOAuthConfigType(auth.type) && (
                  <>
                    <div className="flex gap-6 flex-wrap">
                      {auth.authorization_url && (
                        <span className="break-all">
                          <span className="text-slate-400">Auth URL</span> {auth.authorization_url}
                        </span>
                      )}
                      {auth.token_url && (
                        <span className="break-all">
                          <span className="text-slate-400">Token URL</span> {auth.token_url}
                        </span>
                      )}
                      {auth.flow && (
                        <span>
                          <span className="text-slate-400">Flow</span> {auth.flow}
                        </span>
                      )}
                    </div>
                    {auth.scopes && auth.scopes.length > 0 && (() => {
                      const groups = groupScopesByProduct(auth.scopes);
                      const groupNames = Object.keys(groups).sort();
                      return (
                        <div className="mt-1">
                          <p className="text-slate-400 text-[11px] mb-1.5">Scopes ({auth.scopes.length})</p>
                          <div className="flex flex-wrap gap-1.5">
                            {groupNames.map((g) => (
                              <span
                                key={g}
                                title={groups[g].join("\n")}
                                className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-slate-100 text-slate-600 text-[11px] font-medium cursor-default hover:bg-slate-200 transition-colors"
                              >
                                {g}
                                <span className="text-slate-400 font-normal">{groups[g].length}</span>
                              </span>
                            ))}
                          </div>
                        </div>
                      );
                    })()}
                  </>
                )}
                {isOIDCConfigType(auth.type) && auth.open_id_connect_url && (
                  <div className="break-all">
                    <span className="text-slate-400">OIDC URL</span> {auth.open_id_connect_url}
                  </div>
                )}
              </div>
            ))
          ) : (
            <span className="text-slate-400">No authentication configured.</span>
          )}
        </div>
      )}
    </>
  );
}
