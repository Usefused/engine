import { createBucketContentCache } from "~/lib/bucket-content-cache";
import { hasAnyAppPermission } from "~/lib/current-actor-access";
import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams, type MetaFunction } from "@remix-run/react";
import { BucketCreateModal } from "~/components/buckets/BucketCreateModal";
import { BucketDetailsPanel } from "~/components/buckets/BucketDetailsPanel";
import { BucketList } from "~/components/buckets/BucketList";
import { BucketPageHeader } from "~/components/buckets/BucketPageHeader";
import {
  api,
  type ActivatedService,
  type AuthConnection,
  type BucketConnectSummary,
  type BucketSDKSummary,
  type BucketServiceSummary,
  type BucketSummary,
  type BucketValue,
  type SecretMeta,
} from "~/lib/api";
import {
  bucketTabFromSearch,
  bucketTabSearchParams,
  bucketSearchParams,
  deleteBucketAuthConnection,
  errorMessage,
  preferredBucketID,
  readBucketContents,
  readBuckets,
  readBucketSummary,
  readWorkspaceServices,
  type BucketContentState,
  type BucketDetailTab,
  type BucketSection,
  type BucketContentSection,
  type BucketEntryKind,
  type BucketSecretFormPayload,
  type CredentialFamilyFormPayload,
  type SecretFormPayload,
  type ValueFormPayload,
} from "~/lib/buckets";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import {
  hasAnyPermission,
  hasResourcePermission,
  hasWorkspacePermission,
} from "~/lib/current-actor-access";
import { useToast } from "~/components/Toast";
import {
  consumeCredentialCreationRequest,
  requestsCredentialCreation,
} from "~/lib/credential-navigation";

/** Keeps the browser title aligned with the Bucket navigation and page heading. */
export const meta: MetaFunction = () => [{ title: "Bucket - Fused" }];

const BUCKET_PAGE_SIZE = 12;
const BUCKET_CONTENT_PAGE_SIZE = 10;
const BUCKET_OVERVIEW_PAGE_SIZE = 5;

