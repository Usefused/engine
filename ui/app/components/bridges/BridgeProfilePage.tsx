import { useEffect, useState } from "react";
import { Link, useNavigate } from "@remix-run/react";
import { ArrowLeft, Cable, ChevronDown, ChevronRight, RefreshCw, Search } from "lucide-react";
import { api, type BridgeInstallation, type BridgeManifest } from "~/lib/api";
import { EndpointRow } from "~/components/EndpointRow";
import { ServiceCatalogHeader } from "~/components/integration-details/ServiceCatalogHeader";
import { Select } from "~/components/forms/Select";
import { bridgeSchemaSummary, bridgeUsage, groupBridgeOperations } from "~/lib/bridge-presentation";
import { OperationDetailsHeader, OperationCodeExample, OperationDetailsPanel, OperationDetailsBody, OperationSummary, OperationTechnicalDetails, OperationSection, OperationSchemaDisclosure } from "~/components/OperationDetailsParts";
import { SchemaViewer, type JsonSchemaNode } from "~/components/SchemaViewer";

type Operation = BridgeManifest["document"]["operations"][number];
type Contract = { kind: string; input_schema?: JsonSchemaNode | boolean; output_schema?: JsonSchemaNode | boolean };

/** Read the accepted contract defensively; invalid JSON must not invent operation behavior. */
function readContract(operation: Operation): Contract | null {
  try {
    const value = JSON.parse(operation.contract_json);
    // Only the admitted Bridge contract shape may supply kind and schema labels.
    if (!value || typeof value !== "object" || !["read", "command", "event"].includes(value.kind)) return null;
    return value as Contract;
  } catch {
    return null;
  }
}

