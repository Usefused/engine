import { ChevronLeft, ChevronRight } from "lucide-react";

type BucketPaginationProps = {
  total: number;
  page: number;
  pageSize: number;
  onPageChange: (page: number) => void;
};

export function BucketPagination({ total, page, pageSize, onPageChange }: BucketPaginationProps) {
  if (total <= pageSize) return null;
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(page, pageCount - 1);
  return (
    <div className="flex items-center justify-between border-t border-slate-100 px-4 py-3 text-xs text-slate-500">
      <span>
        Page {current + 1} of {pageCount} · {total} total
      </span>
      <div className="flex items-center gap-1">
        <button
          type="button"
          onClick={() => onPageChange(Math.max(0, current - 1))}
          disabled={current === 0}
          className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40"
          aria-label="Previous page"
          title="Previous page"
        >
          <ChevronLeft className="h-4 w-4" />
        </button>
        <button
          type="button"
          onClick={() => onPageChange(Math.min(pageCount - 1, current + 1))}
          disabled={current >= pageCount - 1}
          className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40"
          aria-label="Next page"
          title="Next page"
        >
          <ChevronRight className="h-4 w-4" />
        </button>
      </div>
    </div>
  );
}