/** Renders authorized buckets with a creation popover anchored to the page action. */
export default function BucketsPage() {
  const toast = useToast();
  const { access, loading: accessLoading } = useCurrentActorAccess();
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedBucketId = searchParams.get("bucket") || "";
  const [buckets, setBuckets] = useState<BucketSummary[]>([]);
  const [selectedBucketOverride, setSelectedBucketOverride] =
    useState<BucketSummary | null>(null);
  const [bucketTotal, setBucketTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [bucketValues, setBucketValues] = useState<BucketValue[]>([]);
  const [bucketValueTotal, setBucketValueTotal] = useState(0);
  const [secretMetas, setSecretMetas] = useState<SecretMeta[]>([]);
  const [secretMetaTotal, setSecretMetaTotal] = useState(0);
  const [authConnections, setAuthConnections] = useState<AuthConnection[]>([]);
  const [authConnectionTotal, setAuthConnectionTotal] = useState(0);
  const [bucketSDKs, setBucketSDKs] = useState<BucketSDKSummary[]>([]);
  const [bucketSDKTotal, setBucketSDKTotal] = useState(0);
  const [bucketServices, setBucketServices] = useState<BucketServiceSummary[]>(
    []
  );
  const [bucketServiceTotal, setBucketServiceTotal] = useState(0);
  const [connectionServices, setConnectionServices] = useState<
    BucketServiceSummary[]
  >([]);
  const [connectSummary, setConnectSummary] =
    useState<BucketConnectSummary | null>(null);
  const [services, setServices] = useState<ActivatedService[]>([]);
  const [secretPage, setSecretPage] = useState(0);
  const [valuePage, setValuePage] = useState(0);
  const [connectionPage, setConnectionPage] = useState(0);
  const [sdkPage, setSdkPage] = useState(0);
  const [servicePage, setServicePage] = useState(0);
  const [serviceSearch, setServiceSearch] = useState("");
  const [connectionServiceSearch, setConnectionServiceSearch] = useState("");
  const [connectedServiceFilter, setConnectedServiceFilter] = useState("");
  const [loadingBuckets, setLoadingBuckets] = useState(true);
  const [loadingContents, setLoadingContents] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deletingConnectionId, setDeletingConnectionId] = useState<
    string | null
  >(null);
  const [modalOpen, setModalOpen] = useState(false);
  const createAnchor = useRef<HTMLDivElement>(null);
  const [entryModalKind, setEntryModalKind] = useState<BucketEntryKind | null>(
    null
  );
  const contentRequestID = useRef(0);
  const [contentRevision, setContentRevision] = useState(0);
  const activeTab = bucketTabFromSearch(searchParams);
  const [sectionSelection, setSectionSelection] = useState<{ bucketId: string; section: BucketSection }>({ bucketId: "", section: "credentials" });
  // A different bucket starts with credentials without inheriting a previous drawer's usage tab.
  const section = sectionSelection.bucketId === selectedBucketId ? sectionSelection.section : "credentials";
  const contentSection: BucketContentSection = section === "credentials" ? activeTab : section;
  // Each authorization snapshot owns its cache, including plaintext environment values.
  const contentCache = useMemo(/** Access changes retire the previous cache and its pending reads. */ () => createBucketContentCache(readBucketContents), [access]);
  const serviceCache = useMemo(/** Service metadata is reused only within the same authorization snapshot. */ () => ({ pending: null as Promise<ActivatedService[]> | null }), [access]);
  /** Selects a usage view while keeping pagination and credential subtab state intact. */
  const selectSection = (next: BucketSection) => setSectionSelection({ bucketId: selectedBucketId, section: next });
  const contentAccess = useMemo(
    () => ({
      // The value field is additionally protected at workspace scope, so a
      // bucket-scoped grant alone cannot safely request plaintext values.
      values: hasWorkspacePermission(access, "bucket.values.read"),
      secrets: hasResourcePermission(
        access,
        "credentials.metadata.read",
        "BUCKET",
        selectedBucketId
      ),
      connections: hasResourcePermission(
        access,
        "connection.read",
        "BUCKET",
        selectedBucketId
      ),
      apps: hasAnyAppPermission(access, "read"),
      services: hasAnyPermission(access, "service.read"),
    }),
    [access, selectedBucketId]
  );
  const canManageBucket = hasResourcePermission(
    access,
    "bucket.manage",
    "BUCKET",
    selectedBucketId
  );
  const canCreateBucket = hasWorkspacePermission(access, "bucket.manage");
  const canManageCredentials = hasResourcePermission(
    access,
    "credentials.manage",
    "BUCKET",
    selectedBucketId
  );
  const canRevealCredentials = hasResourcePermission(access, "credentials.reveal", "BUCKET", selectedBucketId);
  const canManageConnections = hasResourcePermission(
    access,
    "connection.manage",
    "BUCKET",
    selectedBucketId
  );

  // Consumes one-shot credential creation navigation only with create access.
  useEffect(() => {
    if (!requestsCredentialCreation(searchParams)) return;
    if (canCreateBucket) setModalOpen(true);
    // Consume the command so refreshes do not reopen a modal the user closed.
    setSearchParams(consumeCredentialCreationRequest(searchParams), {
      replace: true,
    });
  }, [canCreateBucket, searchParams, setSearchParams]);

  const selectedBucket = useMemo(
    () =>
      buckets.find((bucket) => bucket.id === selectedBucketId) ||
      (selectedBucketOverride?.id === selectedBucketId
        ? selectedBucketOverride
        : undefined),
    [buckets, selectedBucketId, selectedBucketOverride]
  );
  const contentPages = useMemo(
    () => ({
      secrets: {
        limit: BUCKET_CONTENT_PAGE_SIZE,
        offset: secretPage * BUCKET_CONTENT_PAGE_SIZE,
      },
      values: {
        limit: BUCKET_CONTENT_PAGE_SIZE,
        offset: valuePage * BUCKET_CONTENT_PAGE_SIZE,
      },
      connections: {
        limit: BUCKET_CONTENT_PAGE_SIZE,
        offset: connectionPage * BUCKET_CONTENT_PAGE_SIZE,
        serviceId: connectedServiceFilter,
      },
      sdks: {
        limit: BUCKET_OVERVIEW_PAGE_SIZE,
        offset: sdkPage * BUCKET_OVERVIEW_PAGE_SIZE,
      },
      services: {
        limit: BUCKET_OVERVIEW_PAGE_SIZE,
        offset: servicePage * BUCKET_OVERVIEW_PAGE_SIZE,
        search: serviceSearch,
      },
      connectionServices: {
        limit: 20,
        offset: 0,
        search: connectionServiceSearch,
      },
    }),
    [
      connectedServiceFilter,
      connectionPage,
      connectionServiceSearch,
      sdkPage,
      secretPage,
      servicePage,
      serviceSearch,
      valuePage,
    ]
  );

  /** Opens a bucket without carrying an unfinished write into a different credential target. */
  const selectBucket = (bucketId: string) => {
    setEntryModalKind(null);
    // Bucket selection lives in the URL so SDK detail links and refreshes open
    // the same admin context without introducing another client-side store.
    setSelectedBucketOverride(null);
    contentRequestID.current++;
    setLoadingContents(false);
    resetBucketDetailPaging(
      setSecretPage,
      setValuePage,
      setConnectionPage,
      setSdkPage,
      setServicePage,
      setServiceSearch,
      setConnectionServiceSearch,
      setConnectedServiceFilter
    );
    setSearchParams(bucketSearchParams(searchParams, bucketId), {
      replace: true,
    });
  };

  /** Keeps credential subtab navigation bookmarkable without discarding cached sections. */
  const selectTab = (tab: BucketDetailTab) => {
    setSearchParams(bucketTabSearchParams(searchParams, tab), {
      replace: true,
    });
  };

  /** Reveals the matching credential tab while preserving cached usage lists. */
  const addEntry = (kind: BucketEntryKind) => {
    selectSection("credentials");
    // Bucket secrets render alongside service secrets in the "secrets" tab.
    selectTab(kind === "value" ? "env" : "secrets");
    setEntryModalKind(kind);
  };

  const clearSelectedBucket = () => {
    contentRequestID.current++;
    setLoadingContents(false);
    setEntryModalKind(null);
    setSearchParams(bucketSearchParams(searchParams, ""), { replace: true });
  };

  /** Refreshes the current bucket page while retaining pagination state. */
  const loadBucketList = () => {
    setLoadingBuckets(true);
    readBuckets(BUCKET_PAGE_SIZE, page * BUCKET_PAGE_SIZE)
      .then((state) => {
        if (
          rewindEmptyPage(
            state.bucketSummaries,
            state.total || 0,
            page,
            setPage
          )
        )
          return;
        applyBucketList(
          state.bucketSummaries,
          state.total || 0,
          setBuckets,
          setBucketTotal
        );
      })
      .catch((err) => toast.error(errorMessage(err, "Failed to load buckets")))
      .finally(() => setLoadingBuckets(false));
  };

  const contentSetters: BucketContentSetters = {
    setBucketValues, setBucketValueTotal, setSecretMetas, setSecretMetaTotal,
    setAuthConnections, setAuthConnectionTotal, setBucketSDKs, setBucketSDKTotal,
    setBucketServices, setBucketServiceTotal, setConnectionServices, setConnectSummary,
  };

  /** Reads just the selected view, reusing cached pages and ignoring superseded responses. */
  const loadContents = (bucketId: string) => {
    // Authorization must settle before admitting any cache lookup or network request.
    if (accessLoading) return;
    const requestID = ++contentRequestID.current;
    setLoadingContents(true);
    contentCache.read(bucketId, contentPages, contentAccess, contentSection)
      .then(/** A late response may populate its cache but must not replace the active view. */ (state) => {
        if (requestID !== contentRequestID.current) return;
        applyBucketContents(state, contentSetters);
      })
      .catch(/** Errors belong only to the bucket view still requesting the data. */ (err) => {
        if (requestID === contentRequestID.current) toast.error(errorMessage(err, "Failed to load bucket"));
      })
      .finally(/** An earlier request cannot clear a newer view's loading state. */ () => {
        if (requestID === contentRequestID.current) setLoadingContents(false);
      });
  };

  /** Explicit refresh and completed writes invalidate all affected counts and cached pages. */
  const refreshContents = (bucketId: string) => {
    contentCache.invalidate(bucketId);
    contentRequestID.current++;
    clearBucketContents(contentSetters);
    // The next render reloads the current view, even if navigation changed during the write.
    setContentRevision(/** Trigger a fresh read without capturing a stale tab in an async save callback. */ (revision) => revision + 1);
  };

  /** Loads the exact selected bucket independently of the visible list page. */
  const loadSelectedBucket = (bucketId: string) => {
    readBucketSummary(bucketId)
      .then(setSelectedBucketOverride)
      .catch((err) =>
        toast.error(errorMessage(err, "Failed to load selected bucket"))
      );
  };

  /** Refreshes summaries after a successful write and reloads only the current section. */
  const reloadBucketState = (bucketId: string) => {
    loadBucketList();
    loadSelectedBucket(bucketId);
    refreshContents(bucketId);
  };

  // Visible state cannot carry values between buckets or authorization snapshots.
  useEffect(/** Retires old responses before the selected view starts loading. */ () => {
    contentRequestID.current++;
    clearBucketContents(contentSetters);
  }, [selectedBucketId, access]);

  // Metadata from an earlier actor must not survive a permission refresh.
  useEffect(/** Clear the service snapshot while the new access-scoped cache loads on demand. */ () => { setServices([]); }, [access]);

  useEffect(() => {
    loadBucketList();
  }, [page]);

  // Full auth metadata is needed only for service interactions, not when opening the bucket list.
  useEffect(/** Lazily shares metadata between visited views and composers without repeating the request. */ () => {
    let current = true;
    // No service permission means neither a cached label nor a network request may be exposed.
    if (!contentAccess.services) { setServices([]); return; }
    // Generic secrets and metadata rows already have their own labels and need no service catalogue.
    if (!selectedBucketId || !(section === "services" || (section === "credentials" && (activeTab === "connected-users" || entryModalKind === "secret" || entryModalKind === "value")))) return;
    // A rejected lookup is removed so an explicit revisit can try again.
    serviceCache.pending ??= readWorkspaceServices().catch(/** Keep failures retryable without retaining a rejected promise. */ (error) => { serviceCache.pending = null; throw error; });
    serviceCache.pending.then(/** Navigation may abandon the view before its metadata returns. */ (items) => { if (current) setServices(items); })
      .catch(/** Surface errors only in the view that requested the service catalogue. */ (error) => { if (current) toast.error(errorMessage(error, "Failed to load workspace services")); });
    return /** Retire this subscription while retaining the shared request for later use. */ () => { current = false; };
  }, [selectedBucketId, section, activeTab, entryModalKind, contentAccess.services, serviceCache]);

  // Loads only the requested tab/page; revisits reuse the authorization-scoped cache.
  useEffect(() => {
    syncSelectedBucket(
      selectedBucket,
      selectedBucketId,
      selectBucket,
      loadContents,
      loadSelectedBucket,
      setBucketValues,
      setBucketValueTotal,
      setSecretMetas,
      setSecretMetaTotal,
      setAuthConnections,
      setAuthConnectionTotal,
      setBucketSDKs,
      setBucketSDKTotal,
      setBucketServices,
      setBucketServiceTotal,
      setConnectionServices,
      setConnectSummary
    );
  }, [selectedBucket?.id, selectedBucketId, contentPages, contentAccess, accessLoading, contentSection, contentCache, contentRevision]);

  useEffect(
    () =>
      rewindContentPage(
        secretMetas,
        secretMetaTotal,
        secretPage,
        setSecretPage
      ),
    [secretMetas, secretMetaTotal, secretPage]
  );
  useEffect(
    () =>
      rewindContentPage(
        bucketValues,
        bucketValueTotal,
        valuePage,
        setValuePage
      ),
    [bucketValues, bucketValueTotal, valuePage]
  );
  useEffect(
    () =>
      rewindContentPage(
        authConnections,
        authConnectionTotal,
        connectionPage,
        setConnectionPage
      ),
    [authConnections, authConnectionTotal, connectionPage]
  );
  useEffect(
    () => rewindContentPage(bucketSDKs, bucketSDKTotal, sdkPage, setSdkPage),
    [bucketSDKs, bucketSDKTotal, sdkPage]
  );
  useEffect(
    () =>
      rewindContentPage(
        bucketServices,
        bucketServiceTotal,
        servicePage,
        setServicePage
      ),
    [bucketServices, bucketServiceTotal, servicePage]
  );

  /** Selects a created bucket and continues into credential entry. */
  const onBucketCreated = async (name: string) => {
    setPage(0);
    const state = await readBuckets(BUCKET_PAGE_SIZE, 0);
    setBuckets(state.bucketSummaries);
    setBucketTotal(state.total || 0);
    selectBucket(
      state.bucketSummaries.find((bucket) => bucket.name === name)?.id ||
        preferredBucketID(state.bucketSummaries)
    );
    // A new set is only useful once it contains a provider credential, so
    // continue directly into the existing secret composer.
    setEntryModalKind("secret");
    toast.success("Bucket created");
  };

  /** Requires the selected bucket name before removal and refreshes its former page. */
  const deleteSelectedBucket = async () => {
    if (!selectedBucket || selectedBucket.is_default) return;
    const typedName = await toast.prompt(
      deleteBucketPrompt(selectedBucket, connectSummary),
      { placeholder: selectedBucket.name }
    );
    if (typedName !== selectedBucket.name) {
      // Cancellation is silent; a mismatched name leaves the bucket untouched.
      if (typedName !== null) toast.error("Bucket name did not match");
      return;
    }
    await withSaving(
      setSaving,
      async () => {
        await api.workspace.deleteBucket(selectedBucket.name);
        const state = await readBuckets(
          BUCKET_PAGE_SIZE,
          page * BUCKET_PAGE_SIZE
        );
        if (
          rewindEmptyPage(
            state.bucketSummaries,
            state.total || 0,
            page,
            setPage
          )
        )
          return;
        setBuckets(state.bucketSummaries);
        setBucketTotal(state.total || 0);
        clearSelectedBucket();
        toast.success("Bucket removed");
      },
      (err) => toast.error(errorMessage(err, "Failed to remove bucket"))
    );
  };

  const saveSecret = (payload: SecretFormPayload) =>
    saveBucketSecret(
      selectedBucket,
      payload,
      setSaving,
      reloadBucketState,
      toast
    );
  const saveSecrets = (payloads: SecretFormPayload[]) =>
    saveBucketSecrets(
      selectedBucket,
      payloads,
      setSaving,
      reloadBucketState,
      toast
    );
  const saveValue = (payload: ValueFormPayload) =>
    saveBucketValue(
      selectedBucket,
      payload,
      setSaving,
      reloadBucketState,
      toast
    );
  const saveCredentialFamily = (payload: CredentialFamilyFormPayload) =>
    saveBucketCredentialFamily(
      selectedBucket,
      payload,
      setSaving,
      reloadBucketState,
      toast
    );
  const saveGenericBucketSecret = (payload: BucketSecretFormPayload) =>
    saveBucketGenericSecret(
      selectedBucket,
      payload,
      setSaving,
      reloadBucketState,
      toast
    );
  const removeSecret = (item: SecretMeta) =>
    removeBucketSecret(item, setSaving, reloadBucketState, toast);
  const removeValue = (item: BucketValue) =>
    removeBucketValue(item, setSaving, reloadBucketState, toast);
  const removeConnection = (item: AuthConnection) =>
    removeBucketConnection(
      item,
      setDeletingConnectionId,
      reloadBucketState,
      toast
    );

  return (
    <div className="space-y-6">
      <BucketPageHeader canCreate={canCreateBucket} createAnchor={createAnchor} createOpen={modalOpen} onCreateClick={() => setModalOpen(true)} />
      <BucketList
        buckets={buckets}
        selectedBucketId={selectedBucket?.id || ""}
        loading={loadingBuckets}
        total={bucketTotal}
        page={page}
        pageSize={BUCKET_PAGE_SIZE}
        onRefresh={loadBucketList}
        onPageChange={setPage}
        onSelect={selectBucket}
      />
      <BucketDetailsPanel
        bucket={selectedBucket}
        values={bucketValues}
        valueTotal={bucketValueTotal}
        secrets={secretMetas}
        secretTotal={secretMetaTotal}
        connections={authConnections}
        connectionTotal={authConnectionTotal}
        bucketSDKs={bucketSDKs}
        bucketSDKTotal={bucketSDKTotal}
        bucketServices={bucketServices}
        bucketServiceTotal={bucketServiceTotal}
        connectionServices={connectionServices}
        connectSummary={connectSummary}
        services={services}
        section={section}
        onSectionChange={selectSection}
        activeTab={activeTab}
        loading={loadingContents}
        saving={saving}
        deletingConnectionId={deletingConnectionId}
        entryKind={entryModalKind}
        onClose={clearSelectedBucket}
        onRefresh={reloadBucketState}
        onDeleteBucket={deleteSelectedBucket}
        onTabChange={selectTab}
        onAddEntry={addEntry}
        onCancelEntry={() => setEntryModalKind(null)}
        onSaveSecret={saveSecret}
        onSaveSecrets={saveSecrets}
        onSaveCredentialFamily={saveCredentialFamily}
        onSaveValue={saveValue}
        onSaveBucketSecret={saveGenericBucketSecret}
        onRemoveSecret={removeSecret}
        onRemoveValue={removeValue}
        onRemoveConnection={removeConnection}
        pageSize={BUCKET_CONTENT_PAGE_SIZE}
        overviewPageSize={BUCKET_OVERVIEW_PAGE_SIZE}
        secretPage={secretPage}
        valuePage={valuePage}
        connectionPage={connectionPage}
        sdkPage={sdkPage}
        servicePage={servicePage}
        serviceSearch={serviceSearch}
        connectionServiceSearch={connectionServiceSearch}
        connectedServiceFilter={connectedServiceFilter}
        onSecretPageChange={setSecretPage}
        onValuePageChange={setValuePage}
        onConnectionPageChange={setConnectionPage}
        onSDKPageChange={setSdkPage}
        onServicePageChange={setServicePage}
        onServiceSearchChange={(search) => {
          setServiceSearch(search);
          setServicePage(0);
        }}
        onConnectionServiceSearchChange={setConnectionServiceSearch}
        onConnectedServiceFilterChange={(serviceId) => {
          setConnectedServiceFilter(serviceId);
          setConnectionPage(0);
        }}
        permissions={{
          readSecrets: contentAccess.secrets,
          readValues: contentAccess.values,
          readConnections: contentAccess.connections,
          readApps: contentAccess.apps,
          readServices: contentAccess.services,
          manageBucket: canManageBucket,
          manageCredentials: canManageCredentials,
          revealCredentials: canRevealCredentials,
          manageValues: contentAccess.values && canManageBucket,
          manageConnections: canManageConnections,
        }}
      />
      {canCreateBucket && <BucketCreateModal
        open={modalOpen}
        anchor={createAnchor}
        onClose={() => setModalOpen(false)}
        onCreated={onBucketCreated}
      />}
    </div>
  );
}