/** Give each Bridge profile the same page-to-operation-inspector flow as service details. */
export function BridgeProfilePage({ pairing, profile }: { pairing: string; profile: string }) {
  const navigate = useNavigate();
  const [installation, setInstallation] = useState<BridgeInstallation | null>(null);
  const [manifest, setManifest] = useState<BridgeManifest | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState("all");
  const [selected, setSelected] = useState<Operation | null>(null);
  useEffect(/** Bind every response to the profile and refresh that requested it. */ () => {
    let active = true;
    setLoading(true); setError("");
    Promise.all([api.bridges.list(), api.bridges.manifests(pairing)])
      .then(/** A stale page must not replace another profile's accepted manifest. */ ([list, catalogue]) => {
        // Navigation invalidates ownership of both requests.
        if (!active) return;
        setInstallation(list.bridges.find(/** Use exact pairing identity rather than a display name. */ item => item.pairing_id === pairing) ?? null);
        setManifest(catalogue.manifests.find(/** Retained offline profiles remain inspectable by exact identity. */ item => item.profile_id === profile) ?? null);
      }).catch(/** Keep unavailable reads distinct from removed profiles. */ () => {
        // Only the current view may display its request failure.
        if (active) setError("Unable to load this Bridge profile. Refresh to try again.");
      }).finally(/** Settle only the request still owned by this page. */ () => {
        // A replaced request owns its own loading state.
        if (active) setLoading(false);
      });
    return /** Prevent late responses from updating an unmounted profile. */ () => { active = false; };
  }, [pairing, profile, revision]);
  /** Refresh the accepted manifest only after closing an inspector pinned to the old one. */
  function refresh() { setSelected(null); setRevision(/** Use the latest refresh sequence. */ value => value + 1); }
  const operations = manifest?.document.operations ?? [];
  // Bridge kinds use the service type-filter pattern without pretending to be HTTP methods.
  const groups = groupBridgeOperations(operations.filter(/** Keep unknown contracts visible under All types. */ operation => kind === "all" || readContract(operation)?.kind === kind), query);
  // Keep the current route visible while inventory loads or a profile has been removed.
  const profiles = Array.from(new Set([profile, ...(installation?.profile_ids ?? [])]));
  /** Navigation resets the keyed page so selection and pending reads cannot leak across profiles. */
  function changeProfile(next: string) {
    setSelected(null);
    navigate(`/integrations/bridges/${encodeURIComponent(pairing)}/${encodeURIComponent(next)}`);
  }
  return <div className="min-w-0 space-y-6">
    <Link to="/integrations/bridges" className="inline-flex min-h-9 items-center gap-1.5 text-sm font-medium text-slate-500 hover:text-slate-900"><ArrowLeft className="h-4 w-4" aria-hidden="true" />Bridges</Link>
    <header className="flex min-w-0 flex-col items-start justify-between gap-4 sm:flex-row">
      <div className="min-w-0 flex-1">
        <div className="flex items-start gap-3"><span className="rounded-xl border border-slate-200 bg-white p-3"><Cable className="h-6 w-6 text-slate-600" aria-hidden="true" /></span><div className="min-w-0"><h1 className="break-words text-xl font-semibold text-slate-900">{/* Keep the Bridge identity stable when changing the selected profile. */}{installation?.name ?? "Bridge"}</h1><p className="mt-1 text-sm text-slate-500">Connections and operations by profile</p></div></div>
        {/* The retained manifest is readable offline; connection state is never inferred from schema presence. */}
        {manifest && <div className="mt-4 flex flex-wrap items-center gap-3"><span className={`rounded-md px-2 py-1 text-xs font-medium ${manifest.state === "online" ? "bg-emerald-50 text-emerald-700" : "bg-slate-100 text-slate-500"}`}>{manifest.state === "online" ? "Online" : "Offline"}</span><span className="text-xs text-slate-500">{operations.length} operations</span></div>}
      </div>
      <div className="flex w-full flex-wrap items-end gap-3 sm:w-auto">
        <label className="min-w-0 flex-1 text-xs font-medium text-slate-500 sm:w-56 sm:flex-none">Profile
          <Select value={profile} disabled={loading} onChange={/** Switch only within the current Bridge installation. */ event => changeProfile(event.target.value)} className="mt-1.5 w-full">
            {profiles.map(/** Preserve exact profile names as values rather than deriving display aliases. */ id => <option key={id} value={id}>{id}</option>)}
          </Select>
        </label>
      <button type="button" onClick={refresh} disabled={loading} className="inline-flex min-h-10 items-center gap-2 rounded-lg border border-slate-300 bg-white px-3 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50"><RefreshCw className="h-4 w-4" aria-hidden="true" />Refresh</button>
      </div>
    </header>
    {/* Loading and failed reads never masquerade as an empty operation catalogue. */}
    {loading && <p role="status" className="text-sm text-slate-500">Loading profile…</p>}
    {!loading && error && <p role="alert" className="rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-700">{error}</p>}
    {!loading && !error && !manifest && <p role="status" className="rounded-lg border border-slate-200 bg-white p-6 text-sm text-slate-500">This profile is no longer available. Return to Bridges to view the current profiles.</p>}
    {!loading && !error && manifest && <section aria-label="Profile operations" className="min-w-0 overflow-hidden rounded-xl border border-slate-200 bg-white">
      <ServiceCatalogHeader title="Explore operations" description="Browse operations available for this profile, grouped by connection." />
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-4 py-3 sm:px-5">
        <div className="relative w-full min-w-0 sm:w-auto sm:min-w-[250px] sm:flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-slate-400" aria-hidden="true" />
          <input type="text" aria-label="Search operations" placeholder="Search operations or connections..." value={query} onChange={/** Filter the current profile without changing its accepted contracts. */ event => setQuery(event.target.value)} className="w-full rounded-md border border-slate-300 py-1.5 pl-9 pr-8 text-sm focus:border-slate-500 focus:outline-none focus:ring-1 focus:ring-gray-500" />
          {/* The service search pattern keeps clearing local to the current catalogue. */}
          {query && <button type="button" aria-label="Clear operation search" onClick={/** Restore all rows in the selected type filter. */ () => setQuery("")} className="absolute right-2.5 top-1/2 -translate-y-1/2 cursor-pointer text-slate-400 hover:text-slate-600">✕</button>}
        </div>
        <Select aria-label="Operation type" density="compact" value={kind} onChange={/** Type filtering preserves connection grouping and the selected profile. */ event => setKind(event.target.value)} className="w-full text-xs text-slate-600 sm:w-auto"><option value="all">All types</option><option value="read">READ</option><option value="command">COMMAND</option><option value="event">EVENT</option></Select>
      </div>
      <div className="max-h-[60dvh] divide-y divide-slate-100 overflow-y-auto overscroll-contain" role="region" aria-label="Operation list" tabIndex={0}>
        {/* Empty profiles differ from a search or type filter with no matches. */}
        {groups.length === 0 && <p className="p-8 text-center text-sm text-slate-500">{operations.length === 0 ? "No operation schemas available yet. Start this profile in Bridge Studio to sync them." : "No matching operations."}</p>}
        {groups.map(/** Connection identity keeps disclosure state local to the named group. */ group => <BridgeConnectionGroup key={group.connection} connection={group.connection} operations={group.operations} searching={query.trim() !== "" || kind !== "all"} onSelect={setSelected} />)}
      </div>
    </section>}
    {/* Pin inspector content to the same manifest as the selected row; refresh closes it before replacing the manifest. */}
    {selected && manifest && <BridgeOperationSidebar pairing={pairing} manifest={manifest} operation={selected} onClose={/** Return to the same profile and list position. */ () => setSelected(null)} />}
  </div>;
}

