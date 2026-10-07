import { createServer } from 'vite';
import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
const ui = path.resolve(import.meta.dirname, '..');
const requests = [];
const workspace = '22222222-2222-4222-8222-222222222222';
const server = await createServer({ configFile: false, root: path.join(ui, 'testdata/execution-tokens'), resolve: { alias: { '~': path.join(ui, 'app') } }, esbuild: { jsx: 'automatic' }, server: { host: '127.0.0.1', port: 18211, strictPort: true, fs: { allow: [ui] } }, plugins: [{ name: 'token-fixture',
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
      return res.end(JSON.stringify({ data: { currentActorAccess: { subject_id: 'preview', workspace_id: workspace, kind: 'user', authorization_revision: 1, grants: denied ? [] : ['sdk', 'mcp', 'api', 'unified_app'].map(kind => ({ permission: `app.${kind}.tokens.manage`, resource_type: 'WORKSPACE', resource_id: workspace })) } } }));
    }
    requests.push({ method: req.method, family: url.searchParams.get('app_family_id'), input });
    // A short deterministic delay makes the loading and duplicate-submission guard observable.
    await new Promise(resolve => setTimeout(resolve, 800));
    // Duplicate labels are a real recoverable Engine error shape, never a successful issuance.
    if (input.name === 'duplicate') { res.statusCode = 409; return res.end(JSON.stringify({ error: { code: 'app_token_name_conflict', message: 'A token with this name already exists for this app.', remediation: 'Choose a different token name.' } })); }
    res.end(JSON.stringify({ token: 'fused-app-preview-not-a-real-token', name: input.name, expires_at: '2026-11-07T10:00:00Z' }));
  }); }
}] });
await server.listen();
console.log('Execution token preview: http://127.0.0.1:18211/');
