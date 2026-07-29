import { FormEvent, Dispatch, SetStateAction } from "react";
import { Pencil } from "lucide-react";

interface ServiceConfigSectionProps {
  srv: any;
  isAuth: boolean;
  editingConfig: boolean;
  setEditingConfig: Dispatch<SetStateAction<boolean>>;
  configForm: any;
  setConfigForm: Dispatch<SetStateAction<any>>;
  savingConfig: boolean;
  handleSaveConfig: (e: FormEvent) => void;
}

export function ServiceConfigSection({
  srv,
  isAuth,
  editingConfig,
  setEditingConfig,
  configForm,
  setConfigForm,
  savingConfig,
  handleSaveConfig,
}: ServiceConfigSectionProps) {
  return (
    <>
      <div className="flex items-center justify-between mb-3">
        <h2 className="text-sm font-semibold text-slate-800">Configuration</h2>
        {!editingConfig && isAuth && (
          <button
            data-track="edit_service_config"
            onClick={() => {
              const rl = srv.rate_limit || { strategy: "None", requests_per_second: 0, requests_per_minute: 0 };
              if (!["None", "Fixed Window", "Token Bucket"].includes(rl.strategy)) {
                rl.strategy = "None";
              }
              const rc = srv.retry_config || { strategy: "None", max_retries: 0, backoff_ms: 0, retry_on: [] };
              if (!["None", "Exponential Backoff", "Fixed Delay"].includes(rc.strategy)) {
                rc.strategy = "None";
              }
              const dhMap = srv.default_headers;
              const dhArray = dhMap 
                ? Object.entries(dhMap).map(([k, v]) => ({ key: k, value: v as string }))
                : [];
              setConfigForm({
                rate_limit: rl,
                retry_config: rc,
                default_headers: dhArray,
              });
              setEditingConfig(true);
            }}
            className="p-1 text-slate-400 hover:text-slate-600 transition-colors cursor-pointer"
            title="Edit Configuration"
          >
            <Pencil className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {editingConfig && configForm ? (
        <form 
          onSubmit={handleSaveConfig} 
          className="flex flex-col gap-4"
          toolname="save_service_config"
          tooldescription="Save the rate limiting and retry configuration for the service."
        >
          <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
            <div className="flex flex-col gap-3">
              <h3 className="text-xs font-semibold text-slate-700 uppercase tracking-wider">Rate Limiting</h3>
              <div>
                <label className="block text-xs font-medium text-slate-600 mb-1">Strategy</label>
                <select
                  value={configForm.rate_limit.strategy}
                  onChange={(e) =>
                    setConfigForm({
                      ...configForm,
                      rate_limit: { ...configForm.rate_limit, strategy: e.target.value },
                    })
                  }
                  className="w-full text-sm border border-slate-200 rounded-md py-1.5 px-3 focus:border-blue-500 focus:ring-blue-500"
                >
                  <option value="None">None</option>
                  <option value="Fixed Window">Fixed Window</option>
                  <option value="Token Bucket">Token Bucket</option>
                </select>
              </div>
              {configForm.rate_limit.strategy !== "None" && (
                <div className="grid grid-cols-2 gap-4">
                  <div>
                    <label className="block text-xs font-medium text-slate-600 mb-1">Requests per Second</label>
                    <input
                      type="number"
                      min="0"
                      value={configForm.rate_limit.requests_per_second}
                      onChange={(e) =>
                        setConfigForm({
                          ...configForm,
                          rate_limit: { ...configForm.rate_limit, requests_per_second: parseInt(e.target.value) || 0 },
                        })
                      }
                      className="w-full text-sm border border-slate-200 rounded-md py-1.5 px-3 focus:border-blue-500 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-xs font-medium text-slate-600 mb-1">Requests per Minute</label>
                    <input
                      type="number"
                      min="0"
                      value={configForm.rate_limit.requests_per_minute}
                      onChange={(e) =>
                        setConfigForm({
                          ...configForm,
                          rate_limit: { ...configForm.rate_limit, requests_per_minute: parseInt(e.target.value) || 0 },
                        })
                      }
                      className="w-full text-sm border border-slate-200 rounded-md py-1.5 px-3 focus:border-blue-500 focus:ring-blue-500"
                    />
                  </div>
                </div>
              )}
            </div>
            <div className="flex flex-col gap-3">
              <h3 className="text-xs font-semibold text-slate-700 uppercase tracking-wider">Retry Policy</h3>
              <div>
                <label className="block text-xs font-medium text-slate-600 mb-1">Strategy</label>
                <select
                  value={configForm.retry_config.strategy}
                  onChange={(e) =>
                    setConfigForm({
                      ...configForm,
                      retry_config: { ...configForm.retry_config, strategy: e.target.value },
                    })
                  }
                  className="w-full text-sm border border-slate-200 rounded-md py-1.5 px-3 focus:border-blue-500 focus:ring-blue-500"
                >
                  <option value="None">None</option>
                  <option value="Exponential Backoff">Exponential Backoff</option>
                  <option value="Fixed Interval">Fixed Interval</option>
                </select>
              </div>
              {configForm.retry_config.strategy !== "None" && (
                <>
                  <div>
                    <label className="block text-xs font-medium text-slate-600 mb-1">Max Retries</label>
                    <input
                      type="number"
                      min="0"
                      value={configForm.retry_config.max_retries}
                      onChange={(e) =>
                        setConfigForm({
                          ...configForm,
                          retry_config: { ...configForm.retry_config, max_retries: parseInt(e.target.value) || 0 },
                        })
                      }
                      className="w-full text-sm border border-slate-200 rounded-md py-1.5 px-3 focus:border-blue-500 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-xs font-medium text-slate-600 mb-1">Backoff (ms)</label>
                    <input
                      type="number"
                      min="0"
                      value={configForm.retry_config.backoff_ms}
                      onChange={(e) =>
                        setConfigForm({
                          ...configForm,
                          retry_config: { ...configForm.retry_config, backoff_ms: parseInt(e.target.value) || 0 },
                        })
                      }
                      className="w-full text-sm border border-slate-200 rounded-md py-1.5 px-3 focus:border-blue-500 focus:ring-blue-500"
                    />
                  </div>
                </>
              )}
            </div>
          </div>
          <div className="mt-4 flex flex-col gap-3">
            <div className="flex items-center justify-between">
              <h3 className="text-xs font-semibold text-slate-700 uppercase tracking-wider">Default Headers</h3>
              <button
                type="button"
                onClick={() => {
                  setConfigForm({ 
                    ...configForm, 
                    default_headers: [...configForm.default_headers, { key: "", value: "" }] 
                  });
                }}
                className="text-xs text-blue-600 hover:text-blue-700 font-medium"
              >
                + Add Header
              </button>
            </div>
            {configForm.default_headers.map((header: any, idx: number) => (
              <div key={idx} className="flex gap-2 items-center">
                <input
                  type="text"
                  placeholder="Header-Name"
                  value={header.key}
                  onChange={(e) => {
                    const newHeaders = [...configForm.default_headers];
                    newHeaders[idx] = { ...newHeaders[idx], key: e.target.value };
                    setConfigForm({ ...configForm, default_headers: newHeaders });
                  }}
                  className="w-1/3 text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                />
                <input
                  type="text"
                  placeholder="Value"
                  value={header.value}
                  onChange={(e) => {
                    const newHeaders = [...configForm.default_headers];
                    newHeaders[idx] = { ...newHeaders[idx], value: e.target.value };
                    setConfigForm({ ...configForm, default_headers: newHeaders });
                  }}
                  className="flex-1 text-sm border-slate-200 rounded-md focus:border-blue-500 focus:ring-blue-500"
                />
                <button
                  type="button"
                  onClick={() => {
                    const newHeaders = [...configForm.default_headers];
                    newHeaders.splice(idx, 1);
                    setConfigForm({ ...configForm, default_headers: newHeaders });
                  }}
                  className="text-slate-400 hover:text-red-500 p-1"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
                </button>
              </div>
            ))}
            {configForm.default_headers.length === 0 && (
              <div className="text-xs text-slate-500 italic">No default headers configured.</div>
            )}
          </div>
          <div className="flex justify-end gap-2 mt-2 pt-4 border-t border-slate-100">
            <button
              data-track="cancel_edit_config"
              type="button"
              onClick={() => setEditingConfig(false)}
              className="px-3 py-1.5 text-xs font-medium text-slate-600 hover:text-slate-900 transition-colors cursor-pointer"
            >
              Cancel
            </button>
            <button
              data-track="save_service_config"
              type="submit"
              disabled={savingConfig}
              className="px-3 py-1.5 text-xs font-medium bg-blue-600 text-white rounded-md hover:bg-blue-700 transition-colors disabled:opacity-50 cursor-pointer"
            >
              {savingConfig ? "Saving..." : "Save Configuration"}
            </button>
          </div>
        </form>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-6 text-sm text-slate-600">
          <div>
            <span className="text-slate-400 block mb-1">Rate Limiting</span>
            <span className={`font-medium ${!srv.rate_limit?.strategy || srv.rate_limit.strategy === "None" ? "text-slate-400 italic" : ""}`}>
              {(!srv.rate_limit?.strategy || srv.rate_limit.strategy === "None") ? "Not specified" : srv.rate_limit.strategy}
            </span>
          </div>
          <div>
            <span className="text-slate-400 block mb-1">Retry Policy</span>
            <span className={`font-medium ${!srv.retry_config?.strategy || srv.retry_config.strategy === "None" ? "text-slate-400 italic" : ""}`}>
              {(!srv.retry_config?.strategy || srv.retry_config.strategy === "None") ? "Not specified" : srv.retry_config.strategy}
            </span>
          </div>
          <div className="mt-4 pt-4 border-t border-slate-100">
            <h3 className="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-2">Default Headers</h3>
            {!srv.default_headers || Object.keys(srv.default_headers).length === 0 ? (
              <div className="text-sm text-slate-400 italic">None</div>
            ) : (
              <div className="flex flex-col gap-1">
                {Object.entries(srv.default_headers).map(([k, v], idx) => (
                  <div key={idx} className="flex gap-2 text-sm">
                    <span className="font-medium text-slate-700">{k}:</span>
                    <span className="text-slate-600 truncate">{v as string}</span>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      )}
    </>
  );
}
