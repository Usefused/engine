import { Link } from "@remix-run/react";
import { ArrowLeft, Plus } from "lucide-react";

type BucketPageHeaderProps = {
  onCreateClick: () => void;
};

export function BucketPageHeader({ onCreateClick }: BucketPageHeaderProps) {
  return (
    <>
      <Link to="/integrations" className="inline-flex items-center text-sm text-slate-500 hover:text-slate-800 transition-colors">
        <ArrowLeft className="w-4 h-4 mr-2" />
        Back to integrations
      </Link>

      <div className="flex flex-col md:flex-row md:items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-slate-900">Buckets</h1>
          <p className="text-slate-500 mt-1">Credential namespaces for SDKs, MCP servers, secrets, connected users, and plain values.</p>
        </div>
        <button
          type="button"
          onClick={onCreateClick}
          className="inline-flex items-center justify-center gap-2 px-4 py-2 rounded-lg bg-blue-600 text-sm font-medium text-white hover:bg-blue-700"
        >
          <Plus className="w-4 h-4" />
          New Bucket
        </button>
      </div>
    </>
  );
}
