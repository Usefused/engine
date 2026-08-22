import { ChevronLeft, ChevronRight, Database, KeyRound, RefreshCw, Users } from "lucide-react";
import { type BucketSummary } from "~/lib/api";

type BucketListProps = {
  buckets: BucketSummary[];
  selectedBucketId: string;
  loading: boolean;
  total: number;
  page: number;
  pageSize: number;
  onRefresh: () => void;
  onPageChange: (page: number) => void;
  onSelect: (bucketId: string) => void;
};

export function BucketList(props: BucketListProps) {
  const pageCount = Math.max(1, Math.ceil(props.total / props.pageSize));
  return (
    <section className="min-w-0 rounded-lg border border-slate-200 bg-white">
      <BucketListHeader loading={props.loading} total={props.total} onRefresh={props.onRefresh} />
      <BucketRows
        buckets={props.buckets}
        selectedBucketId={props.selectedBucketId}
        loading={props.loading}
        onSelect={props.onSelect}
      />
      <BucketPagination
        page={props.page}
        pageCount={pageCount}
        pageSize={props.pageSize}
        total={props.total}
        onPageChange={props.onPageChange}
      />
    </section>
  );
}

function BucketListHeader({ loading, total, onRefresh }: { loading: boolean; total: number; onRefresh: () => void }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-slate-100 px-4 py-3">
      <div>
        <h2 className="text-sm font-semibold uppercase tracking-wider text-slate-600">Credential sets</h2>
        <p className="mt-1 text-xs text-slate-400">{total} total</p>
      </div>
      <button
        type="button"
        onClick={onRefresh}
        className="p-1.5 rounded-md text-slate-400 hover:text-slate-600 hover:bg-slate-100"
        aria-label="Refresh credential sets"
        title="Refresh"
      >
        <RefreshCw className={`w-4 h-4 ${loading ? "animate-spin" : ""}`} />
      </button>
    </div>
  );
}

function BucketRows({ buckets, selectedBucketId, loading, onSelect }: Pick<BucketListProps, "buckets" | "selectedBucketId" | "loading" | "onSelect">) {
  if (loading) return <BucketEmptyState label="Loading credential sets..." />;
  if (buckets.length === 0) return <BucketEmptyState label="No credential sets yet." />;

  return (
    <div className="divide-y divide-slate-100">
      {buckets.map((bucket) => (
        <BucketRow
          key={bucket.id}
          bucket={bucket}
          selected={bucket.id === selectedBucketId}
          onSelect={() => onSelect(bucket.id)}
        />
      ))}
    </div>
  );
}

function BucketRow({ bucket, selected, onSelect }: { bucket: BucketSummary; selected: boolean; onSelect: () => void }) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={`grid w-full cursor-pointer grid-cols-[minmax(0,1fr)_auto] items-center gap-4 px-4 py-3 text-left transition-colors ${
        selected ? "bg-blue-50/70" : "hover:bg-slate-50"
      }`}
    >
      <span className="min-w-0">
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate text-sm font-semibold text-slate-900">{bucket.name}</span>
          {bucket.is_default && <span className="pointer-events-none rounded-md bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600">Default</span>}
        </span>
        <span className="mt-1 block truncate font-mono text-xs text-slate-400">{bucket.id}</span>
      </span>
      <span className="flex shrink-0 items-center gap-4 text-xs text-slate-500">
        <span className="inline-flex items-center gap-1.5" title="Secrets">
          <KeyRound className="w-3.5 h-3.5" />
          {bucket.secret_count}
        </span>
        <span className="inline-flex items-center gap-1.5" title="Env">
          <Database className="w-3.5 h-3.5" />
          {bucket.value_count}
        </span>
        <span className="inline-flex items-center gap-1.5" title="Connected Users">
          <Users className="w-3.5 h-3.5" />
          {bucket.connected_user_count ?? 0}
        </span>
      </span>
    </button>
  );
}

function BucketPagination({ page, pageCount, pageSize, total, onPageChange }: Pick<BucketListProps, "page" | "pageSize" | "total" | "onPageChange"> & { pageCount: number }) {
  const start = total === 0 ? 0 : page * pageSize + 1;
  const end = Math.min(total, (page + 1) * pageSize);
  return (
    <div className="flex items-center justify-between gap-3 border-t border-slate-100 px-4 py-3">
      <p className="text-xs text-slate-500">{start}-{end} of {total}</p>
      <div className="flex items-center gap-1">
        <button
          type="button"
          onClick={() => onPageChange(page - 1)}
          disabled={page === 0}
          className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
          aria-label="Previous credential set page"
          title="Previous"
        >
          <ChevronLeft className="w-4 h-4" />
        </button>
        <span className="text-xs text-slate-500 pl-2">Page</span>
        <select
          className="bg-white border border-slate-200 rounded px-2 py-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 mx-1"
          value={page}
          onChange={(e) => onPageChange(parseInt(e.target.value, 10))}
        >
          {Array.from({ length: pageCount }, (_, i) => i).map(p => (
            <option key={p} value={p}>{p + 1}</option>
          ))}
        </select>
        <span className="text-xs font-medium text-slate-500 pr-2">of {pageCount}</span>
        <button
          type="button"
          onClick={() => onPageChange(page + 1)}
          disabled={page + 1 >= pageCount}
          className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
          aria-label="Next credential set page"
          title="Next"
        >
          <ChevronRight className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}

function BucketEmptyState({ label }: { label: string }) {
  return <div className="px-4 py-10 text-center text-sm text-slate-400">{label}</div>;
}
