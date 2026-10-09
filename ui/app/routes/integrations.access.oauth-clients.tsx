import { FieldLabel } from "~/components/forms/FieldLabel";
import { Select } from "../components/forms/Select.ts";
import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import type { MetaFunction } from "@remix-run/react";
import { KeyRound, Plus, Ban, X } from "lucide-react";
import { useToast } from "~/components/Toast";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { AccessTabs } from "~/components/access/AccessTabs";
import {
  createOAuthClient,
  listOAuthClients,
  listOAuthScopeCatalog,
  revokeOAuthClient,
  type OAuthClient,
  type OAuthClientType,
  type OAuthScope,
} from "~/lib/oauth-clients";

export const meta: MetaFunction = () => [{ title: "OAuth Clients - Fused" }];

/** Shares responsive Access navigation while retaining this page's permission boundary. */
export default function OAuthClientsPage() {
  const { access } = useCurrentActorAccess();
  return (
    <>
      <AccessTabs />
      <WorkspacePermissionGate permission="access.read" area="OAuth client registrations">
        <OAuthClientsManager canManage={hasWorkspacePermission(access, "access.manage")} />
      </WorkspacePermissionGate>
    </>
  );
}

/** Coordinates listing, registering, and revoking third-party OAuth clients that can request delegated access to this workspace. */
function OAuthClientsManager({ canManage }: { canManage: boolean }) {
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const [clients, setClients] = useState<OAuthClient[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [clientType, setClientType] = useState<OAuthClientType>("CONFIDENTIAL");
  const [redirectURIs, setRedirectURIs] = useState("");
  const [allowedScopes, setAllowedScopes] = useState<string[]>([]);
  const [scopeCatalog, setScopeCatalog] = useState<OAuthScope[]>([]);

  // Fetch the scope catalog once from the server (not hardcoded) so its
  // value/label pairs always reflect the Engine's current permission set.
  useEffect(() => {
    listOAuthScopeCatalog().catch(() => []).then((catalog) => setScopeCatalog(catalog ?? []));
  }, []);

  // Restrict the scope picker to permissions the actor actually holds on this
  // workspace (least-privilege default) instead of free text, so a client can
  // never be registered with a scope the registering admin can't themselves
  // exercise, and the admin never has to guess a scope's exact spelling.
  const availableScopes = useMemo<OAuthScope[]>(() => {
    if (!access) return [];
    const labels = new Map(scopeCatalog.map((scope) => [scope.value, scope.label]));
    const granted = new Set(
      access.grants
        .filter((grant) => grant.resource_type === "WORKSPACE" && grant.resource_id === access.workspace_id)
        .map((grant) => grant.permission)
    );
    // Retired or unknown grants must not reappear as selectable OAuth scopes.
    return Array.from(granted)
      .filter((value) => labels.has(value))
      .map((value) => ({ value, label: labels.get(value) ?? value }))
      .sort((a, b) => a.label.localeCompare(b.label));
  }, [access, scopeCatalog]);

  const refresh = useCallback(async () => {
    const items = await listOAuthClients();
    setClients(items);
  }, []);

  useEffect(() => {
    setLoading(true);
    refresh().catch((error: unknown) => toast.error(errorMessage(error))).finally(() => setLoading(false));
  }, [refresh]);

  async function handleCreate(event: FormEvent) {
    event.preventDefault();
    const redirectList = splitLines(redirectURIs);
    // Both fields are required by the server-side validation too; checking
    // here just avoids a round-trip for an obviously incomplete form.
    if (!name.trim() || redirectList.length === 0 || allowedScopes.length === 0) return;
    setSaving(true);
    try {
      const payload = await createOAuthClient({
        name: name.trim(),
        client_type: clientType,
        redirect_uris: redirectList,
        allowed_scopes: allowedScopes,
      });
      setName("");
      setRedirectURIs("");
      setAllowedScopes([]);
      // Only a confidential client gets an issued secret; public clients rely
      // on PKCE alone and never receive one.
      setCreatedSecret(payload.client_secret);
      await refresh();
      toast.success("OAuth client registered.");
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  async function handleRevoke(client: OAuthClient) {
    const confirmed = await toast.confirm(`Revoke "${client.name}"? Every token it has issued will stop working immediately.`);
    if (!confirmed) return;
    setSaving(true);
    try {
      await revokeOAuthClient(client.id);
      await refresh();
      toast.success("OAuth client revoked.");
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-6">
      <header>
        <p className="text-sm font-medium text-blue-600">Access</p>
        <h1 className="text-2xl font-bold text-slate-900 flex items-center gap-2"><KeyRound className="w-6 h-6" /> OAuth Clients</h1>
        <p className="text-slate-500 mt-1">Register third-party applications that can request delegated access to this workspace via OAuth2.</p>
      </header>

      {canManage && (
        <OAuthClientCreateForm
          name={name}
          clientType={clientType}
          redirectURIs={redirectURIs}
          allowedScopes={allowedScopes}
          availableScopes={availableScopes}
          saving={saving}
          onName={setName}
          onClientType={setClientType}
          onRedirectURIs={setRedirectURIs}
          onAllowedScopes={setAllowedScopes}
          onSubmit={handleCreate}
        />
      )}

      {createdSecret && <CreatedSecretNotice secret={createdSecret} onClear={() => setCreatedSecret(null)} />}

      <OAuthClientsList clients={clients} loading={loading} canManage={canManage} saving={saving} onRevoke={handleRevoke} />
    </div>
  );
}

/** Uses the shared select while keeping selection state and actions owned by this page. */
function OAuthClientCreateForm(props: {
  name: string;
  clientType: OAuthClientType;
  redirectURIs: string;
  allowedScopes: string[];
  availableScopes: OAuthScope[];
  saving: boolean;
  onName: (value: string) => void;
  onClientType: (value: OAuthClientType) => void;
  onRedirectURIs: (value: string) => void;
  onAllowedScopes: (value: string[]) => void;
  onSubmit: (event: FormEvent) => void;
}) {
  return (
    <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm">
      <h2 className="text-base font-semibold text-slate-900 mb-3">Register a client</h2>
      <form onSubmit={props.onSubmit} className="grid min-w-0 grid-cols-1 gap-4 md:grid-cols-2">
        <label className="flex min-w-0 flex-col gap-1">
          <span className="text-xs font-medium text-slate-700"><FieldLabel required>Client name</FieldLabel></span>
          <input value={props.name} onChange={(event) => props.onName(event.target.value)} required maxLength={100} placeholder="Third-party app name" className="min-h-11 w-full min-w-0 rounded-lg border border-slate-300 px-3 py-2 text-base sm:text-sm" />
        </label>
        <label className="flex min-w-0 flex-col gap-1">
          <span className="text-xs font-medium text-slate-700"><FieldLabel>Client type</FieldLabel></span>
          <Select aria-describedby="oauth-client-type-help" value={props.clientType} onChange={(event) => props.onClientType(event.target.value as OAuthClientType)} className="min-h-11 w-full min-w-0 rounded-lg border border-slate-300 px-3 py-2 text-base sm:text-sm">
            <option value="CONFIDENTIAL">Confidential</option>
            <option value="PUBLIC">Public</option>
          </Select>
          {/* Explain the selected authentication model without making the native select wider than its card. */}
          <span id="oauth-client-type-help" className="text-xs leading-5 text-slate-500">{props.clientType === "CONFIDENTIAL" ? "Server-side app. Uses a client secret." : "Native or browser app. Uses PKCE without a secret."}</span>
        </label>
        <label className="flex min-w-0 flex-col gap-1 md:col-span-2">
          <span className="text-xs font-medium text-slate-700"><FieldLabel required>Redirect URIs (one per line)</FieldLabel></span>
          <textarea value={props.redirectURIs} onChange={(event) => props.onRedirectURIs(event.target.value)} required rows={2} placeholder="https://app.example.com/oauth/callback" className="min-h-11 w-full min-w-0 rounded-lg border border-slate-300 px-3 py-2 text-base sm:text-sm font-mono" />
        </label>
        <div className="flex min-w-0 flex-col gap-1 md:col-span-2">
          <span className="text-xs font-medium text-slate-700">Allowed scopes</span>
          <ScopeMultiSelect value={props.allowedScopes} options={props.availableScopes} onChange={props.onAllowedScopes} />
          {props.availableScopes.length === 0 && (
            <p className="text-xs text-amber-600">You have no workspace scopes to grant yet.</p>
          )}
        </div>
        <div className="md:col-span-2">
          <button type="submit" disabled={props.saving} className="inline-flex min-h-11 w-full items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-semibold text-white hover:bg-slate-800 disabled:opacity-50 sm:w-auto">
            <Plus className="w-4 h-4" /> Register client
          </button>
        </div>
      </form>
    </section>
  );
}

/**
 * Multi-select "tag" combobox for OAuth scopes. Chosen scopes render as
 * removable tags; the text field filters `options` (the actor's own granted
 * workspace permissions) by value or label as the admin types, so scopes are
 * picked from a searchable list instead of typed from memory.
 */
function ScopeMultiSelect({
  value,
  options,
  onChange,
}: {
  value: string[];
  options: OAuthScope[];
  onChange: (next: string[]) => void;
}) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  // Narrow the dropdown to scopes not already chosen and matching the typed
  // query (by raw permission string or its label), so results tighten as the
  // admin types instead of requiring an exact scope string up front.
  const needle = query.trim().toLowerCase();
  const suggestions = options.filter((option) => {
    if (value.includes(option.value)) return false;
    if (!needle) return true;
    return option.value.toLowerCase().includes(needle) || option.label.toLowerCase().includes(needle);
  });

  useEffect(() => {
    // Close the dropdown on outside clicks, matching normal combobox behavior.
    function handleClick(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("mousedown", handleClick);
    return () => document.removeEventListener("mousedown", handleClick);
  }, []);

  // Adds a scope tag and resets the search text so the next keystroke starts
  // a fresh search instead of continuing to filter against the old query.
  function addScope(scope: string) {
    onChange([...value, scope]);
    setQuery("");
  }

  // Removes a single scope tag, used by both the tag's own remove button and
  // the Backspace-on-empty-query shortcut below.
  function removeScope(scope: string) {
    onChange(value.filter((item) => item !== scope));
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Enter") {
      // Enter commits the top suggestion instead of submitting the form, so
      // picking a scope never requires reaching for the mouse.
      event.preventDefault();
      if (suggestions.length > 0) addScope(suggestions[0].value);
    } else if (event.key === "Backspace" && query === "" && value.length > 0) {
      // Backspace on an empty query pops the most recently added tag, mirroring
      // familiar tag-input UX (e.g. an email "To" field).
      removeScope(value[value.length - 1]);
    }
  }

  return (
    <div ref={containerRef} className="relative min-w-0">
      <div className="flex flex-wrap items-center gap-1.5 rounded-lg border border-slate-300 px-2 py-1.5 focus-within:ring-2 focus-within:ring-slate-400">
        {value.map((scope) => (
          <span key={scope} className="inline-flex max-w-full items-center gap-1 rounded bg-slate-100 px-2 py-0.5 text-xs font-mono text-slate-800">
            <span className="min-w-0 break-all">{scope}</span>
            <button type="button" onClick={() => removeScope(scope)} className="shrink-0 p-1 text-slate-500 hover:text-slate-800" aria-label={`Remove scope ${scope}`}>
              <X className="w-3 h-3" />
            </button>
          </span>
        ))}
        <input
          value={query}
          onChange={(event) => { setQuery(event.target.value); setOpen(true); }}
          onFocus={() => setOpen(true)}
          onKeyDown={handleKeyDown}
          placeholder={value.length === 0 ? "Search scopes…" : ""}
          className="min-h-9 min-w-0 flex-[1_1_8rem] border-none py-0.5 text-base outline-none sm:text-sm"
          aria-label="Search allowed scopes"
          role="combobox"
          aria-expanded={open}
          aria-autocomplete="list"
        />
      </div>
      {open && suggestions.length > 0 && (
        <ul role="listbox" className="absolute z-10 mt-1 max-h-56 w-full overflow-auto rounded-lg border border-slate-200 bg-white shadow-lg">
          {suggestions.map((option) => (
            <li key={option.value}>
              <button
                type="button"
                onClick={() => addScope(option.value)}
                className="flex min-h-11 w-full flex-col items-start px-3 py-2 text-left hover:bg-slate-50"
              >
                <span className="max-w-full break-all text-sm font-mono text-slate-900">{option.value}</span>
                <span className="text-xs text-slate-500">{option.label}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {open && suggestions.length === 0 && (
        <div className="absolute z-10 mt-1 w-full rounded-lg border border-slate-200 bg-white p-3 text-xs text-slate-500 shadow-lg">
          {needle ? "No matching scopes you have access to." : "No more scopes available to add."}
        </div>
      )}
    </div>
  );
}

/** Allows copying a new client credential without exposing it to automatic agent context. */
function CreatedSecretNotice({ secret, onClear }: { secret: string | null; onClear: () => void }) {
  return (
    <div className="rounded-lg border border-amber-300 bg-amber-50 p-4" role="alert">
      <p className="text-sm font-semibold text-amber-900">
        {secret ? "Copy this client secret now. It will not be shown again." : "This is a public client. It authenticates with PKCE only and has no secret."}
      </p>
      {secret && (
        <>
          <p className="text-xs text-amber-800 mt-1">Keep it secret. Anyone with this secret can request tokens as this client.</p>
          <code data-fused-visible="false" className="block mt-3 break-all rounded bg-white p-2 text-xs text-slate-900">{secret}</code>
        </>
      )}
      <div className="mt-3 flex gap-2">
        {secret && <button type="button" onClick={() => navigator.clipboard.writeText(secret)} className="rounded border border-slate-300 bg-white px-3 py-1.5 text-xs font-semibold text-slate-700 hover:bg-slate-50">Copy secret</button>}
        <button type="button" onClick={onClear} className="rounded border border-slate-300 px-3 py-1.5 text-xs font-semibold text-slate-950">I've saved it</button>
      </div>
    </div>
  );
}

/** Presents readable client rows and a settled empty state without changing registration permissions. */
function OAuthClientsList(props: { clients: OAuthClient[]; loading: boolean; canManage: boolean; saving: boolean; onRevoke: (client: OAuthClient) => void }) {
  return (
    <section className="bg-white border border-slate-200 rounded-xl shadow-sm overflow-hidden">
      <div className="px-4 py-3 border-b border-slate-200">
        <h2 className="font-semibold text-slate-900">Registered clients</h2>
      </div>
      <div className="divide-y divide-slate-100">
        {props.loading && <p className="p-4 text-sm text-slate-500">Loading OAuth clients…</p>}
        {/* Wait for the list before offering guidance, and only suggest registration to managers. */}
        {!props.loading && props.clients.length === 0 && <div className="flex flex-col items-center px-5 py-8 text-center">
          <span className="mb-3 rounded-2xl bg-slate-100 p-3 text-slate-400"><KeyRound className="h-6 w-6" aria-hidden="true" /></span>
          <p className="text-sm font-semibold text-slate-900">No clients registered yet</p>
          <p className="mt-2 max-w-sm text-sm leading-6 text-slate-500">{props.canManage ? "Register a client above to let an app request access to this workspace." : "Registered OAuth clients will appear here."}</p>
        </div>}
        {props.clients.map((client) => (
          <OAuthClientRow key={client.id} client={client} canManage={props.canManage} saving={props.saving} onRevoke={props.onRevoke} />
        ))}
      </div>
    </section>
  );
}

/** Keeps client identity readable and moves revocation below the details on narrow screens. */
function OAuthClientRow({ client, canManage, saving, onRevoke }: { client: OAuthClient; canManage: boolean; saving: boolean; onRevoke: (client: OAuthClient) => void }) {
  const revoked = Boolean(client.revoked_at);
  return (
    <div className="flex flex-col gap-3 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
      <div className="min-w-0 flex-1">
        <p className="break-words text-sm font-medium text-slate-800">{client.name}</p>
        <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-slate-500">
          {/* Keep the existing public/confidential and revocation distinctions visible at every width. */}
          <span className="rounded-md bg-slate-100 px-2 py-1">{client.client_type === "CONFIDENTIAL" ? "Confidential" : "Public"}</span>
          <span>{revoked ? "Revoked" : "Active"}</span>
        </div>
        <p className="mt-2 break-all font-mono text-xs leading-5 text-slate-500">{client.client_id}</p>
        <p className="break-words text-xs leading-5 text-slate-500 mt-1 [overflow-wrap:anywhere]">Scopes: {client.allowed_scopes.join(", ")}</p>
      </div>
      {/* Read-only and revoked records must never offer the revoke action. */}
      {canManage && !revoked && (
        <button type="button" onClick={() => onRevoke(client)} disabled={saving} className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded-lg border border-slate-300 px-3 text-sm text-slate-950 hover:bg-slate-50 disabled:opacity-50 sm:shrink-0">
          <Ban className="w-4 h-4" /> Revoke
        </button>
      )}
    </div>
  );
}

/** Splits a textarea's newline-separated entries into a trimmed, non-empty list. */
function splitLines(value: string): string[] {
  return value.split("\n").map((line) => line.trim()).filter(Boolean);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}
