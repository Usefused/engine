import { useEffect, useMemo, useState } from "react";
import { Link } from "@remix-run/react";
import { Database, Plus, RefreshCw } from "lucide-react";
import { BucketCreateModal } from "~/components/buckets/BucketCreateModal";
import { useToast } from "~/components/Toast";
import { type Bucket } from "~/lib/api";
import { errorMessage, readBucketsForSDK } from "~/lib/buckets";

type BucketRow = {
  bucket: Bucket;
  isAttached: boolean;
};

export function SDKCredentialBucketsSection({ appFamilyId }: { appFamilyId: string }) {
  const toast = useToast();
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [sdkBuckets, setSdkBuckets] = useState<Bucket[]>([]);
  const [loading, setLoading] = useState(false);
  const [modalOpen, setModalOpen] = useState(false);

  const loadBuckets = () => {
    setLoading(true);
    readBucketsForSDK(appFamilyId)
      .then((state) => {
        setBuckets(state.buckets);
        setSdkBuckets(state.sdkBuckets);
      })
      .catch((err) => toast.error(errorMessage(err, "Failed to load credential sets")))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    loadBuckets();
  }, [appFamilyId]);

  const visibleBuckets = useMemo(() => visibleCredentialBuckets(buckets, sdkBuckets), [buckets, sdkBuckets]);

  const onBucketCreated = (name: string) => {
    loadBuckets();
    toast.success(`Credential set ${name} created`);
  };

  return (
    <div>
      <SectionHeader
        hasAttachedBucket={sdkBuckets.length > 0}
        loading={loading}
        onRefresh={loadBuckets}
        onCreate={() => setModalOpen(true)}
      />
      <BucketRows rows={visibleBuckets} loading={loading} />
      <BucketCreateModal open={modalOpen} onClose={() => setModalOpen(false)} onCreated={onBucketCreated} />
    </div>
  );
}

function visibleCredentialBuckets(buckets: Bucket[], sdkBuckets: Bucket[]): BucketRow[] {
  if (sdkBuckets.length > 0) {
    return sdkBuckets.map((bucket) => ({ bucket, isAttached: true }));
  }
  // Runtime credential resolution falls back to the workspace default bucket,
  // so SDK details should show the same effective source instead of appearing empty.
  return buckets
    .filter((bucket) => bucket.is_default)
    .map((bucket) => ({ bucket, isAttached: false }));
}

function SectionHeader({ hasAttachedBucket, loading, onRefresh, onCreate }: { hasAttachedBucket: boolean; loading: boolean; onRefresh: () => void; onCreate: () => void }) {
  return (
    <div className="flex items-center justify-between gap-3 mb-3">
      <div>
        <h4 className="text-sm font-semibold text-slate-700 uppercase tracking-wider">Credential sets</h4>
        <p className="text-xs text-slate-500 mt-1">
          {hasAttachedBucket ? "This app uses the attached credential set at runtime." : "No credential set is linked to this app yet."}
        </p>
      </div>
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={onRefresh}
          className="p-2 rounded-lg border border-slate-200 text-slate-500 hover:text-slate-700 hover:bg-slate-50 cursor-pointer"
          aria-label="Refresh credential sets"
          title="Refresh"
        >
          <RefreshCw className={`w-4 h-4 ${loading ? "animate-spin" : ""}`} />
        </button>
        <button
          type="button"
          onClick={onCreate}
          className="inline-flex items-center gap-2 px-3 py-2 rounded-lg border border-slate-200 text-sm font-medium text-slate-600 hover:bg-slate-50 hover:text-slate-800 cursor-pointer"
        >
          <Plus className="w-4 h-4" />
          New credential set
        </button>
      </div>
    </div>
  );
}

function BucketRows({ rows, loading }: { rows: BucketRow[]; loading: boolean }) {
  return (
    <div className="rounded-lg border border-slate-200 bg-white divide-y divide-slate-100 overflow-hidden">
      {loading ? <EmptyBucketRows label="Loading credential sets..." /> : <LoadedBucketRows rows={rows} />}
    </div>
  );
}

function LoadedBucketRows({ rows }: { rows: BucketRow[] }) {
  if (rows.length === 0) return <EmptyBucketRows label="No linked credential set." />;
  return rows.map(({ bucket, isAttached }) => (
    <Link
      key={bucket.id}
      to={`/integrations/buckets?bucket=${encodeURIComponent(bucket.id)}`}
      className="flex items-center justify-between gap-3 px-4 py-3 hover:bg-slate-50 transition-colors"
    >
      <span className="flex min-w-0 items-center gap-3">
        <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-slate-100 text-slate-500">
          <Database className="w-4 h-4" />
        </span>
        <span className="min-w-0">
          <span className="block truncate text-sm font-semibold text-slate-800">{bucket.name}</span>
          <span className="block text-xs text-slate-500">{isAttached ? "Used at runtime" : "Workspace default"}</span>
        </span>
      </span>
      <span className="shrink-0 text-xs font-medium text-blue-600">Open</span>
    </Link>
  ));
}

function EmptyBucketRows({ label }: { label: string }) {
  return <div className="px-4 py-5 text-sm text-slate-400">{label}</div>;
}
