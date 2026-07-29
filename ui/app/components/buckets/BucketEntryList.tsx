import { Copy, MoreVertical } from "lucide-react";
import { type BucketValue, type SecretMeta } from "~/lib/api";
import { formatExpiry } from "~/lib/buckets";
import { BucketPagination } from "~/components/buckets/BucketPagination";

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
};

type EntryRowModel = {
  id: string;
  kind: "secret" | "value";
  name: string;
  detail: string;
  value: string;
  onRemove: () => void;
};

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
          <EntryRow key={entry.id} entry={entry} />
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
  if (credentialType === "oauth" || credentialType === "oauth2")
    return "OAuth token";
  if (credentialType === "oidc" || credentialType === "openidconnect")
    return "OIDC token";
  if (credentialType === "bearer") return "Bearer token";
  return "Secret";
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

function EntryRow({ entry }: { entry: EntryRowModel }) {
  const canCopy = entry.kind === "value" && entry.value !== "-";
  return (
    <div className="grid grid-cols-[minmax(0,0.9fr)_minmax(220px,1.4fr)_auto_auto] items-center gap-3 px-4 py-3">
      <div className="flex min-w-0 items-center gap-3">
        <span className="font-mono text-xs text-slate-400">
          {entry.kind === "secret" ? "{}" : "[]"}
        </span>
        <div className="min-w-0">
          <p className="truncate font-mono text-sm font-medium text-slate-800">
            {entry.name}
          </p>
          <p className="truncate text-xs text-slate-500">{entry.detail}</p>
        </div>
      </div>
      <EntryValue entry={entry} />
      <button
        type="button"
        onClick={() => copyEntryValue(entry.value)}
        disabled={!canCopy}
        className="rounded-md p-1.5 text-slate-400 hover:bg-slate-50 hover:text-slate-700 disabled:cursor-not-allowed disabled:opacity-30"
        aria-label={`Copy ${entry.name}`}
        title={canCopy ? "Copy value" : "Copy unavailable"}
      >
        <Copy className="h-4 w-4" />
      </button>
      <button
        type="button"
        onClick={entry.onRemove}
        className="rounded-md p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600"
        aria-label={`Remove ${entry.name}`}
        title="Remove"
      >
        <MoreVertical className="h-4 w-4" />
      </button>
    </div>
  );
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
    ? "No secrets in this bucket."
    : "No env values in this bucket.";
}
