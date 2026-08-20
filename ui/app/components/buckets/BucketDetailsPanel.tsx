import { RefreshCw, ShieldCheck, Trash2, Users, X } from "lucide-react";
import {
  type ActivatedService,
  type AuthConnection,
  type BucketConnectSummary,
  type BucketSDKSummary,
  type BucketServiceSummary,
  type BucketSummary,
  type BucketValue,
  type SecretMeta,
} from "~/lib/api";
import { BucketAddDropdown } from "~/components/buckets/BucketAddDropdown";
import { BucketConnectedUsersTable } from "~/components/buckets/BucketConnectedUsersTable";
import { BucketEntryComposer } from "~/components/buckets/BucketEntryComposer";
import { BucketEntryList } from "~/components/buckets/BucketEntryList";
import { BucketOverview } from "~/components/buckets/BucketOverview";
import {
  type BucketDetailTab,
  type BucketEntryKind,
  type SecretFormPayload,
  type ValueFormPayload,
} from "~/lib/buckets";

type BucketDetailsPanelProps = {
  bucket?: BucketSummary;
  values: BucketValue[];
  valueTotal: number;
  secrets: SecretMeta[];
  secretTotal: number;
  connections: AuthConnection[];
  connectionTotal: number;
  bucketSDKs: BucketSDKSummary[];
  bucketSDKTotal: number;
  bucketServices: BucketServiceSummary[];
  bucketServiceTotal: number;
  connectionServices: BucketServiceSummary[];
  connectSummary: BucketConnectSummary | null;
  services: ActivatedService[];
  activeTab: BucketDetailTab;
  loading: boolean;
  saving: boolean;
  deletingConnectionId: string | null;
  entryKind: BucketEntryKind | null;
  onClose: () => void;
  onRefresh: (bucketId: string) => void;
  onDeleteBucket: () => void;
  onTabChange: (tab: BucketDetailTab) => void;
  onAddEntry: (kind: BucketEntryKind) => void;
  onCancelEntry: () => void;
  onSaveSecret: (payload: SecretFormPayload) => Promise<void>;
  onSaveSecrets: (payloads: SecretFormPayload[]) => Promise<void>;
  onSaveValue: (payload: ValueFormPayload) => Promise<void>;
  onRemoveSecret: (item: SecretMeta) => void;
  onRemoveValue: (item: BucketValue) => void;
  onRemoveConnection: (connection: AuthConnection) => void;
  pageSize: number;
  overviewPageSize: number;
  secretPage: number;
  valuePage: number;
  connectionPage: number;
  sdkPage: number;
  servicePage: number;
  serviceSearch: string;
  connectionServiceSearch: string;
  connectedServiceFilter: string;
  onSecretPageChange: (page: number) => void;
  onValuePageChange: (page: number) => void;
  onConnectionPageChange: (page: number) => void;
  onSDKPageChange: (page: number) => void;
  onServicePageChange: (page: number) => void;
  onServiceSearchChange: (search: string) => void;
  onConnectionServiceSearchChange: (search: string) => void;
  onConnectedServiceFilterChange: (serviceId: string) => void;
  permissions: BucketDetailsPermissions;
};

type BucketDetailsPermissions = {
  readSecrets: boolean;
  readValues: boolean;
  readConnections: boolean;
  readApps: boolean;
  readServices: boolean;
  manageBucket: boolean;
  manageCredentials: boolean;
  manageValues: boolean;
  manageConnections: boolean;
};

