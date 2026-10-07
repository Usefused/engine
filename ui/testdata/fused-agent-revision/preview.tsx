import React from 'react';
import { createRoot } from 'react-dom/client';
import { createBrowserRouter, RouterProvider } from 'react-router-dom';
import { FusedAgentProvider } from '../../app/components/agent/FusedAgentProvider';
import { CurrentActorAccessProvider } from '../../app/components/access/CurrentActorAccess';
import { ToastProvider } from '../../app/components/Toast';
import CreateUnifiedApp from '../../app/routes/integrations.unified-apps.new';
import { api } from '../../app/lib/api';

const sid = '10000000-0000-4000-8000-000000000001', vid = '10000000-0000-4000-8000-000000000002';
const operations = ['PostCustomers', 'PostCheckoutSessions', 'GetCustomers'].map((name, i) => ({ id: `op-${i}`, service_id: sid, name, description: name, method: i === 2 ? 'GET' : 'POST', path: i === 1 ? '/checkout/sessions' : '/customers', resource_name: 'customers' }));
const service = { id: sid, name: 'Stripe', slug: 'stripe', provider: { name: 'Stripe', handle: 'stripe' }, service_versions: [{ id: vid, name: '2026-07-29.dahlia', status: 'active' }], resources: [], webhooks: [], endpoint_count: 3, webhook_count: 0, public: true };
const source = `import * as z from "zod/mini";
import { buildUnifiedApp } from "@fused/unified-app";
import { services } from "@fused/operations";
export default buildUnifiedApp({
  input: z.object({ email: z.string(), price: z.string() }),
  output: z.object({ url: z.string() }),
  async execute({ input }) {
    const customer = await services.Stripe.PostCustomers({ email: input.email });
    const checkout = await services.Stripe.PostCheckoutSessions({ customer: customer.id, mode: "subscription", line_items: [{ price: input.price, quantity: 1 }], success_url: "https://example.test/success" });
    return { url: checkout.url };
  }
});`;
const revised = source.replace('const customer = await services.Stripe.PostCustomers({ email: input.email });', 'const matches = await services.Stripe.GetCustomers({ email: input.email, limit: 1 });\n    const customer = matches.data[0] ?? await services.Stripe.PostCustomers({ email: input.email });');
const saved = { app_id: 'fixture-checkout', owner_team: 'payments', config: { kind: 'unified_app', name: 'stripe-checkout', version: '1.0.0', bucket: 'default', description: 'Create a subscription checkout', source, services: { Stripe: { version: '2026-07-29.dahlia', operations: ['PostCustomers', 'PostCheckoutSessions'], auth: { type: 'bearer', name: 'bearerAuth' } } } }, service_pins: [{ key: 'Stripe', service_id: sid, service_version_id: vid }] };

/** Records the real UI API sequence without making any production model or provider calls. */
async function query(document: string, variables: any = {}) {
  await fetch('/fixture/record', { method: 'POST', body: JSON.stringify({ query: document.match(/query\s+(\w+)/)?.[1], variables }) });
  // Synthetic permissions allow editing this fixture without configuring a real account.
  if (document.includes('currentActorAccess')) return { currentActorAccess: { subject_id: 'fixture', workspace_id: 'fixture', kind: 'user', grants: [] } };
  if (document.includes('appBuildSelectors')) return { appBuildSelectors: { items: [{ resource_id: 'bucket', display_name: 'default' }], total: 1 } };
  if (document.includes('parseSDKIntent')) return { parseSDKIntent: { action: 'update', services: [{ name: 'Stripe', endpoint_queries: ['Find a customer by email'] }] } };
  if (document.includes('serviceCandidatesByRefs')) return { serviceCandidatesByRefs: [{ ref: 'Stripe', candidates: [service] }] };
  if (document.includes('classifyPromptOperation')) return { classifyPromptOperation: 'GetCustomers' };
  if (document.includes('draftPromptUnifiedApp')) {
    // Failure and delayed replies exercise atomic edits and cancellation through the production route.
    if (variables.q.includes('fail')) throw new Error('Synthetic drafting failure');
    await new Promise(resolve => setTimeout(resolve, variables.q.includes('slow') ? 6000 : 150));
    return { draftPromptUnifiedApp: JSON.stringify({ source: revised, explanation: 'Added customer lookup before creation.' }) };
  }
  return { searchServices: [service], searchEndpoints: operations, service, serviceVersions: service.service_versions, serviceOperations: operations, serviceVersionAuthConfigs: [{ service_id: sid, version: '2026-07-29.dahlia', service_version_id: vid, auth_configs: [] }], services: { data: [service], total: 1 }, resourceIntegrations: operations };
}
api.graphql = query as typeof api.graphql;
api.mcpGraphql = query as typeof api.mcpGraphql;
// Only private-source hydration is simulated; editing and revision state remain the production React route.
api.appConfig.source = (async () => structuredClone(saved)) as typeof api.appConfig.source;
api.workspace.getServices = (async () => [{ service_id: sid, enabled_versions: [{ service_version_id: vid, status: 'active' }] }]) as typeof api.workspace.getServices;
// Capture compile inputs for assertions while making it impossible for the fixture to deploy anything.
api.appConfig.plan = (async (_kind, request) => {
  await fetch('/fixture/record', { method: 'POST', body: JSON.stringify({ compile: request }) });
  return { plan_id: 'fixture-plan', credential_readiness: { missing: [] } };
}) as typeof api.appConfig.plan;
api.appConfig.apply = async () => { throw new Error('Saving is disabled in this fixture'); };

/** Mounts the real editor and agent sidebar; only account/provider boundaries are synthetic. */
function Preview() {
  return <ToastProvider><CurrentActorAccessProvider isAuth><FusedAgentProvider authenticated><main data-fused-workspace className="min-h-screen p-6"><p className="mb-4 text-sm text-slate-500">Local revision verification · synthetic Stripe app</p><CreateUnifiedApp /></main></FusedAgentProvider></CurrentActorAccessProvider></ToastProvider>;
}
const router = createBrowserRouter([{ path: '*', element: <Preview /> }]);
createRoot(document.getElementById('root')!).render(<RouterProvider router={router} />);
