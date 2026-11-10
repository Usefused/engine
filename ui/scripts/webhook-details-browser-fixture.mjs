import { createServer } from "vite";
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ui = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const service = "11111111-1111-4111-8111-111111111111";
const registration = "22222222-2222-4222-8222-222222222222";
const item = { id: registration, service_id: service, service_name: "Synthetic payments", service_ref: "@fixture/payments", label: "Billing events", slug: "billing-route", callback_url: "https://engine.example/webhook/billing-route", delivery_mode: "direct", signature: "set", created_at: "2026-11-01T12:00:00Z" };
const state = { mode: "success", role: "both", calls: [] };

/** Returns only synthetic JSON; no fixture route proxies to a Registry or Engine. */
function reply(response, body, status = 200) {
  response.statusCode = status;
  response.setHeader("Content-Type", "application/json");
  response.end(JSON.stringify(body));
}

/** Grants a fixed permission matrix for checking that denied tabs issue no reporting request. */
function access() {
  const serviceGrant = { permission: "service.read", resource_type: "SERVICE", resource_id: service };
  const auditGrant = { permission: "audit.read", resource_type: "WORKSPACE", resource_id: "workspace" };
  const grants = { both: [serviceGrant, auditGrant], "service-only": [serviceGrant], "audit-only": [auditGrant], neither: [] }[state.role];
  return { subject_id: "fixture", workspace_id: "workspace", kind: "user", authorization_revision: 1, grants };
}

/** Builds a stable multi-page receipt history with failures and long identifiers to exercise narrow layouts. */
function events() {
  return Array.from({ length: 45 }, (_, index) => {
    // Fixed outcomes exercise drill-downs; rejected requests retain no trusted event type.
    const status = index % 5 === 0 ? "rejected" : index % 3 === 0 ? "failed" : "delivered";
    return { id: `receipt-${index}`, account_id: "fixture", service_id: service, msg_id: `event-${index}-very-long-provider-message-identifier`, event_name: status === "rejected" ? "UNKNOWN" : "invoice.paid", status, delivery_status: status, verification_status: status === "rejected" ? "credential_invalid" : "verified", environment: "production", latency_ms: 12 + index, retry_count: 0, credits_consumed: 0, payload_size: 1024, created_at: "2026-11-10T00:00:00Z", error_reason: status === "delivered" ? "" : "Synthetic verification or delivery failure" };
  });
}

/** Restricts the fake API to the production detail and Activity documents and verifies exact scope. */
function graphql(input, response) {
  const query = input.query, variables = input.variables || {};
  state.calls.push({ query, variables });
  // Access loading/failure is independent of registration metadata and receipt loading.
  if (query.includes("CurrentActorAccess")) return reply(response, state.mode === "access-error" ? { errors: [{ message: "Synthetic access outage" }] } : { data: { currentActorAccess: access() } });
  // The catalogue remains available for testing the name's actual navigation link.
  if (query.includes("WorkspaceWebhookPage")) return reply(response, { data: { workspaceWebhookPage: { items: [item], total: 1 } } });
  // Removed and managed registrations have separate observable states.
  if (query.includes("WebhookRegistration(")) return reply(response, { data: { workspaceWebhook: state.mode === "missing" ? null : { ...item, delivery_mode: state.mode === "managed" ? "managed" : "direct", callback_url: state.mode === "managed" ? "" : item.callback_url } } });
  // Reporting must always carry the exact registration, never only a label or service name.
  if (variables.registrationId !== registration || variables.serviceId !== service) return reply(response, { errors: [{ message: "Missing exact registration scope" }] });
  // Outages and confirmed empty activity must remain distinguishable in the page.
  if (state.mode === "error") return reply(response, { errors: [{ message: "Synthetic reporting outage" }] });
  const rows = state.mode === "empty" ? [] : events();
  // Summary counts are computed from the same synthetic receipt source as the list.
  if (query.includes("WebhookRegistrationAnalytics")) return reply(response, { data: { webhookAnalytics: { total_ingested: rows.length, total_delivered: rows.filter((row) => row.status === "delivered").length, total_rejected: rows.filter((row) => row.status === "rejected").length, total_failed: rows.filter((row) => row.status === "failed").length, average_latency_ms: rows.length ? 34 : 0, p95_latency_ms: rows.length ? 54 : 0 } } });
  // Fixed filters emulate SQL filtering before counts and offsets, including rejected ingress in failures.
  const filtered = rows.filter((row) => (!variables.eventName || row.event_name === variables.eventName) && (!variables.status || (variables.status === "failed" ? row.status !== "delivered" : variables.status === "success" ? row.status === "delivered" : row.status === variables.status)));
  return reply(response, { data: { webhookEvents: { items: filtered.slice(variables.offset, variables.offset + variables.limit), total: filtered.length } } });
}

/** Bounds synthetic request parsing so the fixture cannot become an arbitrary local upload service. */
async function readBody(request) {
  let body = "";
  for await (const chunk of request) {
    body += chunk;
    // Only small GraphQL test documents are admitted.
    if (body.length > 64 * 1024) throw new Error("Fixture request too large");
  }
  return JSON.parse(body || "{}");
}

const server = await createServer({ configFile: false, root: path.join(ui, "testdata/webhook-details"), resolve: { alias: { "~": path.join(ui, "app") }, dedupe: ["react", "react-dom"] }, esbuild: { jsx: "automatic" }, server: { host: "127.0.0.1", port: 18194, strictPort: true }, plugins: [{ name: "synthetic-webhook-details", configureServer(vite) {
  // All observed API requests stay in process and can be inspected through the local fixture state.
  vite.middlewares.use(async (request, response, next) => {
    const url = new URL(request.url, "http://127.0.0.1");
    // Test modes apply on document navigation; no production query or route behavior is changed.
    if (url.searchParams.has("fixtureMode")) { state.mode = url.searchParams.get("fixtureMode"); state.role = url.searchParams.get("fixtureRole") || "both"; state.calls = []; }
    // Reuse the built product CSS so responsive behavior is the actual deployed treatment.
    if (url.pathname === "/fixture.css") { const assets = path.join(ui, "build/client/assets"); const css = readdirSync(assets).find((file) => file.endsWith(".css")); response.setHeader("Content-Type", "text/css"); response.end(readFileSync(path.join(assets, css))); return; }
    // This read-only endpoint contains only synthetic requests and allows permission assertions.
    if (url.pathname === "/fixture/state") return reply(response, state);
    // The closed API route never forwards an unexpected request to a real backend.
    if (url.pathname !== "/engine/graphql") return next();
    try { graphql(await readBody(request), response); } catch { reply(response, { errors: [{ message: "Invalid fixture request" }] }, 400); }
  });
} }] });
await server.listen();
console.log("Synthetic webhook details fixture: http://127.0.0.1:18194/integrations/webhooks?fixtureMode=success");
