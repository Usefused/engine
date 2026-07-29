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
  type BucketEntryKind,
  type SecretFormPayload,
  type ValueFormPayload,
} from "~/lib/buckets";
import { useToast } from "~/components/Toast";

export const meta: MetaFunction = () => [{ title: "Buckets - Fused" }];

const BUCKET_PAGE_SIZE = 12;
const BUCKET_CONTENT_PAGE_SIZE = 10;
const BUCKET_OVERVIEW_PAGE_SIZE = 5;

export default function BucketsPage() {
  const toast = useToast();
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
  const [entryModalKind, setEntryModalKind] = useState<BucketEntryKind | null>(
    null
  );
  const contentRequestID = useRef(0);
  const activeTab = bucketTabFromSearch(searchParams);

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

  const selectBucket = (bucketId: string) => {
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

  const selectTab = (tab: BucketDetailTab) => {
    setSearchParams(bucketTabSearchParams(searchParams, tab), {
      replace: true,
    });
  };

  const addEntry = (kind: BucketEntryKind) => {
    selectTab(kind === "secret" ? "secrets" : "env");
    setEntryModalKind(kind);
  };

  const clearSelectedBucket = () => {
    contentRequestID.current++;
    setLoadingContents(false);
    setEntryModalKind(null);
    setSearchParams(bucketSearchParams(searchParams, ""), { replace: true });
  };

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

  const loadContents = (bucketId: string) => {
    const requestID = ++contentRequestID.current;
    setLoadingContents(true);
    readBucketContents(bucketId, contentPages)
      .then((state) => {
        if (requestID !== contentRequestID.current) return;
        applyBucketContents(state, {
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
          setConnectSummary,
        });
      })
      .catch((err) => {
        if (requestID === contentRequestID.current)
          toast.error(errorMessage(err, "Failed to load bucket contents"));
      })
      .finally(() => {
        if (requestID === contentRequestID.current) setLoadingContents(false);
      });
  };

  const loadSelectedBucket = (bucketId: string) => {
    readBucketSummary(bucketId)
      .then(setSelectedBucketOverride)
      .catch((err) =>
        toast.error(errorMessage(err, "Failed to load selected bucket"))
      );
  };

  const loadServices = () => {
    readWorkspaceServices()
      .then(setServices)
      .catch((err) =>
        toast.error(errorMessage(err, "Failed to load workspace services"))
      );
  };

  const reloadBucketState = (bucketId: string) => {
    loadBucketList();
    loadContents(bucketId);
  };

  useEffect(() => {
    loadBucketList();
  }, [page]);

  useEffect(() => {
    loadServices();
  }, []);

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
  }, [selectedBucket?.id, selectedBucketId, contentPages]);

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

  const onBucketCreated = async (name: string) => {
    setPage(0);
    const state = await readBuckets(BUCKET_PAGE_SIZE, 0);
    setBuckets(state.bucketSummaries);
    setBucketTotal(state.total || 0);
    selectBucket(
      state.bucketSummaries.find((bucket) => bucket.name === name)?.id ||
        preferredBucketID(state.bucketSummaries)
    );
    toast.success("Bucket created");
  };

  const deleteSelectedBucket = async () => {
    if (!selectedBucket || selectedBucket.is_default) return;
    const typedName = await toast.prompt(
      deleteBucketPrompt(selectedBucket, connectSummary),
      { placeholder: selectedBucket.name }
    );
    if (typedName !== selectedBucket.name) {
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
      <BucketPageHeader onCreateClick={() => setModalOpen(true)} />
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
        activeTab={activeTab}
        loading={loadingContents}
        saving={saving}
        deletingConnectionId={deletingConnectionId}
        entryKind={entryModalKind}
        onClose={clearSelectedBucket}
        onRefresh={loadContents}
        onDeleteBucket={deleteSelectedBucket}
        onTabChange={selectTab}
        onAddEntry={addEntry}
        onCancelEntry={() => setEntryModalKind(null)}
        onSaveSecret={saveSecret}
        onSaveSecrets={saveSecrets}
        onSaveValue={saveValue}
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
      />
      <BucketCreateModal
        open={modalOpen}
        onClose={() => setModalOpen(false)}
        onCreated={onBucketCreated}
      />
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

function applyBucketContents(
  state: BucketContentState,
  setters: BucketContentSetters
) {
  const values = state.bucketValues ?? [];
  const secrets = state.secretMetas ?? [];
  const connections = state.authConnections ?? [];
  setters.setBucketValues(values);
  setters.setBucketValueTotal(
    totalOrLength(state.bucketValueTotal, values.length)
  );
  setters.setSecretMetas(secrets);
  setters.setSecretMetaTotal(
    totalOrLength(state.secretMetaTotal, secrets.length)
  );
  setters.setAuthConnections(connections);
  setters.setAuthConnectionTotal(
    totalOrLength(state.authConnectionTotal, connections.length)
  );
  setters.setBucketSDKs(state.bucketSDKs ?? []);
  setters.setBucketSDKTotal(state.bucketSDKTotal ?? 0);
  setters.setBucketServices(state.bucketServices ?? []);
  setters.setBucketServiceTotal(state.bucketServiceTotal ?? 0);
  setters.setConnectionServices(state.connectionServices ?? []);
  setters.setConnectSummary(state.connectSummary ?? null);
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

function deleteBucketPrompt(
  bucket: BucketSummary,
  summary: BucketConnectSummary | null
): string {
  const connectedUsers = summary?.connected_user_count || 0;
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
      toast.success("Secret saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save secret"))
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
      toast.success("Secrets saved");
    },
    (err) => toast.error(errorMessage(err, "Failed to save secrets"))
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

async function removeBucketSecret(
  item: SecretMeta,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  await withSaving(
    setSaving,
    async () => {
      await api.workspace.deleteSecrets(
        item.bucket_id,
        item.service_id,
        item.key_names?.length ? item.key_names : [item.key_name]
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

async function removeBucketValue(
  item: BucketValue,
  setSaving: (saving: boolean) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
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

async function removeBucketConnection(
  item: AuthConnection,
  setDeletingConnectionId: (connectionId: string | null) => void,
  loadContents: (bucketId: string) => void,
  toast: ReturnType<typeof useToast>
) {
  const typedRef = await toast.prompt(connectedUserDeletePrompt(item), {
    placeholder: item.end_user_ref,
  });
  if (typedRef !== item.end_user_ref) {
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
