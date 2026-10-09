import { useCallback, useRef } from 'react';
import { createRoot } from 'react-dom/client';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
import { AgentServiceImport } from '../../app/components/integration-details/AgentServiceImport';
import { FusedAgentProvider } from '../../app/components/agent/FusedAgentProvider';
import { FusedAgentContext, useFusedAgent, type FusedServiceImportBridge } from '../../app/components/agent/FusedAgentContext';
import { CurrentActorAccessProvider } from '../../app/components/access/CurrentActorAccess';
import { ToastProvider } from '../../app/components/Toast';
import { api, type Service, type DiscoveryReviewSummary } from '../../app/lib/api';

const service = { id: 'preview-service', name: 'Agent Import Demo', slug: 'agent-import-demo', is_owner: true } as Service;
const baseline = { service_id: service.id, service_version_id: 'preview-version', revision: 1 };
const receipt = { draft_id: 'preview-draft', draft_revision: 1, review_hash: 'preview-hash' };
const empty = { returned: 0, total: 0, omitted: 0 };
const names = ['item.created', 'item.updated', 'return.approved', 'return.dropoff.created', 'return.dropoff.shipment.updated', 'return.dropoff.updated', 'return.exchange.order.created', 'return.expired', 'return.receiving.created', 'return.rejected', 'return.resolved', 'return.restock.created', 'return.shipment.provided', 'return.shipment.recorded', 'return.shipment.updated', 'return.shipments.provided', 'return.submitted'];
const summary: DiscoveryReviewSummary = { schema_version: 1, session_id: 'preview-session', ...receipt, info: { title: service.name, version: '1.0.0' }, server_counts: empty, auth_scheme_counts: empty, operation_counts: empty, diagnostic_count: 2, evidence_count: 7, webhook_counts: { returned: names.length, total: names.length, omitted: 0 },
  // Public event names exercise wrapping and duplicate-label removal without invoking a model.
  webhooks: names.map(name => ({ method: 'POST', path: name, operation_id: name, summary: 'A documented event delivered to your webhook receiver.', parameter_counts: empty, request_media_types: ['application/json'], request_media_type_counts: {returned:1,total:1,omitted:0}, response_counts: empty, security_alternative_counts: empty })) };
const plan = { plan_id: 'preview-plan', source_hash: '', review_hash: 'preview-plan-hash', service_id: service.id, name: service.name, target_version: '1.0.0', action: 'update_version', is_new_service: false, target_type: 'webhooks', expected_target: baseline, diff: {added: 15, changed: 0, removed: 0} } as const;
/** Fixtures supply only synthetic owner metadata; no account or service is changed. */
api.graphql = (async () => ({ serviceWebhookEditor: baseline })) as typeof api.graphql;
/** Only the permission used by this screen is granted in the preview. */
api.mcpGraphql = (async () => ({ currentActorAccess: {subject_id:'preview',workspace_id:'preview',kind:'user',grants:[{permission:'catalogue.import',resource_type:'WORKSPACE',resource_id:'preview'}]} })) as typeof api.mcpGraphql;
/** Return a prepared fixture instead of crawling or spending model credits. */
api.integrations.startDiscovery = async () => ({session_id:'preview-session',revision:1,state:'awaiting_review',payload:{contract:receipt}} as any);
/** Exercise the real review renderer with a bounded structural summary. */
api.integrations.getDiscoveryReviewSummary = async () => summary;
/** Only planning is mocked; no persistence API is exposed by the preview. */
api.integrations.actOnDiscovery = async () => ({state:'plan_ready',payload:{plan,import_plan:plan}} as any);
/** Prevent the visual fixture from ever submitting a real import. */
api.integrations.applyImport = async () => { throw new Error('Layout preview only; nothing was applied.'); };
/** The preview never navigates following a save. */
function saved() {}

/** Captures the real provider bridge while preserving its portal and mobile behavior. */
function Preview() {
 const agent = useFusedAgent()!;
 const bridge = useRef<FusedServiceImportBridge | null>(null);
 /** Keep production registration active so opening chat sees the same page state. */
 const register = useCallback((value: FusedServiceImportBridge) => { bridge.current = value; return agent.registerServiceImport(value); }, [agent.registerServiceImport]);
 /** Prepare through the actual UI adapter without a model call. */
 async function prepare() { await bridge.current?.prepare({target_type:'webhooks', source_mode:'docs', source_url:'https://example.test/webhooks'}, new AbortController().signal); }
 return <FusedAgentContext.Provider value={{...agent,registerServiceImport:register}}><main className="min-h-dvh bg-slate-50 p-8 text-slate-900"><h1 className="text-2xl font-semibold">Agent Import Demo</h1><p className="my-4 text-sm text-slate-500">Layout preview · no live changes</p><button onClick={prepare} className="rounded-lg border bg-white px-4 py-2">Open import review</button><AgentServiceImport service={service} version="1.0.0" onSaved={saved}/></main></FusedAgentContext.Provider>;
}
const router = createMemoryRouter([{path:'*',element:<ToastProvider><CurrentActorAccessProvider isAuth><FusedAgentProvider authenticated><Preview/></FusedAgentProvider></CurrentActorAccessProvider></ToastProvider>}]);
createRoot(document.getElementById('root')!).render(<RouterProvider router={router}/>);