function rewindEmptyPage(
  nextBuckets: BucketSummary[],
  total: number,
  page: number,
  setPage: (page: number) => void
): boolean {
  if (nextBuckets.length > 0 || total === 0 || page === 0) return false;
  // Deletes can make the current page disappear; rewinding keeps pagination
  // anchored to rows that still exist instead of showing a false empty state.
  setPage(page - 1);
  return true;
}

function applyBucketList(
  nextBuckets: BucketSummary[],
  total: number,
  setBuckets: (buckets: BucketSummary[]) => void,
  setBucketTotal: (total: number) => void
) {
  setBuckets(nextBuckets);
  setBucketTotal(total);
}

type BucketContentSetters = {
  setBucketValues: (values: BucketValue[]) => void;
  setBucketValueTotal: (total: number) => void;
  setSecretMetas: (secrets: SecretMeta[]) => void;
  setSecretMetaTotal: (total: number) => void;
  setAuthConnections: (connections: AuthConnection[]) => void;
  setAuthConnectionTotal: (total: number) => void;
  setBucketSDKs: (sdks: BucketSDKSummary[]) => void;
  setBucketSDKTotal: (total: number) => void;
  setBucketServices: (services: BucketServiceSummary[]) => void;
  setBucketServiceTotal: (total: number) => void;
  setConnectionServices: (services: BucketServiceSummary[]) => void;
  setConnectSummary: (summary: BucketConnectSummary | null) => void;
};

