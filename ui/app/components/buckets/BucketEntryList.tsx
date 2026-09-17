import { Copy } from "lucide-react";
import { type BucketValue, type SecretMeta } from "~/lib/api";
import { formatExpiry } from "~/lib/buckets";
import { bucketEntryActions } from "~/lib/bucket-entry-actions";
import { BucketPagination } from "~/components/buckets/BucketPagination";
import { StoredSecretKeys } from "~/components/buckets/StoredSecretKeys";
import { BucketEntryMenu } from "~/components/buckets/BucketEntryMenu";

type BucketEntryListProps = {
  loading: boolean;
  kind: "secrets" | "env";
  secrets: SecretMeta[];
  values: BucketValue[];
  total: number;
  page: number;
  pageSize: number;
  onRemoveSecret: (item: SecretMeta) => void;
  onRemoveValue: (item: BucketValue) => void;
  onPageChange: (page: number) => void;
  canRemove?: boolean;
};

type EntryRowModel = {
  id: string;
  kind: "secret" | "value";
  name: string;
  detail: string;
  value: string;
  storedSecret?: Pick<SecretMeta, "key_name" | "key_names">;
  onRemove: () => void;
};

/** Renders one authorized page of secrets or values. */
export function BucketEntryList({
  loading,
  kind,
  secrets,
  values,
  total,
  page,
  pageSize,
  onRemoveSecret,
  onRemoveValue,
  onPageChange,
  canRemove = true,
}: BucketEntryListProps) {
  if (loading)
    return (
      <div className="px-5 py-10 text-center text-sm text-slate-400">
        Loading bucket entries...
      </div>
    );
  const entries = bucketEntries(
    kind,
    secrets,
    values,
    onRemoveSecret,
    onRemoveValue
  );
  if (entries.length === 0) {
    return (
      <div>
        <div className="px-5 py-10 text-center text-sm text-slate-400">
          {total > 0 ? "No entries on this page." : emptyLabel(kind)}
        </div>
        <BucketPagination
          total={total}
          page={page}
          pageSize={pageSize}
          onPageChange={onPageChange}
        />
      </div>
    );
  }

  return (
    <div>
      <div className="divide-y divide-slate-100">
        {entries.map((entry) => (
          <EntryRow key={entry.id} entry={entry} canRemove={canRemove} />
        ))}
      </div>
      <BucketPagination
        total={total}
        page={page}
        pageSize={pageSize}
        onPageChange={onPageChange}
      />
    </div>
  );
}

function bucketEntries(
  kind: BucketEntryListProps["kind"],
  secrets: SecretMeta[],
  values: BucketValue[],
  onRemoveSecret: (item: SecretMeta) => void,
  onRemoveValue: (item: BucketValue) => void
): EntryRowModel[] {
  if (kind === "secrets")
    return secrets.map((item) => secretEntry(item, onRemoveSecret));
  return values.map((item) => valueEntry(item, onRemoveValue));
}

/** Preserves the stored key metadata separately from the credential's friendly type label and masked value. */
function secretEntry(
  item: SecretMeta,
  onRemove: (item: SecretMeta) => void
): EntryRowModel {
  const expiry = formatExpiry(item.expires_at);
  return {
    id: `secret-${item.service_id}-${
      item.key_names?.join("-") || item.key_name
    }`,
    kind: "secret",
    name: credentialLabel(item.credential_type),
    detail:
      expiry === "Never"
        ? item.credential_type
        : `${item.credential_type} · expires ${expiry}`,
    value: "********",
    storedSecret: { key_name: item.key_name, key_names: item.key_names },
    onRemove: () => onRemove(item),
  };
}

function credentialLabel(value: string): string {
  const credentialType = value.toLowerCase().replaceAll("-", "_");
  if (credentialType === "api_key" || credentialType === "apikey")
    return "API key";
  if (credentialType === "basic") return "Basic credentials";
  if (["mtls", "mutualtls", "mutual_tls"].includes(credentialType))
    return "mTLS credentials";
  // OAuth/OIDC store an application client_id/client_secret pair, not a
  // single connected-user token, so the label must reflect that pairing.
  if (credentialType === "oauth" || credentialType === "oauth2")
    return "OAuth credentials";
  if (credentialType === "oidc" || credentialType === "openidconnect")
    return "OIDC credentials";
  if (credentialType === "bearer") return "Bearer token";
  // Bucket secrets are generic, service-independent values (Engine tags them
  // with this fixed credential_type rather than a per-auth-scheme one); shown
  // as plain "Secret" since it's the simpler/default case.
  if (credentialType === "bucket_secret") return "Secret";
  // Any other/unrecognized per-service auth type falls back to this label,
  // named "Service Auth" to distinguish it from the generic kind above.
  return "Service Auth";
}