/** Renders a credential-set drawer with permission-aware sections and actions. */
export function BucketDetailsPanel(props: BucketDetailsPanelProps) {
  if (!props.bucket) return null;

  return (
    <>
      <div
        className="fixed inset-0 z-40 bg-slate-900/20 transition-opacity"
        onClick={props.onClose}
      />
      <aside className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white shadow-2xl transition-transform md:w-[calc(100vw-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
        <BucketDetailsHeader
          bucket={props.bucket}
          loading={props.loading}
          saving={props.saving}
          onClose={props.onClose}
          onRefresh={props.onRefresh}
          onDeleteBucket={props.onDeleteBucket}
          onAddEntry={props.onAddEntry}
          permissions={props.permissions}
        />
        {props.permissions.readConnections && <BucketConnectUsage summary={props.connectSummary} />}
        {(props.permissions.readApps || props.permissions.readServices) && <BucketOverview
          sdks={props.bucketSDKs}
          sdkTotal={props.bucketSDKTotal}
          services={props.bucketServices}
          workspaceServices={props.services}
          serviceTotal={props.bucketServiceTotal}
          sdkPage={props.sdkPage}
          servicePage={props.servicePage}
          serviceSearch={props.serviceSearch}
          pageSize={props.overviewPageSize}
          onSDKPageChange={props.onSDKPageChange}
          onServicePageChange={props.onServicePageChange}
          onServiceSearchChange={props.onServiceSearchChange}
          showApps={props.permissions.readApps}
          showServices={props.permissions.readServices}
        />}
        {(props.permissions.manageCredentials || props.permissions.manageValues) && <BucketEntryComposer
          kind={props.entryKind}
          saving={props.saving}
          services={props.services}
          onCancel={props.onCancelEntry}
          onSaveSecret={props.onSaveSecret}
          onSaveSecrets={props.onSaveSecrets}
          onSaveValue={props.onSaveValue}
        />}
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-6 py-3">
          <span className="text-sm font-medium text-slate-700">
            Credentials and values
          </span>
          <BucketTabs
            activeTab={props.activeTab}
            secretCount={props.bucket.secret_count}
            valueCount={props.bucket.value_count}
            connectedUserCount={
              props.connectSummary?.connected_user_count ||
              props.connections.length
            }
            onTabChange={props.onTabChange}
            permissions={props.permissions}
          />
        </div>
        <div className="flex-1 overflow-y-auto">
          <BucketTabPanel {...props} />
        </div>
      </aside>
    </>
  );
}

/** Builds the tab set from readable credential sections. */
function BucketTabs({
  activeTab,
  secretCount,
  valueCount,
  connectedUserCount,
  onTabChange,
  permissions,
}: {
  activeTab: BucketDetailTab;
  secretCount: number;
  valueCount: number;
  connectedUserCount: number;
  onTabChange: (tab: BucketDetailTab) => void;
  permissions: BucketDetailsPermissions;
}) {
  const tabs: Array<{ key: BucketDetailTab; label: string; count: number; visible: boolean }> = ([
    { key: "secrets", label: "Secrets", count: secretCount, visible: permissions.readSecrets },
    { key: "env", label: "Values", count: valueCount, visible: permissions.readValues },
    {
      key: "connected-users",
      label: "Connected users",
      count: connectedUserCount,
      visible: permissions.readConnections,
    },
  ] satisfies Array<{ key: BucketDetailTab; label: string; count: number; visible: boolean }>).filter((tab) => tab.visible);
  return (
    <div className="flex rounded-md border border-slate-200 bg-slate-50 p-0.5">
      {tabs.map((tab) => (
        <button
          key={tab.key}
          type="button"
          onClick={() => onTabChange(tab.key)}
          className={`rounded px-3 py-1.5 text-xs font-medium transition-colors ${
            activeTab === tab.key
              ? "bg-white text-slate-900 shadow-sm"
              : "text-slate-500 hover:text-slate-800"
          }`}
        >
          {tab.label} ({tab.count})
        </button>
      ))}
    </div>
  );
}

/** Renders the selected detail section or an explicit access notice. */
function BucketTabPanel(props: BucketDetailsPanelProps) {
  if (!canReadActiveTab(props.activeTab, props.permissions)) {
    return <p className="px-6 py-10 text-sm text-slate-500">You do not have access to this credential section.</p>;
  }
  if (props.activeTab === "connected-users") {
    return (
      <BucketConnectedUsersTable
        loading={props.loading}
        connections={props.connections}
        services={props.connectionServices}
        workspaceServices={props.services}
        total={props.connectionTotal}
        page={props.connectionPage}
        pageSize={props.pageSize}
        serviceSearch={props.connectionServiceSearch}
        serviceFilter={props.connectedServiceFilter}
        deletingConnectionId={props.deletingConnectionId}
        onPageChange={props.onConnectionPageChange}
        onServiceSearchChange={props.onConnectionServiceSearchChange}
        onServiceFilterChange={props.onConnectedServiceFilterChange}
        onRemoveConnection={props.onRemoveConnection}
        canManage={props.permissions.manageConnections}
      />
    );
  }
  return (
    <BucketEntryList
      loading={props.loading}
      kind={props.activeTab}
      secrets={props.secrets}
      values={props.values}
      total={
        props.activeTab === "secrets" ? props.secretTotal : props.valueTotal
      }
      page={props.activeTab === "secrets" ? props.secretPage : props.valuePage}
      pageSize={props.pageSize}
      onRemoveSecret={props.onRemoveSecret}
      onRemoveValue={props.onRemoveValue}
      onPageChange={
        props.activeTab === "secrets"
          ? props.onSecretPageChange
          : props.onValuePageChange
      }
      canRemove={
        props.activeTab === "secrets"
          ? props.permissions.manageCredentials
          : props.permissions.manageValues
      }
    />
  );
}

/** Maps a selected tab to the permission required for its data. */
function canReadActiveTab(tab: BucketDetailTab, permissions: BucketDetailsPermissions): boolean {
  if (tab === "secrets") return permissions.readSecrets;
  if (tab === "env") return permissions.readValues;
  return permissions.readConnections;
}

type BucketDetailsHeaderProps = {
  bucket: BucketSummary;
  loading: boolean;
  saving: boolean;
  onClose: () => void;
  onRefresh: (bucketId: string) => void;
  onDeleteBucket: () => void;
  onAddEntry: (kind: BucketEntryKind) => void;
  permissions: BucketDetailsPermissions;
};

/** Renders safe refresh controls and authorized credential-set mutations. */
function BucketDetailsHeader({
  bucket,
  loading,
  saving,
  onClose,
  onRefresh,
  onDeleteBucket,
  onAddEntry,
  permissions,
}: BucketDetailsHeaderProps) {
  const deleteDisabledReason = bucketDeleteDisabledReason(bucket, saving);
  return (
    <div className="sticky top-0 z-10 flex flex-col items-stretch justify-between gap-3 border-b border-slate-100 bg-white/90 px-4 py-4 backdrop-blur sm:flex-row sm:items-center sm:px-6 sm:py-5">
      <div className="min-w-0">
        <div className="flex min-w-0 items-center gap-2">
          <h2 className="truncate text-lg font-semibold text-slate-900">
            {bucket.name}
          </h2>
          {bucket.is_default && (
            <span className="shrink-0 rounded-md bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600">
              Default
            </span>
          )}
        </div>
        <p className="mt-1 text-xs text-slate-500">
          {bucket.secret_count + bucket.value_count} entries ·{" "}
          <span className="font-mono">{bucket.id.slice(0, 8)}</span>
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-2 sm:shrink-0 sm:flex-nowrap">
        <button
          type="button"
          onClick={() => onRefresh(bucket.id)}
          className="p-2 rounded-lg border border-slate-200 text-slate-500 hover:bg-slate-50 hover:text-slate-700"
          aria-label="Refresh credential set"
          title="Refresh"
        >
          <RefreshCw className={`w-4 h-4 ${loading ? "animate-spin" : ""}`} />
        </button>
        {permissions.manageBucket && <button
          type="button"
          onClick={onDeleteBucket}
          disabled={!!deleteDisabledReason}
          className="inline-flex items-center gap-2 rounded-lg border border-slate-200 px-3 py-2 text-sm font-medium text-slate-600 hover:border-red-200 hover:bg-red-50 hover:text-red-600 disabled:opacity-50 disabled:hover:border-slate-200 disabled:hover:bg-white disabled:hover:text-slate-600"
          title={deleteDisabledReason || "Remove credential set"}
        >
          <Trash2 className="w-4 h-4" />
          Remove
        </button>}
        {(permissions.manageCredentials || permissions.manageValues) && (
          <BucketAddDropdown
            disabled={saving}
            onSelect={onAddEntry}
            allowedKinds={[
              ...(permissions.manageCredentials ? ["secret" as const] : []),
              ...(permissions.manageValues ? ["value" as const] : []),
            ]}
          />
        )}
        <button
          type="button"
          onClick={onClose}
          className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"
          aria-label="Close credential details"
          title="Close"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}

function BucketConnectUsage({
  summary,
}: {
  summary: BucketConnectSummary | null;
}) {
  if (!summary || !hasConnectUsage(summary)) return null;
  return (
    <div className="border-b border-amber-100 bg-amber-50 px-6 py-3">
      <div className="flex flex-wrap items-center gap-4 text-sm text-amber-900">
        <span className="inline-flex items-center gap-2 font-medium">
          <ShieldCheck className="h-4 w-4" />
          OAuth attached
        </span>
        <span className="inline-flex items-center gap-1.5 text-amber-800">
          <ShieldCheck className="h-4 w-4" />
          {summary.connect_config_count} config
          {summary.connect_config_count === 1 ? "" : "s"}
        </span>
        <span className="inline-flex items-center gap-1.5 text-amber-800">
          <Users className="h-4 w-4" />
          {summary.connected_user_count} connected user
          {summary.connected_user_count === 1 ? "" : "s"}
        </span>
      </div>
      {summary.connected_user_count > 0 && (
        <p className="mt-1 text-xs text-amber-800">
          Deleting this credential set requires typing its name because connected users
          will be removed too.
        </p>
      )}
    </div>
  );
}

function hasConnectUsage(summary: BucketConnectSummary): boolean {
  return summary.connect_config_count > 0 || summary.connected_user_count > 0;
}

function bucketDeleteDisabledReason(
  bucket: BucketSummary,
  saving: boolean
): string {
  if (saving) return "Saving credential changes";
  if (bucket.is_default) return "The default credential set cannot be removed";
  return "";
}