/** Applies only returned sections so a newly visited tab cannot erase earlier data. */
function applyBucketContents(state: BucketContentState, setters: BucketContentSetters) {
  // Omitted fields represent sections not requested, rather than empty results.
  if (state.bucketValues !== undefined) {
    setters.setBucketValues(state.bucketValues);
    setters.setBucketValueTotal(totalOrLength(state.bucketValueTotal, state.bucketValues.length));
  }
  if (state.secretMetas !== undefined) {
    setters.setSecretMetas(state.secretMetas);
    setters.setSecretMetaTotal(totalOrLength(state.secretMetaTotal, state.secretMetas.length));
  }
  if (state.authConnections !== undefined) {
    setters.setAuthConnections(state.authConnections);
    setters.setAuthConnectionTotal(totalOrLength(state.authConnectionTotal, state.authConnections.length));
  }
  if (state.bucketSDKs !== undefined) {
    setters.setBucketSDKs(state.bucketSDKs);
    setters.setBucketSDKTotal(totalOrLength(state.bucketSDKTotal, state.bucketSDKs.length));
  }
  if (state.bucketServices !== undefined) {
    setters.setBucketServices(state.bucketServices);
    setters.setBucketServiceTotal(totalOrLength(state.bucketServiceTotal, state.bucketServices.length));
  }
  if (state.connectionServices !== undefined) setters.setConnectionServices(state.connectionServices);
  if (state.connectSummary !== undefined) setters.setConnectSummary(state.connectSummary);
}

