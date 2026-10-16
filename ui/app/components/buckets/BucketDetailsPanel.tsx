import { useEffect, useRef, useState, type KeyboardEvent } from "react";
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
import { useSearchParams } from "@remix-run/react";
import { ConnectReturnStatus } from "~/components/buckets/BucketConnectAccount";
import { BucketAddDropdown } from "~/components/buckets/BucketAddDropdown";
import { BucketConnectedUsersTable } from "~/components/buckets/BucketConnectedUsersTable";
import { FocusedBucketSecret } from "~/components/buckets/FocusedBucketSecret";
import { BucketEntryComposer } from "~/components/buckets/BucketEntryComposer";
import { BucketEntryList } from "~/components/buckets/BucketEntryList";
import { BucketOverview } from "~/components/buckets/BucketOverview";
import {
  type BucketDetailTab,
  type BucketSection,
  type BucketEntryKind,
  type BucketSecretFormPayload,
  type CredentialFamilyFormPayload,
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
  section: BucketSection;
  onSectionChange: (section: BucketSection) => void;
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
  onSaveCredentialFamily: (payload: CredentialFamilyFormPayload) => Promise<void>;
  onSaveValue: (payload: ValueFormPayload) => Promise<void>;
  onSaveBucketSecret: (payload: BucketSecretFormPayload) => Promise<void>;
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
  revealCredentials: boolean;
  manageValues: boolean;
  manageConnections: boolean;
};

type SelectedBucketDetailsProps = BucketDetailsPanelProps & { bucket: BucketSummary };

/** Resets drawer navigation and form focus when a different bucket is opened. */
export function BucketDetailsPanel(props: BucketDetailsPanelProps) {
  // No credential controls may render without an explicit bucket target.
  if (!props.bucket) return null;
  return <BucketDetailsContent key={props.bucket.id} {...props} bucket={props.bucket} />;
}

