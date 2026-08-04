import { Plus } from "lucide-react";

type BucketPageHeaderProps = {
  onCreateClick: () => void;
};

export function BucketPageHeader({ onCreateClick }: BucketPageHeaderProps) {
  return (
    <div className="flex flex-col justify-between gap-4 md:flex-row md:items-start">
        <div>
          <h1 className="text-2xl font-bold text-slate-900">Credentials</h1>
          <p className="mt-1 text-slate-500">Keep service credentials separate by environment, customer, or team.</p>
        </div>
        <button
          type="button"
          onClick={onCreateClick}
          className="inline-flex w-auto max-w-full self-start items-center justify-center gap-2 whitespace-nowrap rounded-lg bg-slate-950 px-3 py-2 text-sm font-medium text-white hover:bg-slate-800 md:self-auto"
        >
          <Plus className="w-4 h-4" />
          New credential set
        </button>
    </div>
  );
}