/** Clears visible snapshots after navigation, authorization changes, or invalidation. */
function clearBucketContents(setters: BucketContentSetters) {
  applyBucketContents({ bucketValues: [], secretMetas: [], authConnections: [], bucketSDKs: [], bucketServices: [], connectionServices: [], connectSummary: null }, setters);
}

function totalOrLength(total: number | undefined, length: number): number {
  return total ?? length;
}

function syncSelectedBucket(
  selectedBucket: BucketSummary | undefined,
  selectedBucketId: string,
  selectBucket: (bucketId: string) => void,
  loadContents: (bucketId: string) => void,
  loadSelectedBucket: (bucketId: string) => void,
  setBucketValues: (values: BucketValue[]) => void,
  setBucketValueTotal: (total: number) => void,
  setSecretMetas: (secrets: SecretMeta[]) => void,
  setSecretMetaTotal: (total: number) => void,
  setAuthConnections: (connections: AuthConnection[]) => void,
  setAuthConnectionTotal: (total: number) => void,
  setBucketSDKs: (sdks: BucketSDKSummary[]) => void,
  setBucketSDKTotal: (total: number) => void,
  setBucketServices: (services: BucketServiceSummary[]) => void,
  setBucketServiceTotal: (total: number) => void,
  setConnectionServices: (services: BucketServiceSummary[]) => void,
  setConnectSummary: (summary: BucketConnectSummary | null) => void
) {
  if (!selectedBucket) {
    setBucketValues([]);
    setBucketValueTotal(0);
    setSecretMetas([]);
    setSecretMetaTotal(0);
    setAuthConnections([]);
    setAuthConnectionTotal(0);
    setBucketSDKs([]);
    setBucketSDKTotal(0);
    setBucketServices([]);
    setBucketServiceTotal(0);
    setConnectionServices([]);
    setConnectSummary(null);
    if (selectedBucketId) loadSelectedBucket(selectedBucketId);
    return;
  }
  if (selectedBucket.id !== selectedBucketId) {
    selectBucket(selectedBucket.id);
    return;
  }
  loadContents(selectedBucket.id);
}

