import { BucketSecretAccess } from "~/components/buckets/BucketSecretAccess";
import { Copy } from "lucide-react";
import { type ActivatedService, type BucketValue, type SecretMeta } from "~/lib/api";
import { bucketSecretIdentity } from "~/lib/bucket-service-identity";
import { formatExpiry } from "~/lib/buckets";
import { bucketEntryActions } from "~/lib/bucket-entry-actions";
import { BucketPagination } from "~/components/buckets/BucketPagination";
import { StoredSecretKeys } from "~/components/buckets/StoredSecretKeys";
import { BucketEntryMenu } from "~/components/buckets/BucketEntryMenu";
import { bucketSecretEditTarget } from "~/components/buckets/BucketEntryComposerModel";

type BucketEntryListProps = {
  loading: boolean;
  kind: "secrets" | "env";
  secrets: SecretMeta[];
  services?: ActivatedService[];
  values: BucketValue[];
  total: number;
  page: number;
  pageSize: number;
  onEditSecret?: (item: SecretMeta) => void;
  onEditValue?: (item: BucketValue) => void;
  onRemoveSecret: (item: SecretMeta) => void;
  onRemoveValue: (item: BucketValue) => void;
  onPageChange: (page: number) => void;
  canRemove?: boolean;
  canReveal?: boolean;
  onSecretPolicyChanged?: () => void;
};

type EntryRowModel = {
  id: string;
  kind: "secret" | "value";
  name: string;
  detail: string;
  authScheme?: string;
  value: string;
  storedSecret?: SecretMeta;
  onEdit?: () => void;
  onRemove: () => void;
};

/** Renders one authorized page of secrets or values. */
export function BucketEntryList({
  loading,
  kind,
  secrets,
  services = [],
  values,
  total,
  page,
  pageSize,
  onEditSecret,
  onEditValue,
  onRemoveSecret,
  onRemoveValue,
  onPageChange,
  canRemove = true,
  canReveal = false,
  onSecretPolicyChanged,
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
    services,
    onEditSecret,
    onEditValue,
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
          <EntryRow key={entry.id} entry={entry} canRemove={canRemove} canReveal={canReveal} onSecretPolicyChanged={onSecretPolicyChanged} />
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

/** Keeps presentation metadata separate from the original update and removal targets. */
function bucketEntries(
  kind: BucketEntryListProps["kind"],
  secrets: SecretMeta[],
  values: BucketValue[],
  services: ActivatedService[],
  onEditSecret: ((item: SecretMeta) => void) | undefined,
  onEditValue: ((item: BucketValue) => void) | undefined,
  onRemoveSecret: (item: SecretMeta) => void,
  onRemoveValue: (item: BucketValue) => void
): EntryRowModel[] {
  // Only credential rows need service and auth-scheme identity.
  if (kind === "secrets")
    return secrets.map((item) => secretEntry(item, services, onEditSecret, onRemoveSecret));
  return values.map((item) => valueEntry(item, onEditValue, onRemoveValue));
}

/** Preserves the stored key metadata separately from the credential's friendly type label and masked value. */
function secretEntry(
  item: SecretMeta,
  services: ActivatedService[],
  onEdit: ((item: SecretMeta) => void) | undefined,
  onRemove: (item: SecretMeta) => void
): EntryRowModel {
  const expiry = formatExpiry(item.expires_at);
  const identity = bucketSecretIdentity(item, services);
  // Expiry supplements the service slug without crowding the separate auth-scheme tag.
  const detail = [identity.detail, expiry !== "Never" ? `expires ${expiry}` : ""].filter(Boolean).join(" · ");
  // Only offer inline update when the secret's service/auth metadata can be matched to an upsert target.
  const canEdit = Boolean(onEdit && bucketSecretEditTarget(item, services));
  return {
    id: `secret-${item.service_id}-${
      item.key_names?.join("-") || item.key_name
    }`,
    kind: "secret",
    name: identity.name,
    detail,
    authScheme: identity.authScheme,
    value: "********",
    storedSecret: item,
    onEdit: canEdit ? () => onEdit!(item) : undefined,
    onRemove: () => onRemove(item),
  };
}


/** Maps a non-secret bucket value to its row presentation and edit/removal handlers. */
function valueEntry(
  item: BucketValue,
  onEdit: ((item: BucketValue) => void) | undefined,
  onRemove: (item: BucketValue) => void
): EntryRowModel {
  return {
    id: `value-${item.service_id}-${item.key_name}`,
    kind: "value",
    name: item.key_name,
    detail: item.location,
    value: item.value || "-",
    onEdit: onEdit ? () => onEdit(item) : undefined,
    onRemove: () => onRemove(item),
  };
}

/** Shows masked secrets and separates authorized row options from the removal request. */
function EntryRow({ entry, canRemove, canReveal, onSecretPolicyChanged }: { entry: EntryRowModel; canRemove: boolean; canReveal: boolean; onSecretPolicyChanged?: () => void }) {
  const actions = bucketEntryActions(entry.kind, entry.value, canRemove);
  return (
    <div className={entryRowGridClass(entry.kind)}>
      <div className="flex min-w-0 items-center gap-3">
        <span className="font-mono text-xs text-slate-400">
          {/* Keep secret and environment rows visually distinct without exposing values. */}
          {entry.kind === "secret" ? "{}" : "[]"}
        </span>
        <div className="min-w-0">
          <p title={entry.name} className="break-words text-sm font-medium text-slate-800">
            {entry.name}
          </p>
          {/* Keep the service/expiry subtitle beside a distinct auth-scheme tag instead of inline prose. */}
          {(entry.detail || entry.authScheme) && (
            <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-slate-500">
              {entry.detail && <span className="break-words">{entry.detail}</span>}
              {entry.authScheme && (
                <span className="inline-flex items-center rounded-md border border-slate-200 bg-slate-50 px-1.5 py-0.5 font-mono text-[11px] font-medium text-slate-600">
                  {entry.authScheme}
                </span>
              )}
            </div>
          )}
          {/* Only secret rows need separate storage metadata; environment rows already use their key as the title. */}
          {entry.storedSecret && <StoredSecretKeys secret={entry.storedSecret} />}
        </div>
      </div>
      {/* Eligible secrets expose policy and retrieval only through an explicit, permission-gated dialog. */}
      {entry.storedSecret ? <BucketSecretAccess key={`${entry.storedSecret.id}-${canRemove}-${canReveal}-${entry.storedSecret.exportable}`} secret={entry.storedSecret} canManage={canRemove} canReveal={canReveal} onChanged={onSecretPolicyChanged} /> : <EntryValue entry={entry} />}
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
        {actions.canRemove && <BucketEntryMenu name={entry.name} onEdit={entry.onEdit} onRemove={entry.onRemove} />}
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

/** Keeps empty-state wording specific to secrets or values in the selected bucket. */
function emptyLabel(kind: BucketEntryListProps["kind"]): string {
  return kind === "secrets"
    ? "No secrets in this bucket."
    : "No values in this bucket.";
}
