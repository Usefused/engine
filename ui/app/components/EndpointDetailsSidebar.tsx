import { useState, useEffect } from "react";
import { AlertTriangle, ChevronDown, ChevronUp, Loader2, Copy, Check } from "lucide-react";
import { SchemaViewer } from "~/components/SchemaViewer";
import { type IntegrationObject, type DriftSnapshot } from "~/lib/api";
import { stripLinks } from "~/lib/format";

function ResponseSchemaViewer({ code, schema, serviceId }: { code: string; schema: any; serviceId: string }) {
  const [isOpen, setIsOpen] = useState(false);

  const codeStyle = code.startsWith('2')
    ? { bg: "bg-emerald-50",    text: "text-emerald-700",    border: "border-emerald-200",    dotColor: "bg-emerald-500"    }
    : code.startsWith('4')
    ? { bg: "bg-amber-50",     text: "text-amber-700",      border: "border-amber-200",      dotColor: "bg-amber-500"     }
    : code.startsWith('5')
    ? { bg: "bg-red-50",       text: "text-red-700",        border: "border-red-200",        dotColor: "bg-red-500"       }
    : { bg: "bg-slate-50",     text: "text-slate-700",      border: "border-slate-200",      dotColor: "bg-slate-500"     };

  return (
    <div className="border border-slate-200 rounded-xl overflow-hidden shadow-sm">
      <button
        data-track="toggle_response_schema"
        onClick={() => setIsOpen(!isOpen)}
        className={`w-full px-4 py-2.5 flex items-center justify-between cursor-pointer transition-colors focus:outline-none ${isOpen ? 'bg-slate-50 border-b border-slate-200' : 'bg-white hover:bg-slate-50/50'}`}
      >
        <div className="flex items-center gap-2">
          <span className={`inline-flex items-center gap-1.5 text-[10px] font-bold px-2 py-0.5 rounded-md border uppercase tracking-wider ${codeStyle.bg} ${codeStyle.text} ${codeStyle.border}`}>
            <span className={`w-1.5 h-1.5 rounded-full ${codeStyle.dotColor}`} />
            {code}
          </span>
          <span className="text-xs font-medium text-slate-900">Schema</span>
        </div>
        {isOpen ? <ChevronUp className="w-4 h-4 text-slate-500" /> : <ChevronDown className="w-4 h-4 text-slate-500" />}
      </button>
      {isOpen && (
        <div className="bg-[#161c27] p-4 animate-in fade-in slide-in-from-top-1 duration-200">
          <SchemaViewer schema={schema} serviceId={serviceId} />
        </div>
      )}
    </div>
  );
}

export interface EndpointDetailsSidebarProps {
  selectedEndpoint: IntegrationObject;
  setSelectedEndpoint: (ep: IntegrationObject | null) => void;
  srv: any;
  drift: DriftSnapshot[];
  driftAction: string | null;
  handleDismiss: (id: string) => void;
	handleApply: (id: string) => void;
}