function resetBucketDetailPaging(
  setSecretPage: (page: number) => void,
  setValuePage: (page: number) => void,
  setConnectionPage: (page: number) => void,
  setSdkPage: (page: number) => void,
  setServicePage: (page: number) => void,
  setServiceSearch: (search: string) => void,
  setConnectionServiceSearch: (search: string) => void,
  setConnectedServiceFilter: (serviceId: string) => void
) {
  // Bucket contents are scoped to the selected bucket; resetting prevents page
  // offsets and service filters from silently carrying into a different bucket.
  setSecretPage(0);
  setValuePage(0);
  setConnectionPage(0);
  setSdkPage(0);
  setServicePage(0);
  setServiceSearch("");
  setConnectionServiceSearch("");
  setConnectedServiceFilter("");
}

/** Includes connected-user impact in the explicit bucket removal confirmation. */
function deleteBucketPrompt(
  bucket: BucketSummary,
  summary: BucketConnectSummary | null
): string {
  // The bucket summary supplies the warning even before Connected users has been visited.
  const connectedUsers = summary?.connected_user_count ?? bucket.connected_user_count ?? 0;
  const usage =
    connectedUsers > 0
      ? ` This will also delete ${connectedUsers} connected user${
          connectedUsers === 1 ? "" : "s"
        }.`
      : "";
  return `Type "${bucket.name}" to delete this bucket.${usage}`;
}