/** Mirror Service resources while reusing the exact shared operation row renderer. */
function BridgeConnectionGroup({ connection, operations, searching, onSelect }: { connection: string; operations: Operation[]; searching: boolean; onSelect: (operation: Operation) => void }) {
  const [collapsed, setCollapsed] = useState(false);
  // Search results must remain visible even if the connection was previously collapsed.
  const expanded = searching || !collapsed;
  return <section aria-label={`Connection ${connection}`} className="mb-2">
    <button type="button" aria-expanded={expanded} disabled={searching} onClick={/** Keep resource disclosure independent of operation inspection. */ () => setCollapsed(/** Toggle only this connection's saved disclosure preference. */ value => !value)} className="mt-2 flex w-full min-w-0 items-center justify-between gap-2 border-y border-slate-100 bg-slate-50 px-4 py-2.5 text-left transition-colors select-none hover:bg-slate-100 focus-visible:outline focus-visible:outline-2 focus-visible:outline-slate-500 sm:px-5">
      <span className="flex min-w-0 flex-wrap items-center gap-2">
        {/* The chevron reflects the same disclosure state as Service resource headers. */}
        {expanded ? <ChevronDown className="h-4 w-4 text-slate-400" aria-hidden="true" /> : <ChevronRight className="h-4 w-4 text-slate-400" aria-hidden="true" />}
        <span className="min-w-0 break-words text-xs font-semibold uppercase tracking-wider text-slate-600">{connection}</span><span className="ml-2 text-xs text-slate-400">({operations.length})</span>
      </span>
    </button>
    {/* Collapsing a connection keeps its data loaded and removes its rows from keyboard navigation. */}
    {expanded && <div className="divide-y divide-slate-50">{operations.map(/** Preserve identity where different devices expose the same operation name. */ operation => <div key={`${operation.connection_id}:${operation.operation_id}`} role="button" tabIndex={0} aria-haspopup="dialog" onClick={/** Inspect only this accepted operation. */ () => onSelect(operation)} onKeyDown={/** Give keyboard activation the same behavior as clicking a service row. */ event => {
      // Only activation keys open the inspector; other keys retain native scrolling and focus behavior.
      if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onSelect(operation); }
    }} className="cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-slate-400">
      {/* Use Bridge kinds as protocol labels; unknown contracts remain visibly unclassified. */}
      <EndpointRow ep={{ id: `${operation.connection_id}:${operation.operation_id}`, method: readContract(operation)?.kind ?? "Unknown", path: operation.operation_id }} selectable={false} />
    </div>)}</div>}
  </section>;
}

