import type { FormEvent } from "react";
import { AlertTriangle, Check, Copy, Download, Server } from "lucide-react";
import { ArtifactOwnerControls } from "~/components/access/ArtifactOwnerControls";
import type { ArtifactBuildSelector, ArtifactOwningTeam } from "~/lib/artifact-builder-contract";
import type { ServiceVersion } from "~/lib/api";

// The right-hand "generate" form from the Create-a-consumer page (SDK vs
// MCP server config + submit). Pulled out of routes/integrations.sdk-builder.tsx
// verbatim -- same handleGenerate/planAndApplyArtifact contract, only moved
// so that route isn't carrying this JSX inline alongside the service list.
export interface ConsumerGenerationPanelProps {
  generationMode: "sdk" | "mcp";
  ownerTeams: ArtifactOwningTeam[];
  ownerTeamId: string;
  setOwnerTeamId: (id: string) => void;
  availableBuckets: ArtifactBuildSelector[];
  bucketId: string;
  setBucketId: (id: string) => void;
  sdkName: string;
  setSdkName: (name: string) => void;
  setIsDuplicate: (value: boolean) => void;
  checkDuplicateSDK: () => void;
  totalSelectedWebhooks: number;
  webhookAttachment: string;
  setWebhookAttachment: (value: string) => void;
  artifactVersion: string;
  setArtifactVersion: (value: string) => void;
  checkingDuplicate: boolean;
  isDuplicate: boolean;
  language: "typescript" | "python";
  setLanguage: (value: "typescript" | "python") => void;
  totalSelectedServices: number;
  totalSelected: number;
  unactivatedSelectedServiceIds: string[];
  data: Array<{ service: { id: string; name: string }; serviceVersions: ServiceVersion[] }>;
  versionSelections: Record<string, string>;
  handleServiceAddedToWorkspace: (serviceId: string) => void;
  handleGenerate: (e: FormEvent) => void;
  generating: boolean;
  generateStatus: string;
  sdkDeployment: { id: string; name: string; version: string; token: string } | null;
  sdkTokenCopied: boolean;
  setSdkTokenCopied: (value: boolean) => void;
  mcpDeployment: { id: string; url: string; token: string } | null;
  mcpTokenCopied: boolean;
  setMcpTokenCopied: (value: boolean) => void;
  AddSelectedServiceToWorkspaceButton: React.ComponentType<{
    serviceId: string;
    serviceName: string;
    versionTag?: string;
    serviceVersionId?: string;
    onAdded: (serviceId: string) => void;
  }>;
}