async function saveBucketSecret(
  bucket: BucketSummary | undefined,
  payload: SecretFormPayload,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  if (!bucket) return;
  const err = await withSaving(
    setSaving,
    async () => {
      await api.workspace.upsertSecret({ bucketId: bucket.id, ...payload });
      loadContents(bucket.id);
      toast.success("Service Auth saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save service auth"))
  );
  if (err) throw err;
}

async function saveBucketSecrets(
  bucket: BucketSummary | undefined,
  payloads: SecretFormPayload[],
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  if (!bucket || payloads.length === 0) return;
  const err = await withSaving(
    setSaving,
    async () => {
      await api.workspace.upsertSecrets({
        bucketId: bucket.id,
        secrets: payloads,
      });
      loadContents(bucket.id);
      toast.success("Service Auth entries saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save service auth entries"))
  );
  if (err) throw err;
}

async function saveBucketValue(
  bucket: BucketSummary | undefined,
  payload: ValueFormPayload,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  if (!bucket) return;
  const err = await withSaving(
    setSaving,
    async () => {
      await api.workspace.upsertBucketValue({
        bucketId: bucket.id,
        ...payload,
      });
      loadContents(bucket.id);
      toast.success("Value saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save value"))
  );
  if (err) throw err;
}

// OAuth/OIDC application registrations go through Engine's semantic
// credential_family write path rather than raw secret rows.
async function saveBucketCredentialFamily(
  bucket: BucketSummary | undefined,
  payload: CredentialFamilyFormPayload,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  if (!bucket) return;
  const err = await withSaving(
    setSaving,
    async () => {
      await api.workspace.upsertCredentialFamily({
        bucketId: bucket.id,
        ...payload,
      });
      loadContents(bucket.id);
      toast.success("Credentials saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save credentials"))
  );
  if (err) throw err;
}