/** Keeps resource usage separate from credential editing, with actions and navigation always visible. */
function BucketDetailsContent(props: SelectedBucketDetailsProps) {
  const [params] = useSearchParams();
  const { section, onSectionChange } = props;
  const [focusRequest, setFocusRequest] = useState(0);
  const scrollRef = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLDivElement>(null);
  const focusedSecret = params.get("secret");

  // A new secret deep link must reveal its editor even if a usage tab was open.
  useEffect(() => {
    if (focusedSecret) onSectionChange("credentials");
  }, [focusedSecret]);

  // Each section starts in view rather than inheriting another section's long scroll position.
  useEffect(() => {
    if (scrollRef.current) scrollRef.current.scrollTop = 0;
  }, [section, focusedSecret]);

  // Repeated Add clicks also reveal the existing draft without discarding its values.
  useEffect(() => {
    if (!props.entryKind || section !== "credentials") return;
    if (scrollRef.current) scrollRef.current.scrollTop = 0;
    composerRef.current?.querySelector<HTMLElement>("[data-bucket-entry-composer] :is(input, select, button):not([disabled])")?.focus({ preventScroll: true });
  }, [props.entryKind, focusRequest]);

  /** Routes every creation action to its visible editor while retaining route-owned save state. */
  function addEntry(kind: BucketEntryKind) {
    onSectionChange("credentials");
    setFocusRequest(/** Give repeated clicks a new focus request even when the kind is unchanged. */ (request) => request + 1);
    props.onAddEntry(kind);
  }

  return (
    <>
      <div className="fixed inset-0 z-40 bg-slate-900/20 transition-opacity" onClick={props.onClose} />
      <aside data-fused-detail-sidebar className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-hidden border-l border-slate-200 bg-white shadow-2xl transition-transform md:w-[calc(100%-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
        <BucketDetailsHeader {...props} onAddEntry={addEntry} />
        <BucketSectionTabs section={section} onChange={onSectionChange} permissions={props.permissions} />
        {/* Only the selected section scrolls; the header and main tabs stay within reach. */}
        <div ref={scrollRef} role="region" aria-label="Bucket contents" tabIndex={0} className="min-h-0 flex-1 overflow-y-auto overscroll-contain focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-slate-400">
          <div className="px-6"><ConnectReturnStatus result={params.get("fused_connect")} /></div>
          {/* Hiding preserves unsaved credentials while users inspect related apps or services. */}
          <div role="tabpanel" id="bucket-panel-credentials" aria-labelledby="bucket-section-credentials" hidden={section !== "credentials"}>
            <div ref={composerRef}><BucketCredentialsContent {...props} /></div>
          </div>
          {/* Usage lists keep their existing read permissions and never displace the editor. */}
          {section !== "credentials" && <div role="tabpanel" id={`bucket-panel-${section}`} aria-labelledby={`bucket-section-${section}`}>
            {props.loading ? <p role="status" className="px-6 py-10 text-sm text-slate-500">Loading bucket usage…</p> : <BucketOverview
              bucket={props.bucket}
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
              showApps={section === "apps" && props.permissions.readApps}
              showServices={section === "services" && props.permissions.readServices}
            />}
          </div>}
        </div>
      </aside>
    </>
  );
}

/** Provides keyboard-accessible navigation between editing and the two usage lists. */
function BucketSectionTabs({ section, onChange, permissions }: {
  section: BucketSection;
  onChange: (section: BucketSection) => void;
  permissions: BucketDetailsPermissions;
}) {
  const tabs: Array<{ key: BucketSection; label: string }> = [
    { key: "credentials", label: "Credentials" },
    // Usage tabs must not expose lists that the caller cannot read.
    ...(permissions.readApps ? [{ key: "apps" as const, label: "Apps" }] : []),
    ...(permissions.readServices ? [{ key: "services" as const, label: "Services" }] : []),
  ];
  /** Matches standard tab keyboard behavior, including wraparound and first/last shortcuts. */
  function navigate(event: KeyboardEvent<HTMLDivElement>) {
    const current = tabs.findIndex(/** Match the currently selected tab before moving focus. */ (tab) => tab.key === section);
    const destinations: Record<string, number> = { ArrowRight: (current + 1) % tabs.length, ArrowLeft: (current + tabs.length - 1) % tabs.length, Home: 0, End: tabs.length - 1 };
    // Unrelated keys retain their native behavior, including Tab to enter the panel.
    if (!(event.key in destinations)) return;
    event.preventDefault();
    const next = tabs[destinations[event.key]].key;
    onChange(next);
    event.currentTarget.querySelector<HTMLButtonElement>(`#bucket-section-${next}`)?.focus();
  }
  return <div role="tablist" aria-label="Bucket sections" onKeyDown={navigate} className="fused-tabs-scroll shrink-0 gap-1 border-b border-slate-200 bg-white px-4 sm:px-6">
    {tabs.map(/** Associate each tab with its panel for keyboard and screen-reader navigation. */ (tab) => <button
      key={tab.key} id={`bucket-section-${tab.key}`} role="tab" type="button"
      aria-selected={section === tab.key} aria-controls={`bucket-panel-${tab.key}`}
      // Only the active tab participates in the page's Tab sequence; arrow keys reach its siblings.
      tabIndex={section === tab.key ? 0 : -1}
      onClick={/** Select the destination without changing route-owned credential state. */ () => onChange(tab.key)}
      className={`min-h-11 whitespace-nowrap border-b-2 px-4 py-3 text-sm font-medium focus-visible:outline focus-visible:outline-2 focus-visible:outline-inset ${
        // The underline identifies selection without relying on color alone.
        section === tab.key ? "border-slate-900 text-slate-900" : "border-transparent text-slate-500 hover:text-slate-800"
      }`}
    >{tab.label}</button>)}
  </div>;
}

/** Keeps credential creation ahead of lists and retains the existing secret, value, and user controls. */
function BucketCredentialsContent(props: SelectedBucketDetailsProps) {
  const [params, setParams] = useSearchParams();
  /** Dismisses only the focused-secret link while retaining the selected bucket and credential tab. */
  function closeFocusedSecret() {
    const next = new URLSearchParams(params);
    next.delete("secret");
    setParams(next, { replace: true });
  }
  return <>
        {/* Entry writes remain gated by the existing credential/value management grants. */}
        {(props.permissions.manageCredentials || props.permissions.manageValues) && <div data-bucket-entry-composer><BucketEntryComposer
          kind={props.entryKind}
          saving={props.saving}
          services={props.services}
          onCancel={props.onCancelEntry}
          onSaveSecret={props.onSaveSecret}
          onSaveSecrets={props.onSaveSecrets}
          onSaveCredentialFamily={props.onSaveCredentialFamily}
          onSaveValue={props.onSaveValue}
          onSaveBucketSecret={props.onSaveBucketSecret}
        /></div>}
        {/* Deep links reveal only a named metadata entry; the existing credential permission controls writes. */}
        {params.get("secret") && props.permissions.readSecrets && <FocusedBucketSecret key={`${props.bucket.id}:${params.get("secret")}`} bucketID={props.bucket.id} name={params.get("secret")!} canManage={props.permissions.manageCredentials} saving={props.saving} onSave={props.onSaveBucketSecret} onClose={closeFocusedSecret} />}

        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-6 py-3">
          <span className="text-sm font-medium text-slate-700">
            Credentials and values
          </span>
          <BucketTabs
            activeTab={props.activeTab}
            secretCount={props.bucket.secret_count}
            valueCount={props.bucket.value_count}
            // Summary counts remain accurate before the Connected users tab has been loaded.
            connectedUserCount={
              props.connectSummary?.connected_user_count ??
              props.bucket.connected_user_count
            }
            onTabChange={props.onTabChange}
            permissions={props.permissions}
          />
        </div>
        {/* Connection usage remains visible below the editor rather than pushing creation off screen. */}
        {props.activeTab === "connected-users" && props.permissions.readConnections && <BucketConnectUsage summary={props.connectSummary} />}
        <BucketTabPanel {...props} />
  </>;
}

/** Keeps readable credential sections in one scrollable strip without shrinking their labels. */
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
    { key: "env", label: "Env", count: valueCount, visible: permissions.readValues },
    {
      key: "connected-users",
      label: "Connected users",
      count: connectedUserCount,
      visible: permissions.readConnections,
    },
  ] satisfies Array<{ key: BucketDetailTab; label: string; count: number; visible: boolean }>).filter((tab) => tab.visible);
  return (
    <div aria-label="Bucket sections" className="fused-tabs-scroll rounded-md border border-slate-200 bg-slate-50 p-0.5">
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
      services={props.services}
      values={props.values}
      total={
        props.activeTab === "secrets" ? props.secretTotal : props.valueTotal
      }
      page={props.activeTab === "secrets" ? props.secretPage : props.valuePage}
      pageSize={props.pageSize}
      onRemoveSecret={props.onRemoveSecret}
      canReveal={props.permissions.revealCredentials}
      onSecretPolicyChanged={/** Reload only metadata after a policy change. */ () => props.bucket && props.onRefresh(props.bucket.id)}
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

/** Keeps actions visible and anchors dismissal beside the title when mobile actions wrap. */
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
    <div className="relative z-10 flex shrink-0 flex-col items-stretch justify-between gap-3 border-b border-slate-100 bg-white/90 px-4 py-4 backdrop-blur sm:flex-row sm:items-center sm:px-6 sm:py-5">
      <div className="min-w-0 pr-10 sm:pr-0">
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
          aria-label="Refresh bucket"
          title="Refresh"
        >
          <RefreshCw className={`w-4 h-4 ${loading ? "animate-spin" : ""}`} />
        </button>
        {permissions.manageBucket && <button
          type="button"
          onClick={onDeleteBucket}
          disabled={!!deleteDisabledReason}
          className="inline-flex items-center gap-2 rounded-lg border border-slate-200 px-3 py-2 text-sm font-medium text-slate-600 hover:border-slate-400 hover:bg-slate-50 hover:text-slate-950 disabled:opacity-50 disabled:hover:border-slate-200 disabled:hover:bg-white disabled:hover:text-slate-600"
          title={deleteDisabledReason || "Remove bucket"}
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
              // Bucket secrets share the credential-management permission since
              // they are still stored as workspace secrets, just service-independent.
              ...(permissions.manageCredentials ? ["bucket_secret" as const] : []),
              ...(permissions.manageValues ? ["value" as const] : []),
            ]}
          />
        )}
        <button
          type="button"
          onClick={onClose}
          className="absolute right-3 top-3 inline-flex h-10 w-10 items-center justify-center rounded-full text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 sm:static"
          aria-label="Close credential details"
          title="Close"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}

/** Explains connected-user usage before a bucket can be removed. */
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
          {summary.application_credential_count} OAuth app
          {summary.application_credential_count === 1 ? "" : "s"}
        </span>
        <span className="inline-flex items-center gap-1.5 text-amber-800">
          <Users className="h-4 w-4" />
          {summary.connected_user_count} connected user
          {summary.connected_user_count === 1 ? "" : "s"}
        </span>
      </div>
      {summary.connected_user_count > 0 && (
        <p className="mt-1 text-xs text-amber-800">
          Deleting this bucket requires typing its name because connected users
          will be removed too.
        </p>
      )}
    </div>
  );
}

function hasConnectUsage(summary: BucketConnectSummary): boolean {
  // Application credentials and user grants are independent reasons to show connection state.
  return summary.application_credential_count > 0 || summary.connected_user_count > 0;
}

/** Protects the default bucket and explains why removal is unavailable. */
function bucketDeleteDisabledReason(
  bucket: BucketSummary,
  saving: boolean
): string {
  if (saving) return "Saving credential changes";
  // The runtime default must remain available to existing apps.
  if (bucket.is_default) return "The default bucket cannot be removed";
  return "";
}
