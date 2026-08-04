import { UploadCloud } from "lucide-react";
import type { FormEvent } from "react";
import type { SpecificationImportPlan } from "~/lib/api";

export interface DefineServiceDrawerProps {
  isAuth: boolean;
  /** Backdrop click / X button -- just hides the drawer, keeps form state. */
  onClose: () => void;
  /** Cancel button -- hides the drawer AND resets the form fields. */
  onCancel: () => void;
  importPlan: SpecificationImportPlan | null;
  newName: string;
  setNewName: (value: string) => void;
  isSlugManuallyEdited: boolean;
  setIsSlugManuallyEdited: (value: boolean) => void;
  newSlug: string;
  setNewSlug: (value: string) => void;
  requireVersion: boolean;
  setRequireVersion: (value: boolean) => void;
  importMethod: "openapi" | "docs";
  setImportMethod: (value: "openapi" | "docs") => void;
  newVersion: string;
  setNewVersion: (value: string) => void;
  sourceType: "url" | "text";
  setSourceType: (value: "url" | "text") => void;
  newUrl: string;
  setNewUrl: (value: string) => void;
  newContent: string;
  setNewContent: (value: string) => void;
  fileName: string;
  setFileName: (value: string) => void;
  starting: boolean;
  startError: string;
  setStartError: (value: string) => void;
  handleStart: (e: FormEvent) => void;
  handleChangeImportSource: () => void;
}

