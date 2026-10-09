import { useCallback, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { AgentServiceImport } from "../../app/components/integration-details/AgentServiceImport";
import { FusedAgentContext, type FusedServiceImportBridge } from "../../app/components/agent/FusedAgentContext";
import { CurrentActorAccessProvider } from "../../app/components/access/CurrentActorAccess";
import { ToastProvider } from "../../app/components/Toast";
import { api, type Service } from "../../app/lib/api";

const service = { id: 'fixture-service', name: 'Demo CRM', slug: 'demo-crm', is_owner: true } as Service;
let plans = 0, applies = 0;
const baseline = { service_id: service.id, service_version_id: 'fixture-version', revision: 3 };
/** Supplies only local synthetic owner metadata and records no credentials. */
api.graphql = (async () => ({ serviceWebhookEditor: baseline })) as typeof api.graphql;
/** Enables the exact import permission rather than giving the fixture wildcard access. */
api.mcpGraphql = (async () => ({ currentActorAccess: { subject_id: 'fixture', workspace_id: 'fixture', kind: 'user', grants: [{ permission: 'catalogue.import', resource_type: 'WORKSPACE', resource_id: 'fixture' }] } })) as typeof api.mcpGraphql;
/** Produces a replacement diff so UI verification must expose the removal warning. */
api.integrations.planImport = async input => { plans++; return { plan_id: 'fixture-plan', source_hash: '', review_hash: 'review', service_id: service.id, name: service.name, target_version: 'v1', action: 'update_version', is_new_service: false, target_type: input.target_type, expected_target: input.expected_target, diff: { added: 1, changed: 0, removed: 1, removed_names: ['old-event'] } }; };
/** Simulates an interrupted commit to test recovery without mutating any real service. */
api.integrations.applyImport = async () => { applies++; throw new Error('Synthetic interrupted response'); };
/** Confirms the synthetic ledger outcome while preserving the exact destination. */
api.integrations.importStatus = async () => ({ status: 'applied', operation_id: 'fixture-plan', phase: 'done', commit_state: 'committed', service_id: service.id, version: 'v1' });

/** Other agent features are inert because this fixture tests only import planning. */
function noop() {}
/** Unused editor registrations return a harmless cleanup in this isolated harness. */
function unusedRegistration() { return noop; }

/** Exercises the production bridge, permissions and review UI without a live model or Registry. */
function Fixture() {
  const bridge = useRef<FusedServiceImportBridge | null>(null);
  const [message, setMessage] = useState('No requests yet');
  const [owner, setOwner] = useState(true);
  /** Models the agent provider's active-page registration without exposing a submit tool. */
  const registerServiceImport = useCallback((value: FusedServiceImportBridge) => { bridge.current = value; return () => { /* A newer registration must survive an earlier cleanup. */ if (bridge.current === value) bridge.current = null; }; }, []);
  /** Calls the same review-only bridge used by the client tool and reports request counts. */
  async function prepare(target: 'endpoints' | 'webhooks') {
    try { await bridge.current?.prepare({ target_type: target, source_content: '{}' }, new AbortController().signal); setMessage(`Plans: ${plans}; applies: ${applies}`); }
    catch (cause) { setMessage(String(cause)); }
  }
  /** Records confirmed status recovery for visible verification. */
  function saved() { setMessage(`Confirmed save; plans: ${plans}; applies: ${applies}`); }
  /** Switches fixture ownership without changing an account or backend permission. */
  function changeOwner(event: React.ChangeEvent<HTMLInputElement>) { setOwner(event.target.checked); }
  /** Requests the endpoint branch through the production bridge. */
  function endpoints() { void prepare('endpoints'); }
  /** Requests the webhook branch through the production bridge. */
  function webhooks() { void prepare('webhooks'); }
  return <ToastProvider><CurrentActorAccessProvider isAuth><FusedAgentContext.Provider value={{ isOpen: false, open: noop, registerEditor: unusedRegistration, registerDraftBuilder: unusedRegistration, registerServiceImport }}><main data-fused-workspace className="p-8 space-y-4"><h1 className="text-xl font-semibold">Service import verification</h1><p>Synthetic Demo CRM · v1. No live imports.</p><label><input type="checkbox" checked={owner} onChange={changeOwner} /> Owner</label><div className="flex gap-4"><button onClick={endpoints}>Prepare endpoints</button><button onClick={webhooks}>Prepare webhooks</button></div><p role="status">{message}</p><AgentServiceImport service={{ ...service, is_owner: owner }} version="v1" onSaved={saved} /></main></FusedAgentContext.Provider></CurrentActorAccessProvider></ToastProvider>;
}
createRoot(document.getElementById('root')!).render(<Fixture />);
