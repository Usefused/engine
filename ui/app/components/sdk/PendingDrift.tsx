import { AlertTriangle } from "lucide-react";

export type DriftChangeItem = {
  field: string;
  old_value: string;
  new_value: string;
  severity: string;
  description: string;
};

export type PendingDriftItem = {
  id: string;
  status: string;
  integration_object_id: string;
  webhook_object_id: string | null;
  diff: DriftChangeItem[];
};

/** Pending drift detail list -- what changed, not just a count. Endpoint vs
 * webhook is inferred from which ID field is set, and shown with a short ID
 * since this view doesn't resolve human-readable endpoint/webhook names.
 * Shared between the SDK detail page and the Observability SDK tab -- both
 * feed it the same `sdkAnalytics(name).pending_drift` shape. */
export function PendingDriftSection({ items }: { items: PendingDriftItem[] }) {
  if (items.length === 0) {
    return (
      <div className="bg-slate-50 p-6 rounded-xl border border-slate-200 shadow-sm text-sm text-slate-500">
        No pending drift. This SDK's endpoints and webhooks match what's currently documented.
      </div>
    );
  }

  return (
    <div className="rounded-lg border border-slate-200 overflow-hidden divide-y divide-slate-100">
      {items.map(item => {
        const isWebhook = !!item.webhook_object_id;
        const shortId = (isWebhook ? item.webhook_object_id : item.integration_object_id)?.slice(0, 8);
        return (
          <div key={item.id} className="p-4 bg-white">
            <div className="flex items-center gap-2 mb-2">
              <span className="text-xs font-semibold uppercase tracking-wider px-2 py-0.5 rounded bg-slate-100 text-slate-600">
                {isWebhook ? "Webhook" : "Endpoint"}
              </span>
              <span className="text-xs text-slate-400 font-mono">{shortId}</span>
            </div>
            <div className="space-y-1.5">
              {item.diff.map((change, idx) => (
                <div key={idx} className="flex items-start gap-2 text-sm">
                  {change.severity === "breaking" ? (
                    <AlertTriangle className="w-4 h-4 text-red-500 shrink-0 mt-0.5" />
                  ) : (
                    <AlertTriangle className="w-4 h-4 text-amber-400 shrink-0 mt-0.5" />
                  )}
                  <div>
                    <span className="text-slate-700">{change.description || change.field}</span>
                    {change.severity && (
                      <span className={`ml-2 text-xs font-medium ${change.severity === "breaking" ? "text-red-500" : "text-amber-500"}`}>
                        {change.severity}
                      </span>
                    )}
                  </div>
                </div>
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}
