import { readFileSync } from "node:fs";
import { getEncoding } from "js-tiktoken";
import { expect, it, vi } from "vitest";
import { z } from "zod";
import { Fixture, FixtureOperation } from "./fixture.js";
import { searchDocs } from "./searchDocs.js";
import { listTools } from "./metadata.js";
import { runExecuteRequest } from "./executeRequest.js";
import { SessionState } from "./sandbox.js";
import { MCP_SERVER_INSTRUCTIONS } from "./toolDescriptions.js";

const task = "As of 2026-10-19, create reminder drafts for open overdue invoices, except customers who opted out. Do not send messages. Return invoice/draft IDs and skipped invoice IDs.";
const script = readFileSync(new URL("../testdata/reminder-workflow.js", import.meta.url), "utf8");
const tokenizer = getEncoding("o200k_base");
const options = { sessionId: "workflow-test", enginePort: "1" };
const invoices = [
  { id: "i-1", customerId: "c-1", status: "open", dueDate: "2026-10-01", amountCents: 1200, currency: "usd" },
  { id: "i-2", customerId: "c-2", status: "paid", dueDate: "2026-10-01", amountCents: 3400, currency: "usd" },
  { id: "i-3", customerId: "c-3", status: "open", dueDate: "2026-10-01", amountCents: 5600, currency: "usd" },
  { id: "i-4", customerId: "c-4", status: "open", dueDate: "2026-11-01", amountCents: 7800, currency: "usd" },
  { id: "i-5", customerId: "c-5", status: "open", dueDate: "2026-10-02", amountCents: 9100, currency: "usd" },
  { id: "i-6", customerId: "c-6", status: "open", dueDate: "2026-10-03", amountCents: 2300, currency: "usd" },
];
const string = { type: "string" };
const integer = { type: "integer" };

/** Builds a synthetic imported operation with complete callable and response schemas. */
function operation(id: string, properties: Record<string, unknown>, response: Record<string, unknown>): FixtureOperation {
  return {
    operation_id: id, service_id: "billing", name: id.replace(/([a-z])([A-Z])/g, "$1 $2"), method: "POST", path: `/${id}`,
    imported_input_schema: { type: "object", properties, required: Object.keys(properties), additionalProperties: false },
    pagination: { supported: false, caller_bound_supported: false },
    responses: { "200": { description: "Result", representations: [{ media_type: "application/json", schema: {
      dialect: "https://json-schema.org/draft/2020-12/schema", raw: response,
      content_hash: `synthetic:${id}`, projection: {},
    } }] } },
  };
}

const fixture = new Fixture([
  operation("listInvoices", {}, { type: "array", items: { type: "object", properties: {
    id: string, customerId: string, status: string, dueDate: string, amountCents: integer, currency: string,
  } } }),
  operation("getCustomer", { id: string }, { type: "object", properties: { id: string, email: string, doNotContact: { type: "boolean" } } }),
  operation("createReminderDraft", { invoiceId: string, to: string, amountCents: integer, currency: string }, { type: "object", properties: { id: string } }),
], {}, { name: "billing", title: "Billing workflow", version: "1", description: "Synthetic billing operations; no external resources." });

const expected = {
  drafts: [{ invoiceId: "i-1", draftId: "d-1" }, { invoiceId: "i-5", draftId: "d-2" }, { invoiceId: "i-6", draftId: "d-3" }],
  skipped: ["i-2", "i-3", "i-4"], pending: null,
};
const draftSchema = z.object({ invoiceId: z.string(), to: z.string().email(), amountCents: z.number().int(), currency: z.string() }).strict();

