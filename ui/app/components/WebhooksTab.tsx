import { useState } from "react";
import { Info, AlertTriangle, Search } from "lucide-react";
import type { IntegrationObject, Service } from "~/lib/api";
import { WebhookRow } from "~/components/EndpointRow";

interface Webhook {
  id: string;
  name: string;
  description?: string;
  method?: string;
  request_body?: unknown;
}

interface WebhooksTabProps {
  srv: Service;
  setSelectedEndpoint: (ep: IntegrationObject | null) => void;
}

export default function WebhooksTab({
  srv,
  setSelectedEndpoint,
}: WebhooksTabProps) {
	const [showGuide, setShowGuide] = useState(false);
	const [search, setSearch] = useState("");

  const renderedWebhooks = (srv.webhooks as Webhook[] || []).filter((wh) => {
    const q = search.toLowerCase();
    return !q || wh.name?.toLowerCase().includes(q) || wh.description?.toLowerCase().includes(q);
  });

  return (
    <div className="bg-white rounded-xl border border-slate-200">
      <div className="px-5 py-3 border-b border-slate-100 flex items-center justify-between gap-3 flex-wrap">
        <div className="flex items-center gap-3">
          <div className="relative">
            <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-slate-400 pointer-events-none" />
            <input
              type="text"
              placeholder="Search webhooks..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full sm:w-64 text-sm border border-slate-300 rounded-md pl-9 pr-8 py-1.5 focus:outline-none focus:border-slate-500 focus:ring-1 focus:ring-gray-500"
            />
            {search && (
              <button
                onClick={() => setSearch("")}
                className="absolute right-2.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 cursor-pointer"
              >
                ✕
              </button>
            )}
          </div>
          <button
            data-track="toggle_webhook_guide"
            onClick={() => setShowGuide(!showGuide)}
            className={`inline-flex items-center gap-1 text-xs font-semibold transition-colors cursor-pointer ${
              showGuide ? "text-indigo-800 underline" : "text-indigo-600 hover:text-indigo-800 hover:underline"
            }`}
          >
            <Info className="w-3.5 h-3.5" />
            Guide
          </button>
        </div>
      </div>

      {showGuide && (
        <div className="p-5 bg-indigo-50/50 border-b border-indigo-100/50 space-y-3 animate-in fade-in slide-in-from-top-2 duration-200">
          <div>
            <h4 className="text-xs font-bold text-indigo-900 uppercase tracking-wider mb-1 flex items-center gap-1.5">
              <Info className="w-3.5 h-3.5 text-indigo-600" />
              Webhook Normalization Guide
            </h4>
            <p className="text-xs text-indigo-700 leading-relaxed">
              Fused automatically normalizes payloads from incoming webhooks so the generated SDK receives a standard JSON object.
            </p>
          </div>
          <ul className="list-disc pl-4 text-xs text-indigo-700 space-y-1">
            <li><strong>GET Requests</strong>: URL query parameters are parsed and converted to a JSON payload.</li>
            <li><strong>Form Data</strong>: <code className="bg-indigo-100/70 px-1 py-0.5 rounded font-mono text-[10px]">application/x-www-form-urlencoded</code> bodies are converted to JSON.</li>
            <li><strong>Duplicate Keys</strong>: Multiple query parameters or form values with the same key (e.g. <code className="bg-indigo-100/70 px-1 py-0.5 rounded font-mono text-[10px]">?tag=a&tag=b</code>) are packed into a JSON array: <code className="bg-indigo-100/70 px-1 py-0.5 rounded font-mono text-[10px]">"tag": ["a", "b"]</code>.</li>
            <li><strong>Payload Structure</strong>: Webhook events arrive as structured JSON: <code className="bg-indigo-100/70 px-1 py-0.5 rounded font-mono text-[10px]">{`{ body, headers, query, params, path }`}</code>. This makes it clear where values originated (query, header, body, or path params).</li>
          </ul>
          <div className="p-3 bg-amber-50 border border-amber-200 rounded-lg flex gap-2">
            <AlertTriangle className="w-4 h-4 text-amber-600 shrink-0 mt-0.5" />
            <div>
              <h5 className="text-xs font-semibold text-amber-950">GET HMAC Signature Limitation</h5>
              <p className="text-xs text-amber-800 leading-relaxed mt-0.5">
                We do not support HMAC signature verification for GET webhooks because signature logic for GET parameters varies heavily between third-party services. If you require security validation on GET webhooks, please use <strong>Static Token</strong> authentication.
              </p>
            </div>
          </div>
        </div>
      )}

      <div className="divide-y divide-slate-100 mt-2">
        {renderedWebhooks.length > 0 ? (
          renderedWebhooks.map((wh) => (
            <WebhookRow 
              key={wh.id} 
              wh={wh} 
              onClick={() => setSelectedEndpoint({ ...wh, path: wh.name, isWebhook: true } as unknown as IntegrationObject)} 
			  selectable={false}
            />
          ))
        ) : (
          <div className="p-8 text-center text-slate-500 text-sm">
            No webhooks defined for this service.
          </div>
        )}
      </div>
    </div>
  );
}