export default function EndpointDetailsSidebar({
  selectedEndpoint,
  setSelectedEndpoint,
  srv,
  drift,
  driftAction,
	handleDismiss,
	handleApply,
}: EndpointDetailsSidebarProps) {
  const [reqSchemaOpen, setReqSchemaOpen] = useState(false);
  const [copiedPath, setCopiedPath] = useState(false);

  const isLoading = !(selectedEndpoint as any)._detailsLoaded && !selectedEndpoint.isWebhook;

  let endpointType = "REST API";
  if (selectedEndpoint.isWebhook) {
    endpointType = "Webhook";
  } else if (selectedEndpoint.graphql_query || selectedEndpoint.method === "GRAPHQL") {
    endpointType = "GraphQL";
  } else if (selectedEndpoint.responses && Object.values(selectedEndpoint.responses).some(r => r?.format === "binary" || r?.type === "file")) {
    endpointType = "File Transfer";
  } else if (selectedEndpoint.responses && Object.values(selectedEndpoint.responses).some(r => r?.format === "sse" || r?.format === "stream")) {
    endpointType = "Event Stream";
  }

  return (
    <>
      <div 
        className="fixed inset-0 bg-slate-900/20 z-40 transition-opacity" 
        onClick={() => setSelectedEndpoint(null)}
      />
      <div className="fixed inset-y-0 right-0 w-full md:w-[600px] bg-white shadow-2xl z-50 overflow-y-auto overflow-x-hidden transform transition-transform border-l border-slate-200 flex flex-col">
        <div className="p-6 border-b border-slate-100 flex items-center justify-between gap-2 sticky top-0 bg-white/90 backdrop-blur z-10 min-w-0">
          <div className="flex items-center gap-3 min-w-0 overflow-hidden flex-1">
            <span className={`shrink-0 text-xs font-bold px-2 py-1 rounded ${
              selectedEndpoint.isWebhook ? "bg-purple-100 text-purple-700" :
              selectedEndpoint.method === "SOAP" ? "bg-purple-100 text-purple-700" :
              selectedEndpoint.method === "GET" ? "bg-green-100 text-green-700" :
              selectedEndpoint.method === "POST" ? "bg-blue-100 text-blue-700" :
              selectedEndpoint.method === "DELETE" ? "bg-red-100 text-red-700" :
              "bg-slate-100 text-slate-700"
            }`}>
              {selectedEndpoint.isWebhook ? "WEBHOOK" : selectedEndpoint.method}
            </span>
            <div className="flex items-center gap-1.5 group min-w-0 overflow-hidden">
              <code className="text-sm text-slate-800 break-all min-w-0 overflow-hidden">{selectedEndpoint.path}</code>
              <button
                data-track="copy_endpoint_path"
                onClick={(e) => {
                  e.stopPropagation();
                  navigator.clipboard.writeText(selectedEndpoint.path);
                  setCopiedPath(true);
                  setTimeout(() => setCopiedPath(false), 2000);
                }}
                className="opacity-0 group-hover:opacity-100 p-1 text-slate-400 hover:text-slate-600 transition-opacity cursor-pointer rounded hover:bg-slate-100"
                title="Copy Path"
              >
                {copiedPath ? <Check className="w-3.5 h-3.5 text-green-500" /> : <Copy className="w-3.5 h-3.5" />}
              </button>
            </div>
            
            {!selectedEndpoint.isWebhook && (
              <span className={`shrink-0 inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-bold uppercase tracking-wider border ${
                endpointType === "GraphQL" ? "bg-pink-50 text-pink-700 border-pink-200" :
                endpointType === "File Transfer" ? "bg-indigo-50 text-indigo-700 border-indigo-200" :
                endpointType === "Event Stream" ? "bg-cyan-50 text-cyan-700 border-cyan-200" :
                "bg-slate-50 text-slate-500 border-slate-200"
              }`}>
                {endpointType}
              </span>
            )}

            {selectedEndpoint.deprecated && (
              <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-semibold bg-red-50 text-red-700 border border-red-200 uppercase tracking-wider">
                Deprecated
              </span>
            )}
          </div>
		  <div className="flex items-center gap-1">
            <button
              data-track="close_endpoint_sidebar"
              onClick={() => setSelectedEndpoint(null)}
              className="p-2 text-slate-400 hover:text-slate-600 hover:bg-slate-100 rounded-full transition-colors cursor-pointer"
            >
              ✕
            </button>
          </div>
        </div>

        {selectedEndpoint.status === "drifted" && drift.some(s => s.integration_object_id === selectedEndpoint.id) && (
          <div className="bg-orange-50 border-b border-orange-100 p-6">
            <div className="flex items-center justify-between mb-4">
              <h3 className="text-sm font-bold text-orange-900 flex items-center gap-2">
                <AlertTriangle className="w-4 h-4 text-orange-600" />
                Pending Semantic Drift Detected
              </h3>
              <div className="flex gap-2">
                {drift.filter(s => s.integration_object_id === selectedEndpoint.id).map(snap => (
                  <div key={snap.id} className="flex gap-2">
                    <button
                      data-track="dismiss_drift_change"
                      onClick={() => handleDismiss(snap.id)}
                      disabled={driftAction === snap.id}
                      className="px-3 py-1.5 text-xs font-semibold text-orange-800 bg-white hover:bg-orange-100 border border-orange-200 rounded-md transition-colors disabled:opacity-50 cursor-pointer"
                    >
                      {driftAction === snap.id ? "Dismissing..." : "Dismiss"}
                    </button>
                    <button
                      data-track="review_drift_import"
                      onClick={() => handleApply(snap.id)}
                      disabled={driftAction === snap.id}
                      className="px-3 py-1.5 text-xs font-semibold text-white bg-orange-600 hover:bg-orange-700 rounded-md shadow-sm transition-colors disabled:opacity-50 cursor-pointer"
                    >
                      {driftAction === snap.id ? "Planning..." : "Review Import"}
                    </button>
                  </div>
                ))}
              </div>
            </div>
            
            <div className="space-y-4">
              {drift.filter(s => s.integration_object_id === selectedEndpoint.id).map(snap => (
                <div key={snap.id} className="space-y-3">
                  <p className="text-xs text-orange-800/80 mb-2">Detected at {new Date(snap.detected_at).toLocaleString()}</p>
                  {snap.diff.map((change, idx) => (
                    <div key={idx} className="bg-white rounded-lg border border-orange-200 overflow-hidden">
                      <div className="bg-orange-100/50 px-3 py-2 border-b border-orange-200 flex items-center justify-between">
                        <code className="text-xs font-bold text-orange-900">{change.field}</code>
                        {change.severity === "breaking" && (
                          <span className="text-[10px] font-bold uppercase tracking-wider text-red-600 bg-red-100 px-1.5 py-0.5 rounded border border-red-200">Breaking</span>
                        )}
                      </div>
                      {change.description && (
                        <div className="px-3 py-2.5 bg-orange-50/50 border-b border-orange-100/50">
                          <p className="text-xs text-orange-800 leading-relaxed font-medium">
                           {change.description}
                          </p>
                        </div>
                      )}
                      <div className="grid grid-cols-2 divide-x divide-slate-100">
                        <div className="p-3 bg-red-50/30">
                          <div className="text-[10px] font-semibold text-slate-400 uppercase tracking-wider mb-1.5">Previous</div>
                          <pre className="text-xs text-red-900 whitespace-pre-wrap break-words font-mono max-h-[400px] overflow-y-auto custom-scrollbar">
                            {typeof change.old_value === 'object' ? JSON.stringify(change.old_value, null, 2) : String(change.old_value || "null")}
                          </pre>
                        </div>
                        <div className="p-3 bg-green-50/30">
                          <div className="text-[10px] font-semibold text-slate-400 uppercase tracking-wider mb-1.5">New</div>
                          <pre className="text-xs text-green-900 whitespace-pre-wrap break-words font-mono max-h-[400px] overflow-y-auto custom-scrollbar">
                            {typeof change.new_value === 'object' ? JSON.stringify(change.new_value, null, 2) : String(change.new_value || "null")}
                          </pre>
                        </div>
                      </div>
                    </div>
                  ))}
                </div>
              ))}
            </div>
          </div>
        )}
        
        <div className="p-6 space-y-8 flex-1 flex flex-col">
          {isLoading ? (
            <div className="flex-1 flex flex-col items-center justify-center text-slate-400 min-h-[300px]">
              <Loader2 className="w-8 h-8 text-blue-500 animate-spin mb-4" />
              <p className="text-sm font-medium">Loading endpoint details...</p>
            </div>
          ) : (
            <>
              <div>
                <h2 className="text-lg font-semibold text-slate-900">{selectedEndpoint.name}</h2>
                {(selectedEndpoint.deprecated || selectedEndpoint.deprecation_date) && (
                  <div className="mt-3 bg-red-50 border border-red-200 rounded-lg p-3 flex gap-2">
                    <AlertTriangle className="w-5 h-5 text-red-500 shrink-0" />
                    <div>
                      <h4 className="text-sm font-semibold text-red-800">Endpoint Deprecated</h4>
                      <p className="text-xs text-red-700 mt-1">
                        {selectedEndpoint.deprecation_date
                          ? `This endpoint will be removed on ${new Date(selectedEndpoint.deprecation_date).toLocaleDateString()}. Please migrate to a newer version.`
                          : "This endpoint is deprecated and should no longer be used."}
                      </p>
                    </div>
                  </div>
                )}
                {selectedEndpoint.description && (
                  <div 
                    className="text-sm text-slate-600 mt-2 [&_p]:mb-1 last:[&_p]:mb-0 [&_code]:bg-slate-100 [&_code]:px-1 [&_code]:py-0.5 [&_code]:rounded" 
                    dangerouslySetInnerHTML={{ __html: stripLinks(selectedEndpoint.description) }} 
                  />
                )}
              </div>

              {selectedEndpoint.parameters && selectedEndpoint.parameters.length > 0 && (
                <div>
                  <h3 className="text-sm font-semibold text-slate-800 mb-3 border-b border-slate-100 pb-2">Parameters</h3>
                  <div className="bg-slate-50 border border-slate-200 rounded-lg overflow-hidden">
                    <table className="w-full text-left text-sm">
                      <thead className="bg-slate-100 text-slate-600">
                        <tr>
                          <th className="px-4 py-2 font-medium">Name</th>
                          <th className="px-4 py-2 font-medium">In</th>
                          <th className="px-4 py-2 font-medium">Type</th>
                          <th className="px-4 py-2 font-medium">Required</th>
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-slate-200">
                        {selectedEndpoint.parameters.map((p, idx) => (
                          <tr key={idx}>
                            <td className="px-4 py-3 font-mono text-xs text-slate-800">{p.name}</td>
                            <td className="px-4 py-3 text-slate-600">{p.in}</td>
                            <td className="px-4 py-3 font-mono text-xs text-slate-500">{p.type}</td>
                            <td className="px-4 py-3">
                              {p.required ? (
                                <span className="text-red-600 font-medium">Yes</span>
                              ) : (
                                <span className="text-slate-400">No</span>
                              )}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              )}

              {selectedEndpoint.request_body && (
                <div className="mb-6">
                  <h3 className="text-sm font-semibold text-slate-800 mb-3 border-b border-slate-100 pb-2">Request</h3>
                  <div className="border border-slate-200 rounded-xl overflow-hidden shadow-sm">
                    <button
                      data-track="toggle_request_schema"
                      onClick={() => setReqSchemaOpen(!reqSchemaOpen)}
                      className={`w-full px-4 py-2.5 flex items-center justify-between cursor-pointer transition-colors focus:outline-none ${reqSchemaOpen ? 'bg-slate-50 border-b border-slate-200' : 'bg-white hover:bg-slate-50/50'}`}
                    >
                      <div className="flex items-center gap-2">
                        <span className="text-[10px] font-bold px-2 py-0.5 rounded-md bg-slate-100 text-slate-700 border border-slate-200 uppercase tracking-wider">
                          {selectedEndpoint.isWebhook ? "PAYLOAD" : "BODY"}
                        </span>
                        <span className="text-xs font-medium text-slate-900">
                          {selectedEndpoint.isWebhook ? "Event Schema" : "Request Schema"}
                        </span>
                      </div>
                      {reqSchemaOpen ? (
                        <ChevronUp className="w-4 h-4 text-slate-500" />
                      ) : (
                        <ChevronDown className="w-4 h-4 text-slate-500" />
                      )}
                    </button>
                    {reqSchemaOpen && (
                      <div className="bg-[#161c27] p-4 animate-in fade-in slide-in-from-top-1 duration-200">
                        <SchemaViewer schema={selectedEndpoint.request_body} serviceId={srv.id} />
                      </div>
                    )}
                  </div>
                </div>
              )}

              {selectedEndpoint.responses && Object.keys(selectedEndpoint.responses).length > 0 && (
                <div>
                  <h3 className="text-sm font-semibold text-gray-800 mb-3 border-b border-gray-100 pb-2">Responses</h3>
                  <div className="space-y-4">
                    {Object.entries(selectedEndpoint.responses).map(([code, schema]) => (
                      <ResponseSchemaViewer key={code} code={code} schema={schema} serviceId={srv.id} />
                    ))}
                  </div>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </>
  );
}