// A generic bucket secret has no service_id: it is referenced directly
// (e.g. ${bucket.secrets.<key>}) instead of resolved through a workspace service.
async function saveBucketGenericSecret(
  bucket: BucketSummary | undefined,
  payload: BucketSecretFormPayload,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  if (!bucket) return;
  const err = await withSaving(
    setSaving,
    async () => {
      await api.workspace.upsertBucketSecret({
        bucketId: bucket.id,
        ...payload,
      });
      loadContents(bucket.id);
      toast.success("Secret saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save secret"))
  );
  if (err) throw err;
}

/** Requires explicit consent for the exact stored keys before deleting a credential. */
async function removeBucketSecret(
  item: SecretMeta,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  // Paired credentials are one row; the confirmation must cover every key removed together.
  const keyNames = item.key_names?.length ? [...item.key_names] : [item.key_name];
  const confirmed = await toast.confirm(
    `Remove secret stored under "${keyNames.join('", "')}"? Apps using this credential may stop working. This cannot be undone.`,
    { confirmLabel: "Remove secret", cancelLabel: "Cancel" }
  );
  // Cancellation, dismissal, or an unexpected result cannot authorize a mutation.
  if (confirmed !== true) return;
  await withSaving(
    setSaving,
    async () => {
      await api.workspace.deleteSecrets(
        item.bucket_id,
        item.service_id,
        keyNames
      );
      loadContents(item.bucket_id);
      toast.success("Secret removed");
    },
    (err) => toast.error(errorMessage(err, "Failed to remove secret"))
  );
}

function rewindContentPage(
  items: unknown[],
  total: number,
  page: number,
  setPage: (page: number) => void
) {
  if (items.length === 0 && total > 0 && page > 0) setPage(page - 1);
}

/** Confirms removal using the environment key only, never its stored value. */
async function removeBucketValue(
  item: BucketValue,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  const confirmed = await toast.confirm(
    `Remove environment value "${item.key_name}"? Apps using this value may stop working. This cannot be undone.`,
    { confirmLabel: "Remove value", cancelLabel: "Cancel" }
  );
  // No save state, reload, or DELETE may run until the user explicitly confirms.
  if (confirmed !== true) return;
  await withSaving(
    setSaving,
    async () => {
      await api.workspace.deleteBucketValue(
        item.bucket_id,
        item.service_id,
        item.key_name
      );
      loadContents(item.bucket_id);
      toast.success("Value removed");
    },
    (err) => toast.error(errorMessage(err, "Failed to remove value"))
  );
}

/** Preserves typed-reference confirmation before removing a reusable connected user. */
async function removeBucketConnection(
  item: AuthConnection,
  setDeletingConnectionId: (connectionId: string | null) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  const typedRef = await toast.prompt(connectedUserDeletePrompt(item), {
    placeholder: item.end_user_ref,
  });
  // Empty, cancelled, or mismatched references never authorize connection removal.
  if (!item.end_user_ref || typedRef !== item.end_user_ref) {
    // Cancellation is not an error; only an attempted mismatch needs feedback.
    if (typedRef !== null) toast.error("Connected user ref did not match");
    return;
  }
  setDeletingConnectionId(item.id);
  try {
    await deleteBucketAuthConnection(item.bucket_id, item.id);
    loadContents(item.bucket_id);
    toast.success("Connected user removed");
  } catch (err) {
    toast.error(errorMessage(err, "Failed to remove connected user"));
  } finally {
    setDeletingConnectionId(null);
  }
}

/** Explains the exact connected-user reference required without exposing OAuth credentials. */
function connectedUserDeletePrompt(item: AuthConnection): string {
  // Connected users are reusable OAuth credentials, so removal should be an
  // explicit act instead of a one-click row action.
  return `Type "${item.end_user_ref}" to remove this connected user.`;
}

async function withSaving(
  setSaving: (saving: boolean) => void,
  action: () => Promise<void>,
  onError: (err: unknown) => void
): Promise<unknown | null> {
  setSaving(true);
  try {
    await action();
    return null;
  } catch (err) {
    onError(err);
    return err;
  } finally {
    setSaving(false);
  }
}