/** Models provider validation and accepted mutations independently of the script under test. */
function backend(failAfterDraft = 0, source = invoices) {
  const drafts: Array<z.infer<typeof draftSchema> & { id: string }> = [];
  // Reject invented inputs and operations rather than letting a permissive mock hide workflow mistakes.
  const call = vi.fn(async (_options: unknown, id: string, params: unknown) => {
    switch (id) {
      case "listInvoices":
        z.object({}).strict().parse(params);
        return structuredClone(source);
      case "getCustomer": {
        const input = z.object({ id: z.string() }).strict().parse(params);
        // Only customer IDs supplied by discovery results are valid workflow dependencies.
        expect(invoices.map(invoice => invoice.customerId)).toContain(input.id);
        // One customer opts out to exercise the provider-dependent exclusion.
        return { id: input.id, email: `${input.id}@example.com`, doNotContact: input.id === "c-3" };
      }
      case "createReminderDraft": {
        const input = draftSchema.parse(params);
        drafts.push({ ...input, id: `d-${drafts.length + 1}` });
        // An accepted write with a lost response must never be treated as safe to repeat.
        if (drafts.length === failAfterDraft) throw new Error("response lost after draft accepted");
        return { id: drafts.at(-1)!.id };
      }
      default:
        throw new Error(`Undiscovered operation: ${id}`);
    }
  });
  return { call, drafts };
}

/** Uses independent concise intents, as instructed by the public two-tool contract. */
function discover() {
  // Each intent returns a ready schema; another exact lookup would add no necessary inputs.
  return [["list invoices", "listInvoices"], ["get customer", "getCustomer"], ["create reminder draft", "createReminderDraft"]].map(([query, id]) => {
    const result = searchDocs(fixture, { query, limit: 1 });
    expect(result).toMatchObject({ operations: [{ operation_id: id, execution_ready: true }] });
    return { tool: "search_docs", arguments: { query, limit: 1 }, result };
  });
}

/** Counts the same public context and user task for both strategies, excluding reasoning and host framing. */
function tokens(transcript: unknown[]): number {
  return tokenizer.encode(JSON.stringify({ instructions: MCP_SERVER_INSTRUCTIONS, tools: listTools(), task, transcript }), [], []).length;
}

// A single authored workflow must honor dependent inputs and exclusions while reducing client round trips.
it("completes a branching write workflow with fewer tool calls and tokens than stepwise execution", async () => {
  const discovery = discover();
  const provider = backend();
  const result = await runExecuteRequest({ script }, options, new SessionState(), undefined, provider.call);
  expect(result.isError).toBe(false);
  expect(JSON.parse(result.text)).toEqual(expected);
  expect(provider.call).toHaveBeenCalledTimes(8);
  expect(provider.drafts).toEqual([
    { id: "d-1", invoiceId: "i-1", to: "c-1@example.com", amountCents: 1200, currency: "usd" },
    { id: "d-2", invoiceId: "i-5", to: "c-5@example.com", amountCents: 9100, currency: "usd" },
    { id: "d-3", invoiceId: "i-6", to: "c-6@example.com", amountCents: 2300, currency: "usd" },
  ]);
  const scripted = [...discovery, { tool: "execute", arguments: { script }, result: JSON.parse(result.text) }];
  const stepwise: unknown[] = [...discovery];
  const separate = backend();
  const session = new SessionState();
  // Replay only the deterministic comparison against a fresh synthetic account, never the failed account.
  for (const [, operationId, params] of provider.call.mock.calls) {
    const args = { operationId, params };
    const response = await runExecuteRequest(args, options, session, undefined, separate.call);
    expect(response.isError).toBe(false);
    stepwise.push({ tool: "execute", arguments: args, result: JSON.parse(response.text) });
  }
  expect(separate.drafts).toEqual(provider.drafts);
  expect(scripted).toHaveLength(4);
  expect(stepwise).toHaveLength(11);
  expect(tokens(scripted)).toBeLessThan(tokens(stepwise));
  process.stdout.write(JSON.stringify({ scenario: "branching reminder workflow", stepwise_tool_calls: stepwise.length, scripted_tool_calls: scripted.length, provider_calls: provider.call.mock.calls.length, stepwise_tokens: tokens(stepwise), scripted_tokens: tokens(scripted) }) + "\n");
});

