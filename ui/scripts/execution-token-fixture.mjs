import { createServer } from 'vite';
import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
const ui = path.resolve(import.meta.dirname, '..');
const requests = [];
const port = Number(process.env.FUSED_TOKEN_PREVIEW_PORT || 18211);
const tokens = [
  { id: 'preview-active', name: 'backend-production', status: 'active', allow: ['*'], created_at: '2026-10-01T09:00:00Z', expires_at: null, last_used_at: '2026-10-07T10:00:00Z' },
  { id: 'preview-retry', name: 'retry-token', status: 'active', allow: ['Stripe.PostCustomers'], created_at: '2026-10-02T09:00:00Z', expires_at: '2027-10-01T09:00:00Z', last_used_at: null },
  { id: 'preview-expired', name: 'old-preview', status: 'expired', allow: ['*'], created_at: '2026-09-01T09:00:00Z', expires_at: '2026-09-02T09:00:00Z', last_used_at: null },
  { id: 'preview-revoked', name: 'retired-client', status: 'revoked', allow: ['*'], created_at: '2026-09-01T09:00:00Z', expires_at: null, last_used_at: null },
];
let failedRevoke = false;
const workspace = '22222222-2222-4222-8222-222222222222';
const server = await createServer({ configFile: false, root: path.join(ui, 'testdata/execution-tokens'), resolve: { alias: { '~': path.join(ui, 'app') } }, esbuild: { jsx: 'automatic' }, server: { host: '127.0.0.1', port, strictPort: true, fs: { allow: [ui] } }, plugins: [{ name: 'token-fixture',
  /** Replaces only Engine responses, retaining the real browser request and permission code. */
  configureServer(vite) { vite.middlewares.use(async (req, res, next) => {
    const url = new URL(req.url, 'http://localhost');
    // All assets and source modules stay under Vite's ordinary rendering path.
    if (!['/engine/graphql', '/workspace/app-tokens', '/fixture.css', '/fixture/report'].includes(url.pathname)) return next();
    // The preview uses the same generated CSS as the embedded app.
    if (url.pathname === '/fixture.css') { res.setHeader('Content-Type', 'text/css'); return res.end(readFileSync(path.join(ui, 'build/client/assets', readdirSync(path.join(ui, 'build/client/assets')).find(f => f.endsWith('.css'))))); }
    res.setHeader('Content-Type', 'application/json');
    // Read-only evidence records request shape without any real credential material.
    if (url.pathname === '/fixture/report') return res.end(JSON.stringify(requests));
    let raw = ''; for await (const chunk of req) raw += chunk;
    const input = JSON.parse(raw || '{}');
    // Query parameters let browser checks exercise denied users with no production role changes.
    if (url.pathname === '/engine/graphql') {
      const denied = (req.headers.referer || '').includes('denied');
      // Metadata queries expose lifecycle history only, with an optional deterministic read failure.
      if (input.query?.includes('query AppExecutionTokens')) {
        // A failed list must remain distinguishable from a valid empty token set.
        if ((req.headers.referer || '').includes('list-error')) return res.end(JSON.stringify({ errors: [{ message: 'Token list temporarily unavailable.' }] }));
        return res.end(JSON.stringify({ data: { appTokens: tokens } }));
      }
      return res.end(JSON.stringify({ data: { currentActorAccess: { subject_id: 'preview', workspace_id: workspace, kind: 'user', authorization_revision: 1, grants: denied ? [] : ['sdk', 'mcp', 'api', 'unified_app'].map(kind => ({ permission: `app.${kind}.tokens.manage`, resource_type: 'WORKSPACE', resource_id: workspace })) } } }));
    }
    requests.push({ method: req.method, family: url.searchParams.get('app_family_id'), name: url.searchParams.get('name'), input });
    // A short deterministic delay makes the loading and duplicate-submission guard observable.
    await new Promise(resolve => setTimeout(resolve, 800));
    // Revocation retains history, matching Engine rather than deleting the row from the fixture.
    if (req.method === 'DELETE') {
      const name = url.searchParams.get('name');
      // One recoverable failure exercises manual retry without performing a real credential mutation.
      if (name === 'retry-token' && !failedRevoke) { failedRevoke = true; res.statusCode = 503; return res.end(JSON.stringify({ error: { message: 'Could not revoke this token. Try again.' } })); }
      const token = tokens.find(item => item.name === name && item.status === 'active');
      // Stale names fail instead of claiming an absent token was revoked.
      if (!token) { res.statusCode = 404; return res.end(JSON.stringify({ error: { message: 'The selected app token was not found.' } })); }
      token.status = 'revoked'; res.statusCode = 204; return res.end();
    }
    // Duplicate labels are a real recoverable Engine error shape, never a successful issuance.
    if (input.name === 'duplicate') { res.statusCode = 409; return res.end(JSON.stringify({ error: { code: 'app_token_name_conflict', message: 'A token with this name already exists for this app.', remediation: 'Choose a different token name.' } })); }
    tokens.unshift({ id: `preview-${tokens.length}`, name: input.name, status: 'active', allow: input.allow, created_at: new Date().toISOString(), expires_at: null, last_used_at: null });
    res.end(JSON.stringify({ token: 'fused-app-preview-not-a-real-token', name: input.name, expires_at: '2026-11-07T10:00:00Z' }));
  }); }
}] });
await server.listen();
console.log(`Execution token preview: http://127.0.0.1:${port}/`);