function valueEntry(
  item: BucketValue,
  onRemove: (item: BucketValue) => void
): EntryRowModel {
  return {
    id: `value-${item.service_id}-${item.key_name}`,
    kind: "value",
    name: item.key_name,
    detail: item.location,
    value: item.value || "-",
    onRemove: () => onRemove(item),
  };
}

/** Shows masked secrets and separates authorized row options from the removal request. */
function EntryRow({ entry, canRemove }: { entry: EntryRowModel; canRemove: boolean }) {
  const actions = bucketEntryActions(entry.kind, entry.value, canRemove);
  return (
    <div className={entryRowGridClass(entry.kind)}>
      <div className="flex min-w-0 items-center gap-3">
        <span className="font-mono text-xs text-slate-400">
          {/* Keep secret and environment rows visually distinct without exposing values. */}
          {entry.kind === "secret" ? "{}" : "[]"}
        </span>
        <div className="min-w-0">
          <p className="truncate font-mono text-sm font-medium text-slate-800">
            {entry.name}
          </p>
          <p className="truncate text-xs text-slate-500">{entry.detail}</p>
          {/* Only secret rows need separate storage metadata; environment rows already use their key as the title. */}
          {entry.storedSecret && <StoredSecretKeys secret={entry.storedSecret} />}
        </div>
      </div>
      <EntryValue entry={entry} />
      <div className="flex items-center gap-1">
        {/* Only visible environment values may be copied; secret placeholders are not credentials. */}
        {actions.canCopy && (
          <button
            type="button"
            onClick={() => copyEntryValue(entry.value)}
            className="rounded-md p-1.5 text-slate-400 hover:bg-slate-50 hover:text-slate-700"
            aria-label={`Copy ${entry.name}`}
            title="Copy value"
          >
            <Copy className="h-4 w-4" />
          </button>
        )}
        {/* Opening options must never perform the destructive route action. */}
        {actions.canRemove && <BucketEntryMenu name={entry.name} onRemove={entry.onRemove} />}
      </div>
    </div>
  );
}

/**
 * Secret rows only ever render a fixed masked placeholder ("********"), so
 * they don't need the wide value column env rows require for real, possibly
 * long values and their hover tooltip. Shrinking that column to fit-content
 * on secret rows leaves the name/label column room to breathe instead of
 * wrapping/truncating on narrow (mobile) viewports.
 */
function entryRowGridClass(kind: EntryRowModel["kind"]): string {
  if (kind === "secret") {
    return "grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-3 px-4 py-3";
  }
  return "grid grid-cols-[minmax(0,0.9fr)_minmax(220px,1.4fr)_auto] items-center gap-3 px-4 py-3";
}

function EntryValue({ entry }: { entry: EntryRowModel }) {
  if (entry.kind !== "value") {
    return (
      <p className="min-w-0 truncate text-right font-mono text-sm text-slate-500">
        {entry.value}
      </p>
    );
  }

  return (
    <div className="group/value relative min-w-0" tabIndex={0}>
      <p className="truncate text-right font-mono text-sm text-slate-500">
        {entry.value}
      </p>
      <div
        role="tooltip"
        className="pointer-events-none absolute right-0 top-full z-30 mt-2 hidden max-w-[min(36rem,calc(100vw-2rem))] rounded-md border border-slate-200 bg-white px-3 py-2 text-left font-mono text-xs leading-relaxed text-slate-700 shadow-lg group-hover/value:block group-focus/value:block"
      >
        <span className="block max-h-52 overflow-auto whitespace-pre-wrap break-all">
          {entry.value}
        </span>
      </div>
    </div>
  );
}

function copyEntryValue(value: string) {
  if (value === "-" || typeof navigator === "undefined") return;
  navigator.clipboard?.writeText(value);
}

function emptyLabel(kind: BucketEntryListProps["kind"]): string {
  return kind === "secrets"
    ? "No secrets in this credential set."
    : "No values in this credential set.";
}
