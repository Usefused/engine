import { Check, Copy } from "lucide-react";

/** Presents one-time runtime credentials only after the shared app lifecycle completes. */
export function ExecutionTokenField({ token, copied, onCopy }: { token: string; copied: boolean; onCopy: () => void }) {
  return (
    <div className="mt-3 flex items-center gap-2">
      <code className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap rounded border border-slate-200 bg-slate-50 px-2 py-1.5 text-xs">{token}</code>
      <button
        type="button"
        title="Copy execution token"
        aria-label="Copy execution token"
        className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded border border-slate-200 bg-white hover:bg-slate-50"
        // Copy only on an explicit action; keep the credential out of persistent storage.
        onClick={async () => {
          await navigator.clipboard.writeText(token);
          onCopy();
        }}
      >
        {/* A successful copy replaces the affordance with confirmation. */}
        {copied ? <Check className="h-4 w-4 text-emerald-600" /> : <Copy className="h-4 w-4" />}
      </button>
    </div>
  );
}