// The "Define a service" drawer: same import/plan/apply contract as before
// (api.integrations.planImport / applyImport / start, all owned by the
// parent route), just pulled out of integrations._index.tsx so that route
// isn't carrying ~300 lines of drawer JSX inline.
export function DefineServiceDrawer({
  isAuth,
  onClose,
  onCancel,
  importPlan,
  newName,
  setNewName,
  isSlugManuallyEdited,
  setIsSlugManuallyEdited,
  newSlug,
  setNewSlug,
  requireVersion,
  setRequireVersion,
  importMethod,
  setImportMethod,
  newVersion,
  setNewVersion,
  sourceType,
  setSourceType,
  newUrl,
  setNewUrl,
  newContent,
  setNewContent,
  fileName,
  setFileName,
  starting,
  startError,
  setStartError,
  handleStart,
  handleChangeImportSource,
}: DefineServiceDrawerProps) {
  return (
    <>
      <div
        className="fixed inset-0 bg-slate-900/20 z-40 transition-opacity"
        onClick={onClose}
      />
      <div className="fixed inset-y-0 right-0 w-full md:w-[600px] bg-white shadow-2xl z-50 overflow-y-auto transform transition-transform border-l border-slate-200 flex flex-col">
        <div className="p-6 border-b border-slate-100 sticky top-0 bg-white/90 backdrop-blur z-10">
          <div className="flex items-center justify-between">
            <h2 className="text-lg font-semibold text-slate-900">Define a service</h2>
            <button
              data-track="close_new_service_panel"
              onClick={onClose}
              className="p-2 text-slate-400 hover:text-slate-600 hover:bg-slate-100 rounded-full transition-colors cursor-pointer"
            >
              ✕
            </button>
          </div>
          <p className="text-sm text-slate-500 mt-1">Start from a spec, docs, or manual service details. Fused turns it into reusable operations.</p>
        </div>

        <div className="p-6">
          {!isAuth ? (
            <div className="flex flex-col items-center justify-center py-12 text-center gap-4">
              <p className="text-slate-600 text-sm">You need to be logged in to create a service.</p>
              <a
                href="/login"
                className="px-5 py-2 bg-slate-950 hover:bg-slate-800 text-white text-sm font-medium rounded-lg transition-colors"
              >
                Log in to get started
              </a>
            </div>
          ) : (
          <form
            onSubmit={handleStart}
            className="space-y-6"
            toolname="create_new_service"
            tooldescription="Create a new service by importing an OpenAPI spec or Docs URL."
          >
            <div>
              <label className="block text-sm font-medium text-slate-700 mb-2">
                Service name
              </label>
              <input
                type="text"
                value={newName}
                onChange={(e) => {
                  const val = e.target.value;
                  setNewName(val);
                  if (!isSlugManuallyEdited) {
                    setNewSlug(val.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, ''));
                  }
                }}
                placeholder="Stripe"
                className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20 focus:border-[var(--brand-violet)] transition-all"
                autoFocus
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-slate-700 mb-2">
                Service slug
              </label>
              <input
                type="text"
                value={newSlug}
                onChange={(e) => {
                  setNewSlug(e.target.value);
                  setIsSlugManuallyEdited(true);
                }}
                placeholder="stripe"
                className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20 focus:border-[var(--brand-violet)] transition-all"
              />
            </div>
            {(requireVersion || importMethod === "docs" || importPlan) && (
              <div>
                <label className="block text-sm font-medium text-slate-700 mb-2">
                  Service version
                  {!importPlan && (
                    <span className="text-red-500 font-normal"> * (required)</span>
                  )}
                </label>
                <input
                  type="text"
                  value={newVersion}
                  onChange={(e) => setNewVersion(e.target.value)}
                  readOnly={Boolean(importPlan)}
                  placeholder="1.0"
                  className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20 focus:border-[var(--brand-violet)] transition-all read-only:bg-slate-50 read-only:text-slate-600"
                />
              </div>
            )}
            <div>
              {importPlan ? (
                <div className="space-y-4">
                  <div className="border-y border-slate-200 py-4 space-y-3">
                    <div className="grid grid-cols-3 gap-3 pt-1 text-center">
                      <div><div className="text-lg font-semibold text-emerald-700">{importPlan.diff.added}</div><div className="text-xs text-slate-500">Added</div></div>
                      <div><div className="text-lg font-semibold text-amber-700">{importPlan.diff.changed}</div><div className="text-xs text-slate-500">Changed</div></div>
                      <div><div className="text-lg font-semibold text-red-700">{importPlan.diff.removed}</div><div className="text-xs text-slate-500">Removed</div></div>
                    </div>
                  </div>
                  <button
                    type="button"
                    onClick={handleChangeImportSource}
                    disabled={starting}
                    className="text-sm font-medium text-[var(--brand-violet)] hover:text-[var(--brand-violet-hover)] disabled:cursor-not-allowed disabled:text-slate-400"
                  >
                    Change source
                  </button>
                </div>
              ) : (
                <>
                  <label className="block text-sm font-medium text-slate-700 mb-2">
                    Source
                  </label>
                  <div className="flex gap-4 mb-1 border-b border-slate-200">
                    <button
                      data-track="select_import_method_openapi"
                      type="button"
                      onClick={() => setImportMethod("openapi")}
                      className={`pb-2 text-sm font-medium ${importMethod === "openapi" ? "text-[var(--brand-violet)] border-b-2 border-[var(--brand-violet)]" : "text-slate-500 hover:text-slate-700"} cursor-pointer`}
                    >
                      Spec file or URL
                    </button>
                    <button
                      data-track="select_import_method_docs"
                      type="button"
                      onClick={() => setImportMethod("docs")}
                      className={`pb-2 text-sm font-medium ${importMethod === "docs" ? "text-[var(--brand-violet)] border-b-2 border-[var(--brand-violet)]" : "text-slate-500 hover:text-slate-700"} cursor-pointer`}
                    >
                      Docs URL
                      <span className="ml-1.5 inline-flex items-center px-1.5 py-0.5 rounded text-[9px] font-semibold bg-amber-50 text-amber-700 border border-amber-200 uppercase tracking-wider">
                        Experimental
                      </span>
                    </button>
                  </div>
                  <p className="text-xs text-slate-500 mb-4">
                    {importMethod === "openapi"
                      ? "Supports OpenAPI, GraphQL, AsyncAPI, and Postman collections."
                      : "Points at your public docs site -- Fused reads it and proposes operations to extract."}
                  </p>

                  {importMethod === "openapi" && (
                    <>
                      <div className="flex bg-slate-100 p-1 rounded-lg mb-6 w-fit">
                        <button
                          data-track="select_source_type_url"
                          type="button"
                          onClick={() => setSourceType("url")}
                          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${sourceType === "url" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
                        >
                          Provide URL
                        </button>
                        <button
                          data-track="select_source_type_file"
                          type="button"
                          onClick={() => setSourceType("text")}
                          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all ${sourceType === "text" ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700 hover:bg-slate-200/50"} cursor-pointer`}
                        >
                          Upload File
                        </button>
                      </div>

                      {sourceType === "url" ? (
                        <div>
                          <label className="block text-sm font-medium text-slate-700 mb-1">
                            {"Schema / Collection URL"}
                          </label>
                          <p className="text-xs text-slate-500 mb-3">Note: Postman Collection support is currently experimental.</p>
                          <input
                            type="url"
                            value={newUrl}
                            onChange={(e) => {
                              setNewUrl(e.target.value);
                              setRequireVersion(false);
                              setNewVersion("");
                            }}
                            placeholder="https://api.example.com/openapi.json"
                            className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20 focus:border-[var(--brand-violet)] transition-all"
                          />
                        </div>
                      ) : (
                        <div>
                          <label className="block text-sm font-medium text-slate-700 mb-1">
                            {"Upload Schema / Collection File"}
                          </label>
                          <p className="text-xs text-slate-500 mb-3">Note: Postman Collection support is currently experimental.</p>
                          <div className="flex flex-col gap-2">
                            <div className="relative border-2 border-dashed border-slate-300 rounded-xl p-8 text-center hover:bg-slate-50 hover:border-[var(--brand-violet)] transition-all group">
                              <input
                                type="file"
                                className="absolute inset-0 w-full h-full opacity-0 cursor-pointer"
                                onChange={(e) => {
                                  const file = e.target.files?.[0];
                                  if (!file) {
                                    setNewContent("");
                                    setFileName("");
                                    return;
                                  }
                                  const ext = file.name.split('.').pop()?.toLowerCase();
                                  if (!['json', 'yaml', 'yml', 'graphql'].includes(ext || '')) {
                                    setStartError("Only JSON, YAML, or GraphQL files are supported.");
                                    setNewContent("");
                                    setFileName("");
                                    e.target.value = '';
                                    return;
                                  }
                                  if (file.size > 5 * 1024 * 1024) {
                                    setStartError("File is too large. Maximum size is 5MB.");
                                    setNewContent("");
                                    setFileName("");
                                    e.target.value = '';
                                    return;
                                  }
                                  setStartError("");
                                  setFileName(file.name);
                                  const reader = new FileReader();
                                  reader.onload = (ev) => {
                                    const text = ev.target?.result as string;
                                    setNewContent(text);
                                    setRequireVersion(false);
                                    setNewVersion("");
                                  };
                                  reader.readAsText(file);
                                }}
                              />
                              <div className="flex flex-col items-center gap-2 pointer-events-none">
                                <UploadCloud className="w-8 h-8 text-slate-400 group-hover:text-[var(--brand-violet)] transition-colors" />
                                <p className="text-sm font-medium text-slate-700">Click to upload or drag and drop</p>
                                <p className="text-xs text-slate-500">JSON, YAML, or GraphQL (max 5MB)</p>
                              </div>
                            </div>
                            {fileName && (
                              <p className="text-sm text-green-600 font-medium">✓ Loaded {fileName}</p>
                            )}
                          </div>
                        </div>
                      )}
                    </>
                  )}

                  {importMethod === "docs" && (
                    <div className="space-y-4">
                      <div>
                        <label className="block text-sm font-medium text-slate-700 mb-2">
                          Service Docs URL
                        </label>
                        <input
                          type="url"
                          value={newUrl}
                          onChange={(e) => setNewUrl(e.target.value)}
                          placeholder="https://stripe.com/docs/api"
                          className="w-full px-4 py-3 rounded-lg border border-slate-300 text-[15px] focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20 focus:border-[var(--brand-violet)] transition-all"
                        />
                      </div>
                    </div>
                  )}
                </>
              )}
            </div>
            <div>
              {startError && (
                <p className="text-sm text-red-600 bg-red-50 border border-red-100 p-3 rounded-lg">
                  {startError}
                </p>
              )}
              <div className="pt-4 flex justify-end gap-3">
                <button
                  data-track="cancel_new_service"
                  type="button"
                  onClick={onCancel}
                  className="px-4 py-2 text-sm font-medium text-slate-600 hover:text-slate-800 cursor-pointer"
                >
                  Cancel
                </button>
                <button
                  data-track="submit_new_service"
                  type="submit"
                  disabled={starting || !newName.trim() || (!importPlan && requireVersion && !newVersion.trim()) || (importMethod === "docs" && (!newUrl.trim() || !newVersion.trim())) || (importMethod === "openapi" && !importPlan && sourceType === "url" && !newUrl.trim()) || (importMethod === "openapi" && !importPlan && sourceType === "text" && !newContent.trim())}
                  className="px-6 py-2 bg-slate-950 hover:bg-slate-800 disabled:opacity-50 text-white font-medium rounded-lg transition-colors flex justify-center items-center gap-2"
                >
                  {starting
                    ? "Processing..."
                    : importMethod === "openapi"
                      ? importPlan ? "Add service to workspace" : "Preview service definition"
                      : "Discover operations"}
              </button>
            </div>
          </div>
        </form>
          )}
      </div>
      </div>
    </>
  );
}
