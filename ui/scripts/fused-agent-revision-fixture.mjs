import { createServer } from 'vite';
import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
const ui = path.resolve(import.meta.dirname, '..');
const state = { calls: [], tools: [], goal: '', turn: 0 };
/** Produces the same suspension envelope as the Harnest runtime. */
function action(id, name, args = {}) { return { type: 'response.completed', status: 'requires_action', requiredAction: { type: 'client_tool', id, callId: id, name, arguments: args } }; }
/** Completes a synthetic response after exercising production frontend tool dispatch. */
function done(text) { return { type: 'response.completed', status: 'completed', outputText: text }; }
/** Reads a fixture request without depending on application authentication. */
async function body(req) { let value = ''; for await (const chunk of req) value += chunk; return JSON.parse(value || '{}'); }
const server = await createServer({ configFile: false, root: path.join(ui, 'testdata/fused-agent-revision'), resolve: { alias: { '~': path.join(ui, 'app') } }, esbuild: { jsx: 'automatic' }, server: { host: '127.0.0.1', port: 18210, strictPort: true, fs: { allow: [ui] } }, plugins: [{ name: 'revision-fixture',
  /** Replaces external boundaries only; React state and tool implementation are real. */
  configureServer(vite) { vite.middlewares.use(async (req, res, next) => {
    const url = new URL(req.url, 'http://localhost');
    // Let Vite handle modules and HTML normally.
    if (!url.pathname.startsWith('/agent/') && !url.pathname.startsWith('/fixture/') && url.pathname !== '/fixture.css') return next();
    // Built CSS ensures the preview uses production styling.
    if (url.pathname === '/fixture.css') { res.setHeader('Content-Type', 'text/css'); return res.end(readFileSync(path.join(ui, 'build/client/assets', readdirSync(path.join(ui, 'build/client/assets')).find(f => f.endsWith('.css'))))); }
    let result = {};
    // Routes expose synthetic evidence without saving any application data.
    if (url.pathname === '/fixture/report') result = state;
    else if (url.pathname === '/fixture/record') state.calls.push(await body(req));
    else if (url.pathname === '/agent/status') result = { ready: true, status: 'ready', enabled: true };
    else if (url.pathname === '/agent/sessions') result = req.method === 'POST' ? { id: 'fixture', state: {}, metadata: {} } : { sessions: [] };
    else if (url.pathname === '/agent/responses') {
      const request = await body(req); state.goal = request.input.split('\n\nUser request:\n').at(-1); state.turn++;
      res.setHeader('Content-Type', 'text/event-stream'); return res.end(`data: ${JSON.stringify(action(`page-${state.turn}`, 'get_page_context'))}\n\n`);
    } else if (url.pathname.startsWith('/agent/client-tools/')) {
      const id = url.pathname.split('/').at(-1), { output } = await body(req);
      state.tools.push({ id, output });
      // Failures stop the chain so no fallback mutation masks the issue.
      if (output?.ok === false) result = done(`Tool refused: ${output.error}`);
      else if (id.startsWith('page-') && state.goal.startsWith('config')) result = action(`config-view-${state.turn}`, 'set_unified_app_view', { view: 'yaml', expected_revision: output.revision });
      // The fixture edits only the visible YAML handle returned by the real view-switch tool.
      else if (id.startsWith('config-view-')) {
        const field = output.page.fields.find(item => item.label === 'YAML configuration');
        // Malformed text must produce validation failure, while ordinary config edits remain unsaved.
        const value = state.goal.includes('invalid') ? 'kind: [' : field.value.replace('description: Create a subscription checkout', 'description: Reuse customers by email') + 'generate: false\n';
        result = action(`config-edit-${state.turn}`, 'update_form_field', { field_id: field.id, expected_revision: output.page.revision, value });
      } else if (id.startsWith('config-edit-')) result = done('Updated the unsaved YAML configuration.');
      else if (id.startsWith('page-')) result = action(`search-${state.turn}`, 'search_services', { query: 'Stripe' });
      else if (id.startsWith('search-')) result = action(`ops-${state.turn}`, 'search_service_operations', { service_id: '10000000-0000-4000-8000-000000000001', version: '2026-07-29.dahlia', query: 'customer email' });
      else if (id.startsWith('ops-')) result = action(`refresh-${state.turn}`, 'get_page_context');
      else if (id.startsWith('refresh-')) result = action(`revise-${state.turn}`, 'revise_unified_app', { goal: state.goal, expected_revision: output.revision - (state.goal.includes('stale') ? 1 : 0) });
      else if (id.startsWith('revise-')) result = done('Updated the unsaved selection and TypeScript together. Review and compile the draft.');
      else result = done('Fixture complete.');
    }
    res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(result));
  }); }
}] });
await server.listen();
console.log('Revision fixture: http://127.0.0.1:18210/integrations/unified-apps/new?edit=fixture-checkout');