// Empty work queues should finish normally, without inventing customers or sending any writes.
it("returns an empty summary without writes when there are no invoices", async () => {
  const provider = backend(0, []);
  const result = await runExecuteRequest({ script }, options, new SessionState(), undefined, provider.call);
  expect(result.isError).toBe(false);
  expect(JSON.parse(result.text)).toEqual({ drafts: [], skipped: [], pending: null });
  expect(provider.call).toHaveBeenCalledTimes(1);
  expect(provider.drafts).toHaveLength(0);
});

// Exclusions must still prevent writes when every returned invoice is ineligible.
it.each([
  { source: [invoices[1], invoices[3]], calls: 1, skipped: ["i-2", "i-4"] },
  { source: [invoices[2]], calls: 2, skipped: ["i-3"] },
])("avoids writes for an entirely excluded queue: $skipped", async ({ source, calls, skipped }) => {
  const provider = backend(0, source);
  const result = await runExecuteRequest({ script }, options, new SessionState(), undefined, provider.call);
  expect(result.isError).toBe(false);
  expect(JSON.parse(result.text)).toEqual({ drafts: [], skipped, pending: null });
  expect(provider.call).toHaveBeenCalledTimes(calls);
  expect(provider.drafts).toHaveLength(0);
});

// Follow-up inspection must use the saved receipt, not repeat the write workflow.
it("answers a follow-up from the completed receipt without any provider calls", async () => {
  const provider = backend();
  const session = new SessionState();
  await runExecuteRequest({ script }, options, session, undefined, provider.call);
  provider.call.mockClear();
  const result = await runExecuteRequest({ script: 'return session.get("reminders").drafts;' }, options, session, undefined, provider.call);
  expect(result.isError).toBe(false);
  expect(JSON.parse(result.text)).toEqual(expected.drafts);
  expect(provider.call).not.toHaveBeenCalled();
});

// Partial completion is not transactional: preserve known progress and identify the uncertain write without replay.
it("stops after an accepted write loses its response and inspects progress without duplication", async () => {
  const provider = backend(2);
  const session = new SessionState();
  const result = await runExecuteRequest({ script }, options, session, undefined, provider.call);
  expect(result.isError).toBe(true);
  expect(JSON.parse(result.text)).toMatchObject({ provider_execution: "unknown", recovery_action: "do_not_replay", automatic_replay: false });
  expect(provider.drafts.map(draft => draft.invoiceId)).toEqual(["i-1", "i-5"]);
  expect(provider.call).toHaveBeenCalledTimes(6);
  const progress = await runExecuteRequest({ script: 'return session.get("reminders");' }, options, session, undefined, provider.call);
  expect(progress.isError).toBe(false);
  expect(JSON.parse(progress.text)).toEqual({ drafts: [expected.drafts[0]], skipped: ["i-2", "i-3", "i-4"], pending: "i-5" });
  expect(provider.call).toHaveBeenCalledTimes(6);
  expect(provider.drafts).toHaveLength(2);
});

// A local formatting failure after all writes succeeded must not invite rerunning those writes.
it("reports completed provider work when only the final summary fails", async () => {
  const provider = backend();
  const session = new SessionState();
  const brokenSummary = script.replace("return progress;", 'throw new Error("summary formatting failed");');
  const result = await runExecuteRequest({ script: brokenSummary }, options, session, undefined, provider.call);
  expect(JSON.parse(result.text)).toMatchObject({ provider_execution: "complete", recovery_action: "do_not_replay", automatic_replay: false });
  const progress = await runExecuteRequest({ script: 'return session.get("reminders");' }, options, session, undefined, provider.call);
  expect(JSON.parse(progress.text)).toEqual(expected);
  expect(provider.call).toHaveBeenCalledTimes(8);
  expect(provider.drafts).toHaveLength(3);
});