export function ConsumerGenerationPanel(props: ConsumerGenerationPanelProps) {
  const {
    generationMode,
    ownerTeams,
    ownerTeamId,
    setOwnerTeamId,
    availableBuckets,
    bucketId,
    setBucketId,
    sdkName,
    setSdkName,
    setIsDuplicate,
    checkDuplicateSDK,
    totalSelectedWebhooks,
    webhookAttachment,
    setWebhookAttachment,
    artifactVersion,
    setArtifactVersion,
    checkingDuplicate,
    isDuplicate,
    language,
    setLanguage,
    totalSelectedServices,
    totalSelected,
    unactivatedSelectedServiceIds,
    data,
    versionSelections,
    handleServiceAddedToWorkspace,
    handleGenerate,
    generating,
    generateStatus,
    sdkDeployment,
    sdkTokenCopied,
    setSdkTokenCopied,
    mcpDeployment,
    mcpTokenCopied,
    setMcpTokenCopied,
    AddSelectedServiceToWorkspaceButton,
  } = props;

  return (
    <div className="w-full lg:w-80 flex-shrink-0">
      <div className="bg-white rounded-2xl border border-slate-200 shadow-xl shadow-slate-200/50 overflow-hidden sticky top-8">
        <div className="border-b border-slate-200 bg-slate-50 p-6">
          <h2 className="text-xl font-bold text-slate-900 mb-2">{generationMode === "mcp" ? "MCP server setup" : "App setup"}</h2>
          <p className="text-sm text-slate-500">
            {generationMode === "mcp" ? "Configure the MCP server your agent connects to." : "Configure how your app consumes the selected operations."}
          </p>
        </div>

        <form
          onSubmit={handleGenerate}
          className="p-6 space-y-5"
          toolname="generate_sdk"
          tooldescription="Generate a native SDK or MCP server based on the selected endpoints. Requires name and version."
        >
          <ArtifactOwnerControls ownerTeams={ownerTeams} ownerTeamId={ownerTeamId} buckets={availableBuckets} bucketId={bucketId} onOwnerTeamChange={setOwnerTeamId} onBucketChange={setBucketId} />
          <div>
            <label className="block text-sm font-medium text-slate-700 mb-1.5">{generationMode === "mcp" ? "Server name" : "App name"}</label>
            <input
              type="text"
              required
              placeholder={generationMode === "mcp" ? "customer-support" : "@myorg/custom-sdk"}
              value={sdkName}
              onChange={e => {
                setSdkName(e.target.value);
                setIsDuplicate(false);
              }}
              onBlur={generationMode === "sdk" ? checkDuplicateSDK : undefined}
              className="w-full px-3 py-2.5 rounded-lg border border-slate-300 text-sm focus:outline-none focus:border-[var(--brand-violet)] focus:ring-2 focus:ring-[var(--brand-violet)]/20 transition-all bg-slate-50 focus:bg-white"
            />
          </div>

          {totalSelectedWebhooks > 0 && <div>
            <label className="block text-sm font-medium text-slate-700 mb-1.5">Webhook bundle</label>
            <input
              type="text"
              required
              placeholder="customer-events"
              value={webhookAttachment}
              onChange={(event) => setWebhookAttachment(event.target.value)}
              className="w-full px-3 py-2.5 rounded-lg border border-slate-300 text-sm bg-white"
            />
            <p className="mt-1.5 text-xs text-slate-500">Name of the team-owned webhook configuration that supplies these events.</p>
          </div>}

          <div>
            <label className="block text-sm font-medium text-slate-700 mb-1.5 flex justify-between">
              <span>Version</span>
              {generationMode === "sdk" && checkingDuplicate && <span className="text-xs text-slate-400">Checking...</span>}
            </label>
            <input
              type="text"
              required
              placeholder="1.0.0"
              value={artifactVersion}
              onChange={e => {
                setArtifactVersion(e.target.value);
                setIsDuplicate(false);
              }}
              onBlur={generationMode === "sdk" ? checkDuplicateSDK : undefined}
              className="w-full px-3 py-2.5 rounded-lg border border-slate-300 text-sm focus:outline-none focus:border-[var(--brand-violet)] focus:ring-2 focus:ring-[var(--brand-violet)]/20 transition-all bg-slate-50 focus:bg-white"
            />
            {generationMode === "sdk" && isDuplicate && (
              <div className="mt-2 p-2 bg-yellow-50 border border-yellow-200 rounded-lg flex items-start gap-2 animate-in fade-in slide-in-from-top-1">
                <div className="text-yellow-600 mt-0.5">
                  <AlertTriangle className="w-4 h-4" />
                </div>
                <p className="text-xs text-yellow-800 leading-tight">
                  An SDK with this name and version already exists. Generating it again will overwrite the existing package file.
                </p>
              </div>
            )}
          </div>

          <div className="pt-2">
            {generationMode === "sdk" && <><label className="block text-sm font-medium text-slate-700 mb-2">Language</label>
            <div className="grid grid-cols-2 gap-3">
              <button
                data-track="select_language_typescript"
                type="button"
                onClick={() => setLanguage("typescript")}
                className={`flex items-center justify-center py-2 px-3 rounded-lg border transition-all ${
                  language === "typescript"
                    ? "border-[var(--brand-violet)] bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]"
                    : "border-slate-200 bg-white text-slate-500 hover:border-slate-300"
                }`}
              >
                <span className="text-sm font-medium">TypeScript</span>
              </button>
              <button
                data-track="select_language_python"
                type="button"
                onClick={() => setLanguage("python")}
                className={`flex items-center justify-center py-2 px-3 rounded-lg border transition-all ${
                  language === "python"
                    ? "border-[var(--brand-violet)] bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]"
                    : "border-slate-200 bg-white text-slate-500 hover:border-slate-300"
                }`}
              >
                <span className="text-sm font-medium">Python</span>
              </button>
            </div>
            </>}
          </div>

          <div className="pt-4 border-t border-slate-100">
            <div className="flex items-center justify-between mb-6">
              <span className="text-sm text-slate-500 font-medium">Selected operations</span>
              <div className="flex items-center gap-3">
                {totalSelectedServices > 10 && (
                  <span className="text-xs font-semibold text-rose-600 bg-rose-50 px-2.5 py-1 rounded-full border border-rose-200">
                    Max 10 services allowed ({totalSelectedServices} selected)
                  </span>
                )}
                <span className="inline-flex items-center justify-center px-2.5 py-1 rounded-full text-xs font-bold bg-[var(--brand-violet-tint)] text-[var(--brand-violet)]">
                  {totalSelected}
                </span>
              </div>
            </div>

            {unactivatedSelectedServiceIds.length > 0 && (
              <div className="mb-4 p-3 bg-amber-50 border border-amber-200 rounded-xl animate-in fade-in slide-in-from-top-1">
                <p className="text-xs text-amber-800 leading-snug mb-2.5">
                  {unactivatedSelectedServiceIds.length === 1
                    ? "This service isn't in your workspace yet. Add it to continue:"
                    : "These services aren't in your workspace yet. Add them to continue:"}
                </p>
                <div className="flex flex-wrap gap-2">
                  {unactivatedSelectedServiceIds.map(serviceId => {
                    const svcData = data.find(d => d.service.id === serviceId);
                    const serviceName = svcData?.service.name || serviceId;
                    const selectedVersionId = versionSelections[serviceId] || svcData?.serviceVersions?.[0]?.id || "";
                    const selectedVersion = svcData?.serviceVersions?.find((v) => v.id === selectedVersionId);
                    return (
                      <AddSelectedServiceToWorkspaceButton
                        key={serviceId}
                        serviceId={serviceId}
                        serviceName={serviceName}
                        serviceVersionId={selectedVersionId}
                        versionTag={selectedVersion?.name || ""}
                        onAdded={handleServiceAddedToWorkspace}
                      />
                    );
                  })}
                </div>
              </div>
            )}

            <button
              data-track="generate_sdk_or_mcp"
              type="submit"
              disabled={generating || totalSelected === 0 || !sdkName.trim() || !bucketId || totalSelectedServices > 10 || unactivatedSelectedServiceIds.length > 0}
              className="w-full py-3 px-4 bg-slate-950 hover:bg-slate-800 disabled:opacity-50 disabled:cursor-not-allowed text-white font-semibold rounded-xl shadow-sm transition-all flex items-center justify-center gap-2 active:scale-[0.98]"
            >
              {generating ? (
                <>
                  <div className="w-5 h-5 border-2 border-white/30 border-t-white rounded-full animate-spin" />
                  {generationMode === "mcp" ? "Deploying..." : "Generating..."}
                </>
              ) : generationMode === 'sdk' ? (
                <>
                  <Download className="w-5 h-5" />
                  Create app
                </>
              ) : (
                <>
                  <Server className="w-5 h-5" />
                  Create MCP server
                </>
              )}
            </button>

            {generating && generateStatus && (
              <div className="mt-4 flex items-center justify-center gap-2.5 text-sm text-slate-500 animate-in fade-in slide-in-from-top-2 duration-300">
                <div className="flex gap-1 items-center shrink-0">
                  <div className="w-1.5 h-1.5 bg-blue-500/80 rounded-full animate-bounce [animation-delay:-0.3s]" />
                  <div className="w-1.5 h-1.5 bg-blue-500/80 rounded-full animate-bounce [animation-delay:-0.15s]" />
                  <div className="w-1.5 h-1.5 bg-blue-500/80 rounded-full animate-bounce" />
                </div>
                <span className="text-center leading-snug">{generateStatus}</span>
              </div>
            )}

            {sdkDeployment && (
              <div className="mt-4 border-t border-slate-200 pt-4 text-sm text-slate-700">
                <div className="flex items-center gap-2 font-semibold text-slate-900">
                  <Check className="h-4 w-4 text-emerald-600" /> App ready
                </div>
                <p className="mt-2 text-xs text-slate-500">The package has been downloaded.</p>
                {sdkDeployment.token && (
                  <ExecutionTokenField
                    token={sdkDeployment.token}
                    copied={sdkTokenCopied}
                    onCopy={() => setSdkTokenCopied(true)}
                  />
                )}
                {sdkDeployment.token ? (
                  <p className="mt-2 text-xs text-slate-500">This token is displayed once. Store it securely; the CLI can issue a replacement later.</p>
                ) : (
                  <p className="mt-2 text-xs text-slate-500">This existing app kept its current execution tokens.</p>
                )}
                <a
                  href={`/integrations/sdks/${sdkDeployment.id}`}
                  className="mt-3 inline-flex text-xs font-medium text-blue-700 hover:underline"
                >
                  View {sdkDeployment.name} v{sdkDeployment.version}
                </a>
              </div>
            )}

            {mcpDeployment && (
              <div className="mt-4 border-t border-slate-200 pt-4 text-sm text-slate-700">
                <div className="flex items-center gap-2 font-semibold text-slate-900">
                  <Check className="h-4 w-4 text-emerald-600" /> MCP server ready
                </div>
                <p className="mt-2 break-all font-mono text-xs">{mcpDeployment.url}</p>
                {mcpDeployment.token && (
                  <ExecutionTokenField
                    token={mcpDeployment.token}
                    copied={mcpTokenCopied}
                    onCopy={() => setMcpTokenCopied(true)}
                  />
                )}
                <p className="mt-2 text-xs text-slate-500">This token is displayed only when the server is first created. Store it securely.</p>
              </div>
            )}
          </div>
        </form>
      </div>
    </div>
  );
}

function ExecutionTokenField({ token, copied, onCopy }: { token: string; copied: boolean; onCopy: () => void }) {
  return (
    <div className="mt-3 flex items-center gap-2">
      <code className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap rounded border border-slate-200 bg-slate-50 px-2 py-1.5 text-xs">{token}</code>
      <button
        type="button"
        title="Copy execution token"
        aria-label="Copy execution token"
        className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded border border-slate-200 bg-white hover:bg-slate-50"
        onClick={async () => {
          await navigator.clipboard.writeText(token);
          onCopy();
        }}
      >
        {copied ? <Check className="h-4 w-4 text-emerald-600" /> : <Copy className="h-4 w-4" />}
      </button>
    </div>
  );
}