/** Reuse the service operation header and schema renderer for one accepted Bridge capability. */
function BridgeOperationSidebar({ pairing, manifest, operation, onClose }: { pairing: string; manifest: BridgeManifest; operation: Operation; onClose: () => void }) {
  const contract = readContract(operation);
  return <OperationDetailsPanel label={`Operation ${operation.operation_id}`} onClose={onClose} header={<OperationDetailsHeader badge="BRIDGE" badgeClass="bg-slate-100 text-slate-700" identity={operation.operation_id} copyLabel="Copy operation name" kind={contract?.kind} kindClass="border-slate-200 bg-slate-50 text-slate-500" onClose={onClose} closeLabel="Close Bridge operation" />}>
      <OperationDetailsBody>
        <OperationSummary title={operation.operation_id}><OperationTechnicalDetails facts={[["Operation ID", operation.operation_id], ["Profile", manifest.profile_id], ["Connection", operation.connection_id]]} /></OperationSummary>
        {/* Invalid contracts remain inspectable as an error rather than invented schemas. */}
        {!contract && <p role="alert" className="text-sm text-rose-700">This operation's contract could not be read. Refresh the profile to load its current manifest.</p>}
        {/* Accepted schemas remain authoritative; missing fields must not look like an empty response. */}
        {contract && <>
          <BridgeUsage pairing={pairing} manifest={manifest} operation={operation} contract={contract} />
          <BridgeSchema label="Input" schema={contract.input_schema} event={contract.kind === "event"} pairing={pairing} hash={manifest.manifest_hash} />
          <BridgeSchema label="Output" schema={contract.output_schema} pairing={pairing} hash={manifest.manifest_hash} />
        </>}
      </OperationDetailsBody>
  </OperationDetailsPanel>;
}

/** Use the same contract disclosures and dark schema surface as operation and webhook inspectors. */
function BridgeSchema({ label, schema, event = false, pairing, hash }: { label: "Input" | "Output"; schema: Contract["input_schema"]; event?: boolean; pairing: string; hash: string }) {
  // Events are received rather than invoked with an input body.
  const summary = event ? "Events are received, not called. No invocation input is required." : bridgeSchemaSummary(schema, label);
  return <OperationSection title={label}>
    {/* Explain absent fields without introducing a separate Bridge-only notice card. */}
    {summary && <p className="mb-3 text-xs leading-5 text-slate-500">{summary}</p>}
    {/* Keep false schemas inspectable; only missing schemas lack a disclosure. */}
    {schema != null && <OperationSchemaDisclosure label={`${label} schema`}>
        {/* A raw empty/open schema avoids the inference wording used by the generic tree renderer. */}
        {summary ? <pre className="text-[11px] leading-5 text-slate-200"><code>{JSON.stringify(schema, null, 2)}</code></pre> : <SchemaViewer schema={schema} serviceId={pairing} componentScope={hash} allowRemoteRefs={false} />}
    </OperationSchemaDisclosure>}
  </OperationSection>;
}

/** Present Bridge calls and bindings with the shared operation/webhook example disclosure. */
function BridgeUsage({ pairing, manifest, operation, contract }: { pairing: string; manifest: BridgeManifest; operation: Operation; contract: Contract }) {
  const usage = bridgeUsage(pairing, manifest.profile_id, manifest.manifest_hash, operation.connection_id, operation.operation_id, contract);
  // Event capabilities can only be selected by SDK receivers, never invoked as unary calls.
  const isEvent = contract.kind === "event";
  return <OperationSection title="Use in an app">
    {/* Events omit the unary example instead of suggesting an unsupported execution path. */}
    {!isEvent && <OperationCodeExample label="Unified App example" language="TypeScript" code={usage.code} copyLabel="Copy Unified App example" description="Use this call inside a Unified App's execute function. Add the matching app binding before plan/apply. Use a unique alias in both places when adding more operations." />}
    <OperationCodeExample label="App binding" language="JSON" code={usage.binding} copyLabel="Copy app binding" description={isEvent ? "Add this binding to an SDK configuration, then handle the generated LocalBridgeEvent with its webhook receiver. Events cannot be called with fused.callBridge." : "Add this binding to your app configuration. It selects this exact profile, connection, and operation."} />
  </OperationSection>;
}
