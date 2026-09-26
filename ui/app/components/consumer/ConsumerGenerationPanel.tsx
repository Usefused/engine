import type { FormEvent } from "react";
import { AlertTriangle, Check, Copy, Download, Globe2, Info, Server } from "lucide-react";
import { AppOwnerControls } from "~/components/access/AppOwnerControls";
import { McpTransportEndpoints, type McpTransportEndpointData } from "~/components/mcp/McpTransportEndpoints";
import type { AppBuildSelector, AppCreationMode, AppOwningTeam } from "~/lib/app-builder-contract";
import type { ServiceVersion } from "~/lib/api";
import { formatVersion } from "~/lib/format";

// The right-hand "generate" form from the Create-a-consumer page (SDK vs
// MCP server config + submit). Pulled out of routes/integrations.builder.tsx
// verbatim -- same handleGenerate/planAndApplyApp contract, only moved
// so that route isn't carrying this JSX inline alongside the service list.
export interface ConsumerGenerationPanelProps {
  generationMode: AppCreationMode;
  existingApp?: { name: string; bucket: string; owner: string };
  selectedWorkflowCount?: number;
  selectionError?: string;
  selectionPending?: boolean;
  mcpDescription: string;
  setMcpDescription: (value: string) => void;
  intelligentSearch: boolean;
  setIntelligentSearch: (value: boolean) => void;
  ownerTeams: AppOwningTeam[];
  ownerTeamId: string;
  setOwnerTeamId: (id: string) => void;
  availableBuckets: AppBuildSelector[];
  bucketId: string;
  setBucketId: (id: string) => void;
  onCreateCredential: () => void;
  sdkName: string;
  setSdkName: (name: string) => void;
  setIsDuplicate: (value: boolean) => void;
  checkDuplicateSDK: () => void;
  totalSelectedWebhooks: number;
  webhookAttachment: string;
  setWebhookAttachment: (value: string) => void;
  appVersion: string;
  setAppVersion: (value: string) => void;
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
  mcpDeployment: ({ id: string; token: string } & McpTransportEndpointData) | null;
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

/** Names the adapter-specific setup while retaining one shared selection form. */
function GenerationModeHeader({ generationMode }: { generationMode: AppCreationMode }) {
  const title = generationMode === "app" ? "App setup" : generationMode === "mcp" ? "MCP server setup" : generationMode === "api" ? "REST API setup" : "SDK setup";
  const description = generationMode === "app"
    ? "One identity and token for SDK, MCP, and REST delivery."
    : generationMode === "mcp"
    ? "Configure the MCP server your agent connects to."
    : generationMode === "api"
      ? "Configure the Engine REST API your app calls."
      : "Configure the generated package your app imports.";
  return (
    <div className="border-b border-slate-200 bg-slate-50 px-4 py-4">
      <h2 className="mb-1 text-lg font-bold text-slate-900">{title}</h2>
      <p className="text-xs leading-4 text-slate-500">{description}</p>
    </div>
  );
}

/** Captures the immutable app-family name for every delivery adapter. */
function NameField({ generationMode, sdkName, setSdkName, setIsDuplicate, checkDuplicateSDK }: {
  generationMode: AppCreationMode; sdkName: string; setSdkName: (v: string) => void; setIsDuplicate: (v: boolean) => void; checkDuplicateSDK: () => void;
}) {
  // One-company Engines use simple family names; package-manager scopes add no identity value here.
  const placeholder = generationMode === "app" ? "customer-app" : generationMode === "mcp" ? "customer-support" : generationMode === "api" ? "customer-api" : "customer-sdk";
  const label = generationMode === "app" ? "App name" : generationMode === "mcp" ? "Server name" : generationMode === "api" ? "API name" : "SDK name";
  return (
    <div>
      <label className="mb-1 block text-sm font-medium text-slate-700">{label}</label>
      <input
        type="text"
        required
        placeholder={placeholder}
        value={sdkName}
        onChange={e => { setSdkName(e.target.value); setIsDuplicate(false); }}
        onBlur={generationMode !== "mcp" ? checkDuplicateSDK : undefined}
        className="w-full rounded-lg border border-slate-300 bg-slate-50 px-2.5 py-1.5 text-sm transition-all focus:border-[var(--brand-violet)] focus:bg-white focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20"
      />
    </div>
  );
}

/** Keeps an optional webhook bundle name aligned with the compact identity fields. */
function WebhookBundleField({ totalSelectedWebhooks, webhookAttachment, setWebhookAttachment }: {
  totalSelectedWebhooks: number; webhookAttachment: string; setWebhookAttachment: (v: string) => void;
}) {
  if (totalSelectedWebhooks === 0) return null;
  return (
    <div>
      <label className="mb-1 block text-sm font-medium text-slate-700">Webhook bundle</label>
      <input
        type="text"
        required
        placeholder="customer-events"
        value={webhookAttachment}
        onChange={(event) => setWebhookAttachment(event.target.value)}
        className="w-full rounded-lg border border-slate-300 bg-white px-2.5 py-1.5 text-sm"
      />
      <p className="mt-1 text-xs text-slate-500">Name of the team-owned webhook configuration that supplies these events.</p>
    </div>
  );
}

/** Captures immutable version identity and previews collisions for SDK-kind deliveries. */
function VersionField({ generationMode, appVersion, setAppVersion, setIsDuplicate, checkDuplicateSDK, checkingDuplicate, isDuplicate }: {
  generationMode: AppCreationMode; appVersion: string; setAppVersion: (v: string) => void; setIsDuplicate: (v: boolean) => void;
  checkDuplicateSDK: () => void; checkingDuplicate: boolean; isDuplicate: boolean;
}) {
  const isSdkKind = generationMode !== "mcp";
  return (
    <div>
      <label htmlFor="app-version" className="mb-1 flex justify-between text-sm font-medium text-slate-700">
        <span>Version</span>
        {isSdkKind && checkingDuplicate && <span className="text-xs text-slate-400">Checking...</span>}
      </label>
      <input
        id="app-version"
        type="text"
        required
        placeholder="1.0.0"
        value={appVersion}
        onChange={e => { setAppVersion(e.target.value); setIsDuplicate(false); }}
        onBlur={isSdkKind ? checkDuplicateSDK : undefined}
        className="w-full rounded-lg border border-slate-300 bg-slate-50 px-2.5 py-1.5 text-sm transition-all focus:border-[var(--brand-violet)] focus:bg-white focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20"
      />
      {isSdkKind && isDuplicate && (
        <div className="mt-2 p-2 bg-yellow-50 border border-yellow-200 rounded-lg flex items-start gap-2 animate-in fade-in slide-in-from-top-1">
          <div className="text-yellow-600 mt-0.5">
            <AlertTriangle className="w-4 h-4" />
          </div>
          <p className="text-xs text-yellow-800 leading-tight">
            An app with this name and version already exists. Engine will preserve its immutable delivery mode and scope.
          </p>
        </div>
      )}
    </div>
  );
}

/** Offers package language only when app creation will generate an SDK artifact. */
function LanguageSelector({ generationMode, language, setLanguage }: {
  generationMode: AppCreationMode; language: "typescript" | "python"; setLanguage: (v: "typescript" | "python") => void;
}) {
  // Combined delivery generates the same typed package as an SDK-only App.
  if (generationMode !== "sdk" && generationMode !== "app") return null;
  return (
    <div>
      <label htmlFor="app-language" className="mb-1 block text-sm font-medium text-slate-700">Language</label>
      <select
        id="app-language"
        data-track="select_app_language"
        value={language}
        onChange={event => setLanguage(event.target.value as "typescript" | "python")}
        className="w-full rounded-lg border border-slate-300 bg-white px-2.5 py-1.5 text-sm focus:border-[var(--brand-violet)] focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]/20"
      >
        <option value="typescript">TypeScript</option>
        <option value="python">Python</option>
      </select>
    </div>
  );
}

/** Keeps the selected-operation count visible without adding another large form section. */
function SelectedOperationsSummary({ totalSelectedServices, totalSelected }: { totalSelectedServices: number; totalSelected: number }) {
  return (
    <div className="mb-3 flex items-center justify-between">
      <span className="text-sm text-slate-500 font-medium">Selected operations</span>
      <div className="flex items-center gap-3">
        {totalSelectedServices > 10 && (
          <span className="text-xs font-semibold text-rose-600 bg-rose-50 px-2.5 py-1 rounded-full border border-rose-200">
            Max 10 services allowed ({totalSelectedServices} selected)
          </span>
        )}
        <span className="inline-flex items-center justify-center rounded-full bg-[var(--brand-violet-tint)] px-2 py-0.5 text-xs font-bold text-[var(--brand-violet)]">
          {totalSelected}
        </span>
      </div>
    </div>
  );
}

type ConsumerGenerationServiceData = Array<{ service: { id: string; name: string }; serviceVersions: ServiceVersion[] }>;

function findServiceData(serviceId: string, data: ConsumerGenerationServiceData) {
  return data.find(d => d.service.id === serviceId);
}

function defaultVersionId(svcData: ConsumerGenerationServiceData[number] | undefined): string {
  if (!svcData) return "";
  const versions = svcData.serviceVersions;
  return versions && versions.length > 0 ? versions[0].id : "";
}

function versionTagFor(svcData: ConsumerGenerationServiceData[number] | undefined, versionId: string): string {
  if (!svcData) return "";
  const match = svcData.serviceVersions.find(v => v.id === versionId);
  return match ? match.name : "";
}

function unactivatedServiceDisplay(
  serviceId: string,
  data: ConsumerGenerationServiceData,
  versionSelections: Record<string, string>
) {
  const svcData = findServiceData(serviceId, data);
  const serviceName = svcData ? svcData.service.name : serviceId;
  const selectedVersionId = versionSelections[serviceId] || defaultVersionId(svcData);
  const versionTag = versionTagFor(svcData, selectedVersionId);
  return { serviceName, selectedVersionId, versionTag };
}

interface UnactivatedServicesWarningProps {
  unactivatedSelectedServiceIds: string[];
  data: ConsumerGenerationServiceData;
  versionSelections: Record<string, string>;
  onAdded: (serviceId: string) => void;
  AddSelectedServiceToWorkspaceButton: ConsumerGenerationPanelProps["AddSelectedServiceToWorkspaceButton"];
}

function UnactivatedServicesWarning({ unactivatedSelectedServiceIds, data, versionSelections, onAdded, AddSelectedServiceToWorkspaceButton }: UnactivatedServicesWarningProps) {
  if (unactivatedSelectedServiceIds.length === 0) return null;
  return (
    <div className="mb-4 p-3 bg-amber-50 border border-amber-200 rounded-xl animate-in fade-in slide-in-from-top-1">
      <p className="text-xs text-amber-800 leading-snug mb-2.5">
        {unactivatedSelectedServiceIds.length === 1
          ? "This service isn't in your workspace yet. Add it to continue:"
          : "These services aren't in your workspace yet. Add them to continue:"}
      </p>
      <div className="flex flex-wrap gap-2">
        {unactivatedSelectedServiceIds.map(serviceId => {
          const { serviceName, selectedVersionId, versionTag } = unactivatedServiceDisplay(serviceId, data, versionSelections);
          return (
            <AddSelectedServiceToWorkspaceButton
              key={serviceId}
              serviceId={serviceId}
              serviceName={serviceName}
              serviceVersionId={selectedVersionId}
              versionTag={versionTag}
              onAdded={onAdded}
            />
          );
        })}
      </div>
    </div>
  );
}

/** Renders progress and call-to-action copy for the selected delivery adapter. */
function GenerateSubmitButtonContent({ generating, generationMode }: { generating: boolean; generationMode: AppCreationMode }) {
  if (generating) {
    return (
      <>
        <div className="w-5 h-5 border-2 border-white/30 border-t-white rounded-full animate-spin" />
        {generationMode === "mcp" ? "Deploying..." : generationMode === "api" ? "Publishing..." : generationMode === "app" ? "Creating..." : "Generating..."}
      </>
    );
  }
  if (generationMode === "sdk" || generationMode === "app") {
    return (
      <>
        <Download className="w-5 h-5" />
        {generationMode === "app" ? "Create App" : "Create SDK"}
      </>
    );
  }
  // REST publication creates an executable app without implying a package download.
  if (generationMode === "api") {
    return (
      <>
        <Globe2 className="w-5 h-5" />
        Create REST API
      </>
    );
  }
  return (
    <>
      <Server className="w-5 h-5" />
      Create MCP server
    </>
  );
}

/** Submits one plan/apply workflow regardless of the chosen delivery adapter. */
function GenerateSubmitButton({ generating, disabled, generationMode }: { generating: boolean; disabled: boolean; generationMode: AppCreationMode }) {
  return (
    <button
      data-track="generate_sdk_or_mcp"
      type="submit"
      disabled={disabled}
      className="flex w-full items-center justify-center gap-2 rounded-xl bg-slate-950 px-4 py-2.5 font-semibold text-white shadow-sm transition-all hover:bg-slate-800 active:scale-[0.98] disabled:cursor-not-allowed disabled:opacity-50"
    >
      <GenerateSubmitButtonContent generating={generating} generationMode={generationMode} />
    </button>
  );
}

function GenerationProgress({ generating, generateStatus }: { generating: boolean; generateStatus: string }) {
  if (!generating || !generateStatus) return null;
  return (
    <div className="mt-4 flex items-center justify-center gap-2.5 text-sm text-slate-500 animate-in fade-in slide-in-from-top-2 duration-300">
      <div className="flex gap-1 items-center shrink-0">
        <div className="w-1.5 h-1.5 bg-blue-500/80 rounded-full animate-bounce [animation-delay:-0.3s]" />
        <div className="w-1.5 h-1.5 bg-blue-500/80 rounded-full animate-bounce [animation-delay:-0.15s]" />
        <div className="w-1.5 h-1.5 bg-blue-500/80 rounded-full animate-bounce" />
      </div>
      <span className="text-center leading-snug">{generateStatus}</span>
    </div>
  );
}

/** Reports either generated-package or direct-REST success from SDK-kind plan/apply. */
function SdkDeploymentResult({ generationMode, sdkDeployment, sdkTokenCopied, setSdkTokenCopied }: {
  generationMode: AppCreationMode; sdkDeployment: ConsumerGenerationPanelProps["sdkDeployment"]; sdkTokenCopied: boolean; setSdkTokenCopied: (v: boolean) => void;
}) {
  if (!sdkDeployment) return null;
  const isAPI = generationMode === "api";
  return (
    <div className="mt-4 border-t border-slate-200 pt-4 text-sm text-slate-700">
      <div className="flex items-center gap-2 font-semibold text-slate-900">
        <Check className="h-4 w-4 text-emerald-600" /> {generationMode === "app" ? "App ready: SDK, MCP, and REST" : isAPI ? "REST API ready" : "SDK ready"}
      </div>
      <p className="mt-2 text-xs text-slate-500">{generationMode === "app" ? "The package is downloaded, and the same App ID and token work for REST and MCP." : isAPI ? "The API is published on this Engine." : "The package has been downloaded."}</p>
      {sdkDeployment.token && (
        <ExecutionTokenField token={sdkDeployment.token} copied={sdkTokenCopied} onCopy={() => setSdkTokenCopied(true)} />
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
        View {sdkDeployment.name} {formatVersion(sdkDeployment.version)}
      </a>
    </div>
  );
}

/** Shows hosted MCP endpoints while keeping the combined App token in its shared result. */
function McpDeploymentResult({ mcpDeployment, mcpTokenCopied, setMcpTokenCopied, generationMode }: {
  mcpDeployment: ConsumerGenerationPanelProps["mcpDeployment"]; mcpTokenCopied: boolean; setMcpTokenCopied: (v: boolean) => void;
  generationMode: AppCreationMode;
}) {
  if (!mcpDeployment) return null;
  return (
    <div className="mt-4 border-t border-slate-200 pt-4 text-sm text-slate-700">
      <div className="flex items-center gap-2 font-semibold text-slate-900">
        <Check className="h-4 w-4 text-emerald-600" /> MCP server ready
      </div>
      <div className="mt-3">
        <McpTransportEndpoints endpoints={mcpDeployment} />
      </div>
      {generationMode !== "app" && mcpDeployment.token && (
        <ExecutionTokenField token={mcpDeployment.token} copied={mcpTokenCopied} onCopy={() => setMcpTokenCopied(true)} />
      )}
      {generationMode !== "app" && <p className="mt-2 text-xs text-slate-500">This token is displayed only when the server is first created. Store it securely.</p>}
    </div>
  );
}

/** Renders the shared creation controls while adapting delivery-specific fields and outcomes. */
export function ConsumerGenerationPanel(props: ConsumerGenerationPanelProps) {
  const {
    generationMode,
    setIsDuplicate,
    checkDuplicateSDK,
    totalSelectedWebhooks,
    webhookAttachment,
    setWebhookAttachment,
    appVersion,
    setAppVersion,
    checkingDuplicate,
    isDuplicate,
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

  // Pending or conflicting workflow selections cannot submit an older capability set.
  const submitDisabled = generationSubmitDisabled(props);
  // Package language shares a row with version only when creating a new SDK-bearing App.
  const showsLanguage = !props.existingApp && (generationMode === "sdk" || generationMode === "app");

  return (
    /* The stacked layout should not stretch every input across a tablet-width screen. */
    <div className="mx-auto w-full max-w-lg flex-shrink-0 lg:mx-0 lg:w-80 lg:max-w-none">
      <div className="bg-white rounded-2xl border border-slate-200 shadow-xl shadow-slate-200/50 overflow-hidden sticky top-8">
        <GenerationModeHeader generationMode={generationMode} />

        <form
          onSubmit={handleGenerate}
          className="space-y-3 p-4"
          toolname="generate_sdk"
          tooldescription="Create a generated SDK, direct REST API, or MCP server from selected operations. Requires name and version."
        >
          <GenerationIdentity props={props} />
          <WebhookBundleField totalSelectedWebhooks={totalSelectedWebhooks} webhookAttachment={webhookAttachment} setWebhookAttachment={setWebhookAttachment} />
          {/* Let Language consume the remaining form width beside the compact Version field. */}
          <div className={showsLanguage ? "grid w-full grid-cols-[7rem_minmax(0,1fr)] gap-3" : ""}>
            <VersionField
              generationMode={generationMode}
              appVersion={appVersion}
              setAppVersion={setAppVersion}
              setIsDuplicate={setIsDuplicate}
              checkDuplicateSDK={checkDuplicateSDK}
              checkingDuplicate={checkingDuplicate}
              isDuplicate={isDuplicate}
            />
            {/* A successor keeps its family's language, while new SDKs expose it beside Version. */}
            <NewAppLanguage props={props} />
          </div>

          <div className="border-t border-slate-100 pt-3">
            {/* Workflow methods participate in the same operation count and app creation action. */}
            {!!props.selectedWorkflowCount && <p className="mb-3 text-xs text-slate-500">{props.selectedWorkflowCount} selected {/* Match the singular label when only one workflow is chosen. */}workflow{props.selectedWorkflowCount === 1 ? "" : "s"}. Required service versions will be enabled when you create the app.</p>}
            <SelectedOperationsSummary totalSelectedServices={totalSelectedServices} totalSelected={totalSelected} />

            <UnactivatedServicesWarning
              unactivatedSelectedServiceIds={unactivatedSelectedServiceIds}
              data={data}
              versionSelections={versionSelections}
              onAdded={handleServiceAddedToWorkspace}
              AddSelectedServiceToWorkspaceButton={AddSelectedServiceToWorkspaceButton}
            />

            {/* Keep search consent next to the creation action where its effect is decided. */}
            {!props.existingApp && (generationMode === "mcp" || generationMode === "app") && <IntelligentSearchField {...props} />}

            <GenerateSubmitButton generating={generating} disabled={submitDisabled} generationMode={generationMode} />

            <GenerationProgress generating={generating} generateStatus={generateStatus} />

            <SdkDeploymentResult generationMode={generationMode} sdkDeployment={sdkDeployment} sdkTokenCopied={sdkTokenCopied} setSdkTokenCopied={setSdkTokenCopied} />

            <McpDeploymentResult generationMode={generationMode} mcpDeployment={mcpDeployment} mcpTokenCopied={mcpTokenCopied} setMcpTokenCopied={setMcpTokenCopied} />
          </div>
        </form>
      </div>
    </div>
  );
}


/** Collects identity only for new apps; extensions retain Engine-owned family metadata. */
function GenerationIdentity({ props }: { props: ConsumerGenerationPanelProps }) {
  // Existing apps expose their preserved binding instead of editable ownership and credential controls.
  if (props.existingApp) return <dl className="space-y-3 text-sm">
    <div><dt className="text-xs text-slate-500">App</dt><dd className="mt-1 font-medium text-slate-900">{props.existingApp.name}</dd></div>
    <div><dt className="text-xs text-slate-500">Credentials</dt><dd className="mt-1 text-slate-700">{props.existingApp.bucket}</dd></div>
  </dl>;
  return <>
    <AppOwnerControls ownerTeams={props.ownerTeams} ownerTeamId={props.ownerTeamId} buckets={props.availableBuckets} bucketId={props.bucketId} onOwnerTeamChange={props.setOwnerTeamId} onBucketChange={props.setBucketId} onCreateCredential={props.onCreateCredential} />
    <NameField {...props} />
    {/* MCP delivery needs an authored description for discovery. */}
    {(props.generationMode === "mcp" || props.generationMode === "app") && <McpDescriptionField {...props} />}
  </>;
}

/** Language selection is available only while establishing a new SDK family. */
function NewAppLanguage({ props }: { props: ConsumerGenerationPanelProps }) {
  // A successor cannot change its family's pinned language.
  if (props.existingApp) return null;
  return <LanguageSelector {...props} />;
}

/** One shared gate prevents pending, empty, or conflicting capability selections from being submitted. */
function generationSubmitDisabled(props: ConsumerGenerationPanelProps) {
  return [props.selectionError, props.selectionPending, props.generating, props.totalSelected === 0, !props.sdkName.trim(), !props.bucketId, props.totalSelectedServices > 10, props.unactivatedSelectedServiceIds.length > 0].some(Boolean);
}

/** Presents one-time runtime credentials only after the shared app lifecycle completes. */
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

/** Names the authored description for the delivery the user is creating. */
function McpDescriptionField(props: ConsumerGenerationPanelProps) {
  // A combined App has one user-facing identity, while MCP-only creation describes its server.
  const isApp = props.generationMode === "app";
  return <div>
    <label className="block text-sm font-medium text-slate-700">{isApp ? "App description" : "Server description"}
      {/* Preserve a description for MCP discovery in both creation modes. */}
      <textarea required rows={2} maxLength={1024} value={props.mcpDescription} onChange={event => props.setMcpDescription(event.target.value)}
        placeholder={isApp ? "Describe the services and tasks this app enables." : "Describe the services and tasks this server enables."}
        className="mt-1 w-full resize-y rounded-lg border border-slate-200 px-2.5 py-1.5 text-sm" />
    </label>
  </div>;
}

/** Shows optional Jev processing consent beside the final creation action. */
function IntelligentSearchField(props: ConsumerGenerationPanelProps) {
  return <div className="space-y-2 pb-3">
    <label className="flex items-center gap-2 text-sm text-slate-700">
      {/* Only an owner-selected checkbox adds the remote search provider to the config. */}
      <input type="checkbox" checked={props.intelligentSearch} onChange={event => props.setIntelligentSearch(event.target.checked)} aria-describedby="jev-disclosure"
        className="h-4 w-4 shrink-0 accent-[var(--brand-violet)]" />
      <span>Intelligent search with Jev</span>
    </label>
    <div id="jev-disclosure" className="flex items-start gap-2 rounded-lg border border-violet-100 bg-violet-50/60 p-2 text-xs leading-4 text-slate-600">
      <Info aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0 text-violet-500" />
      <p>Search intent, operation names, and descriptions are sent to Jev via Fused Registry. No extra API key needed.</p>
    </div>
  </div>;
}
