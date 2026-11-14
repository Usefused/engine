import { CurrentActorAccessProvider } from '../../app/components/access/CurrentActorAccess';
import { createRoot } from 'react-dom/client';
import { AppTryButton } from '../../app/components/apps/AppTrySidebar';
import { api } from '../../app/lib/api';
import './style.css';
// The isolated preview supplies a synthetic permission snapshot through the normal provider.
api.mcpGraphql = async <T,>() => ({ currentActorAccess: { subject_id: 'preview', workspace_id: 'preview', grants: [{ permission: 'app.api.tokens.manage', resource_type: 'APP', resource_id: 'preview' }, { permission: 'app.unified_app.tokens.manage', resource_type: 'APP', resource_id: 'preview' }] } } as T);
api.appTokens.generate = async (_family, input) => ({ token: 'synthetic-preview-token', name: input.name, expires_at: null });
api.appTokens.revoke = async () => undefined;
const bucket = '11111111-1111-4111-8111-111111111111';
const unified = new URLSearchParams(window.location.search).has('unified');
/** Exercises typed object input for authored apps and scalar input for physical operations without a connected account. */
// Each mode retains its own contract rather than coercing a provider form into an authored payload.
api.appConfig.openAPI = async () => ({ 'x-fused-app-id': 'preview', paths: { '/v1/apps/{app_id}/executions': { post: { requestBody: { content: { 'application/json': { schema: { oneOf: [
  { properties: { operation: { const: unified ? 'execute' : 'createCustomer' }, ...(unified ? {} : { selector: {} }), bucket_id: { enum: [bucket], 'x-fused-bucket-names': { [bucket]: 'Sandbox' } }, input: unified ? { type: 'object', required: ['payload'], properties: { payload: { type: 'object', description: 'Request body passed to the app.' } } } : { type: 'object', required: ['email'], properties: { email: { type: 'string', minLength: 3, description: 'Email address for the customer.' }, name: { type: 'string' }, notify: { type: 'boolean' }, metadata: { type: 'object' } } } } },
  ...(!unified ? [{ properties: { operation: { const: 'listCustomers' }, selector: {}, input: { type: 'object', properties: { limit: { type: 'integer', minimum: 1, maximum: 100 } } } } }] : []),
 ] } } } } } } } });
/** Previews configured authored connections and editable provider routing without contacting either service. */
// Authored and physical modes need distinct readiness states to exercise the compact status row.
api.appConfig.tryReadiness = async (_id, _operation, selected, user) => ({ requirements: [{ service_id: unified ? 'openai' : 'stripe', service: unified ? 'OpenAI' : 'Stripe', bucket_id: selected || bucket, bucket_name: selected ? 'Sandbox' : 'Default', auth_name: unified ? 'ApiKeyAuth' : 'apiKey', auth_type: 'api_key', credentials: unified || selected ? 'ready' : 'missing', connection: user ? 'ready' : 'not_required' }], runtime_buckets: [{ id: bucket, name: 'Sandbox' }], note: unified ? 'Connections shown cover declared provider services. The app chooses connected users from its code and input; child apps keep their own buckets.' : undefined });
/** The preview's execution boundary returns synthetic output and never contacts a provider. */
window.fetch = async () => new Response(JSON.stringify({ status_code: 200, results: [{ id: 'cus_preview', email: 'test@example.com' }] }), { status: 200 });
/** Retains the real launch button and modal lifecycle within a representative app page. */
function Preview() {
 return <main className="min-h-dvh bg-slate-50 p-8 text-slate-900"><p className="mb-6 text-xs text-slate-500">Local preview · synthetic connections · no provider calls</p><div className="flex items-center justify-between"><div><h1 className="text-3xl font-semibold">Customer onboarding</h1><p className="mt-2 text-slate-500">Create customers and look up their account details.</p></div><AppTryButton target={{ versionID: 'preview', executionID: 'preview', familyID: 'preview', name: 'Customer onboarding', unified }} /></div><div className="mt-8 rounded-xl border border-slate-200 bg-white p-6">Version 1.0.0 · Active</div></main>;
}
createRoot(document.getElementById('root')!).render(<CurrentActorAccessProvider isAuth><Preview /></CurrentActorAccessProvider>);
