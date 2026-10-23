import { spawn, execFileSync } from 'node:child_process';
import { createInterface } from 'node:readline';
import { mkdtempSync, mkdirSync, writeFileSync, symlinkSync, copyFileSync, readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { dirname, resolve, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';
import { catalogue, TASK, startProvider } from './billing-fixture.mjs';

const runtime = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repository = resolve(runtime, '../..');
const root = mkdtempSync(join(tmpdir(), 'fused-live-eval-'));
const reports = resolve(process.argv[2] || join(root, 'reports'));
mkdirSync(reports, { recursive: true });
const baselineRevision = '172d638007d5ba2843d2125738cb711319dbdb8a';
const baseline = join(root, 'baseline');
mkdirSync(baseline);
// Build the actual previous runtime, not a hand-written imitation of its prompts or execution behavior.
const archive = execFileSync('git', ['archive', baselineRevision, 'runtime/mcp/src'], { cwd: repository });
execFileSync('tar', ['-x', '-C', baseline], { input: archive });
symlinkSync(join(runtime, 'node_modules'), join(baseline, 'runtime/mcp/node_modules'));
execFileSync(join(runtime, 'node_modules/.bin/esbuild'), [join(baseline, 'runtime/mcp/src/server.ts'), '--bundle', '--platform=node', '--format=cjs', `--outfile=${join(baseline, 'bundle.cjs')}`], { stdio: 'pipe' });
// An explicit snapshot isolates prompt-only comparisons from earlier runtime and schema changes.
const baselineBundle = process.argv[3] ? resolve(process.argv[3]) : join(baseline, 'bundle.cjs');
const baselineIdentity = process.argv[3] ? `snapshot-sha256:${createHash('sha256').update(readFileSync(baselineBundle)).digest('hex')}` : baselineRevision;
execFileSync('npm', ['run', 'build'], { cwd: runtime, stdio: 'pipe' });
// Both bundles are CommonJS; the current package's type=module must not reinterpret its .js artifact.
const currentBundle = join(root, 'current.cjs');
copyFileSync(join(runtime, 'dist/bundle.js'), currentBundle);

/** Maintains request identity and bounded waits without exposing harness files to the model. */
function appServer(cwd, onTool, report) {
  const disabled = ['shell_tool', 'unified_exec', 'apps', 'plugins', 'memories', 'multi_agent', 'multi_agent_v2', 'browser_use', 'computer_use', 'image_generation', 'view_image', 'skill_search', 'shell_snapshot', 'hooks', 'sleep_tool', 'goals', 'workspace_dependencies'];
  const args = ['app-server', '-c', 'mcp_servers={}', '-c', 'project_doc_max_bytes=0', '-c', 'web_search="disabled"', '--enable', 'skip_host_skill_discovery'];
  for (const feature of disabled) args.push('--disable', feature);
  const child = spawn('codex', args, { cwd, stdio: ['pipe', 'pipe', 'pipe'] });
  const pending = new Map();
  let nextId = 0, finish;
  const completed = new Promise(resolve => { finish = resolve; });
  /** Sends only protocol messages, never fixtures, solutions, or expected outputs. */
  function send(message) { child.stdin.write(JSON.stringify(message) + '\n'); }
  /** Bounds initialization and control requests independently of the agent's run timeout. */
  function request(method, params) {
    return new Promise((resolve, reject) => {
      const id = ++nextId;
      const timer = setTimeout(() => { pending.delete(id); reject(new Error(`Timed out: ${method}`)); }, 30000);
      pending.set(id, { resolve, reject, timer });
      send({ id, method, params });
    });
  }
  // Only operational stderr is kept locally; it is never sent back to the evaluated agent.
  child.stderr.on('data', data => { report.hostStderr = (report.hostStderr || '') + data.toString(); });
  createInterface({ input: child.stdout }).on('line', async line => {
    try {
      const message = JSON.parse(line);
      // Responses resolve the host's outstanding setup calls; server requests are handled below.
      if (message.id !== undefined && !message.method) {
        const waiter = pending.get(message.id);
        // Ignore late responses whose bounded request already expired.
        if (waiter) {
          clearTimeout(waiter.timer);
          pending.delete(message.id);
          // Setup errors invalidate the run rather than becoming agent failures.
          if (message.error) waiter.reject(new Error(JSON.stringify(message.error)));
          else waiter.resolve(message.result);
        }
        return;
      }
      // The only executable capability offered by this host is forwarding the two real MCP tools.
      if (message.method === 'item/tool/call') {
        const result = await onTool(message.params);
        send({ id: message.id, result });
        return;
      }
      // No approval, filesystem, or other server capability is granted during evaluation.
      if (message.id !== undefined) {
        report.unexpectedRequests.push(message.method);
        send({ id: message.id, error: { code: -32601, message: 'Unavailable in this evaluation host' } });
      }
      // Usage notifications carry cumulative counters; retain the latest rather than double-counting them.
      if (message.method === 'thread/tokenUsage/updated') report.usage = message.params.tokenUsage;
      // Completed items preserve exact tool arguments and the final answer for independent inspection.
      if (message.method === 'item/completed') report.items.push(message.params.item);
      // Host errors are evidence even if the model subsequently completes the task.
      if (message.method === 'error') report.hostErrors.push(message.params);
      // Completion ends the wait only after the model has finished its own tool sequence.
      if (message.method === 'turn/completed') finish(message.params.turn);
    } catch (error) {
      report.hostErrors.push({ message: error.message });
      finish({ status: 'host_failed' });
    }
  });
  // An early host exit is a transport failure, not a completed agent task.
  child.on('exit', code => finish({ status: 'host_exited', code }));
  // Terminating the ephemeral host cannot resume or expose another run's context.
  return { request, send, completed, close: () => child.kill() };
}

let pinnedModel;
/** Runs one fresh model conversation against one fresh MCP session and hidden synthetic account. */
async function evaluate(variant, seed, fault) {
  const label = `${variant}-${seed}${fault ? '-lost-response' : ''}`;
  const provider = await startProvider(seed, fault);
  const workdir = mkdtempSync(join(root, 'empty-'));
  const fixturePath = join(root, `${label}-fixture.json`);
  writeFileSync(fixturePath, JSON.stringify(catalogue));
  const report = { label, variant, seed, task: TASK, baselineRevision: baselineIdentity, startedAt: new Date().toISOString(), calls: [], items: [], unexpectedRequests: [], hostErrors: [] };
  // Each variant runs its compiled runtime with identical transport and provider configuration.
  const transport = new StdioClientTransport({ command: process.execPath, args: [variant === 'baseline' ? baselineBundle : currentBundle], env: { FUSED_FIXTURE_PATH: fixturePath, FUSED_ENGINE_PORT: String(provider.port), FUSED_SESSION_ID: provider.bearer }, stderr: 'pipe' });
  const client = new Client({ name: 'fused-blind-evaluation', version: '1.0.0' });
  let host, timeout;
  try {
    await client.connect(transport);
    // Preserve startup diagnostics as harness evidence without feeding them to the evaluated model.
    transport.stderr?.on('data', data => { report.runtimeStderr = (report.runtimeStderr || '') + data.toString(); });
    const { tools } = await client.listTools();
    // Guard the advertised capability set before any model turn begins.
    if (tools.map(tool => tool.name).sort().join(',') !== 'execute,search_docs') throw new Error('Expected exactly two MCP tools');
    report.publicContract = { instructions: client.getInstructions(), tools };
    // Forward raw MCP text and error status without coaching, schema repairs, or answer hints.
    host = appServer(workdir, async ({ tool, arguments: args }) => {
      // Unexpected capabilities and unbounded loops invalidate the run instead of escaping the test boundary.
      if (!['search_docs', 'execute'].includes(tool) || report.calls.length >= 20) throw new Error('Tool boundary or call budget exceeded');
      const call = { tool, arguments: args };
      report.calls.push(call);
      const result = await client.callTool({ name: tool, arguments: args });
      call.result = result;
      return { contentItems: result.content.map(content => ({ type: 'inputText', text: content.text })), success: !result.isError };
    }, report);
    await host.request('initialize', { clientInfo: { name: 'fused_blind_eval', version: '1' }, capabilities: { experimentalApi: true } });
    host.send({ method: 'initialized', params: {} });
    const started = await host.request('thread/start', {
      model: pinnedModel, cwd: workdir, ephemeral: true, environments: [], selectedCapabilityRoots: [],
      approvalPolicy: 'never', sandbox: 'read-only', baseInstructions: client.getInstructions(), developerInstructions: '',
      dynamicTools: tools.map(tool => ({ type: 'function', name: tool.name, description: tool.description, inputSchema: tool.inputSchema, deferLoading: false })),
      config: { project_doc_max_bytes: 0, 'features.memories': false, 'features.skip_host_skill_discovery': true },
    });
    report.host = { model: started.model, reasoningEffort: started.reasoningEffort, instructionSources: started.instructionSources, environments: [] };
    console.log(JSON.stringify({ started: label, ...report.host }));
    // Fail closed if the host loaded repository or personal instruction files.
    if (started.instructionSources?.length) throw new Error('Unexpected instruction files loaded');
    pinnedModel = started.model;
    await host.request('turn/start', { threadId: started.thread.id, input: [{ type: 'text', text: TASK }], environments: [] });
    report.turn = await Promise.race([host.completed, new Promise(resolve => { timeout = setTimeout(() => resolve({ status: 'timeout' }), 180000); })]);
  } catch (error) {
    report.setupError = error.message;
  } finally {
    clearTimeout(timeout);
    host?.close();
    await client.close();
    provider.server.closeAllConnections();
    await new Promise(resolve => provider.server.close(resolve));
  }
  report.finishedAt = new Date().toISOString();
  report.providerCalls = provider.attempts;
  report.drafts = provider.drafts;
  report.grade = provider.grade();
  // A confident answer cannot substitute for completed, policy-correct, nonduplicated provider effects.
  report.success = report.turn?.status === 'completed' && report.grade.complete && report.grade.exclusionsRespected && report.grade.duplicateWrites === 0;
  writeFileSync(join(reports, `${label}.json`), JSON.stringify(report, null, 2));
  console.log(JSON.stringify({ label, model: report.host?.model, status: report.turn?.status, success: report.success, toolCalls: report.calls.length, providerCalls: provider.attempts.length, usage: report.usage?.total, grade: report.grade, setupError: report.setupError }));
  // Setup failures cannot establish a meaningful comparison and must not burn repeated model calls.
  if (report.setupError || report.turn?.status === 'host_failed' || report.turn?.status === 'host_exited') throw new Error(`Invalid evaluation: ${label}; see saved report`);
  return report;
}

const results = [];
// Alternate order across paired accounts to reduce cache/order bias; every invocation still has fresh state.
for (const [seed, fault, order] of [[11, false, ['baseline', 'current']], [22, false, ['current', 'baseline']], [33, false, ['baseline', 'current']], [44, true, ['current', 'baseline']]]) {
  for (const variant of order) results.push(await evaluate(variant, seed, fault));
}
writeFileSync(join(reports, 'summary.json'), JSON.stringify(results.map(({ label, host, grade, success, calls, providerCalls, usage, turn }) => ({ label, host, grade, success, toolCalls: calls.length, providerCalls: providerCalls.length, usage, status: turn?.status })), null, 2));
console.log(`Reports: ${reports}`);
