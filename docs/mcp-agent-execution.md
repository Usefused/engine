# MCP execution for agents

Fused exposes two tools: `search_docs` and `execute`. Use discovery once to find
the operation and its inputs, then reuse that information for subsequent calls.

Fused supplies this guidance through its tool descriptions and initialization
instructions; users do not need to edit their model's system prompt. Search one
operation intent at a time rather than putting the whole workflow into a query.
After discovering the required schemas, compose multi-step work in one script,
using local variables for intermediate results. Stop on uncertain writes,
reconcile them with documented reads, then continue only unattempted steps.

Clients can optionally fetch the `fused-workflows` prompt with `prompts/get`, or
read `fused://guides/workflows` through `resources/read`. Both return the same
workflow guide. Native catalogues retain provider-owned prompts, resources and
selected webhook events alongside this guide. The guide adds no execution tool
or provider permission and follows the existing app authorization boundary.

The common descriptions, instructions and guide live in
[`guidance.json`](../runtime/mcp/src/guidance.json), consumed by both the Node
runtime and modern Engine transport. Transport-specific session instructions
remain separate. Detailed guidance is fetched on demand rather than repeated
with every operation response.

## Discover and run an operation

```json
{"query":"list posts"}
```

When `execution_ready` is true, the returned `params_schema` is sufficient to
construct the request. An additional exact lookup is unnecessary. A compact
`response_summary`, when available, describes the provider's declared response
type and field names; it does not replace the full response schema or guarantee
the contents of a particular response. Empty-input operations include an
executable example. Required values always come from the task and the schema.

Call `execute` with the discovered ID and flat parameters:

```json
{
  "operationId":"posts",
  "params":{"status":"all"},
  "select":["title","slug","status"],
  "saveAs":"posts"
}
```

`params` defaults to `{}`. `select` optionally chooses top-level fields from an
object or each object in an array. It changes the visible result, not the
provider request. Primitive values remain unchanged. For nested projections,
use a script. `saveAs` optionally saves the full result before projection.

`search_docs` exact lookup also returns the callable schema without repeating
raw parameters, request documents and every response schema. Those remain
available with `section`, such as `response:200` or `request`, and `schemaPath`.
`schema_status.complete` describes full documentation completeness;
`execution_ready` describes whether the callable input schema was included.
Follow `next_action` when that schema is too large to include whole.

## Read saved data or compose a workflow

Read saved data without rediscovery or another provider request:

```json
{"script":"return session.get('posts').find(post => post.slug === 'unified-apps');"}
```

Use scripts for multi-step workflows, nested projections, or optional pagination
bounds. Await provider calls and return the answer:

```js
const posts = await call("posts", { status: "all" });
session.set("posts", posts);
return posts.filter(post => post.status === "published")
  .map(post => ({ title: post.title, slug: post.slug }));
```

Choose either `operationId` or `script`. `params`, `select` and `saveAs` belong
only to direct execution. Conflicting modes fail before any provider call.
Authentication, selected operations, bucket routing and authorization remain
Engine-owned on both paths. Direct execution uses the existing sandbox call
path, timeouts, cancellation and result-size limits.

State is temporary. Compatibility connections own it implicitly. Modern clients
must include the returned `stateHandle` when reading saved state or following a
retained-result request. A handle is not a stored-value key. Large results retain
the existing bounded preview and exact `next_request`; follow it instead of
repeating the provider call. A new connection cannot recover prior saved data.

## Errors and pagination

Return data for inspection. Throwing it deliberately produces an execution
error and loses normal successful-result delivery. Script failures before any
provider attempt report `not_started`. Failures after all started calls returned
success report `complete` and still forbid replay. Unresolved or failed sibling
calls retain conservative `unknown` outcomes. No mode automatically retries a
provider mutation.

Engine pagination runs automatically when configured. Exact operation lookup is
needed only when requesting a lower caller page bound. Use the existing script
form `call(operationId, params, {pagination:{maxPages:N}})` when that operation's
exact guidance permits it. Provider page-size inputs are separate from this
control. Existing HTTP rate and concurrency limits are unchanged.

## Reproducible efficiency checks

Run from `runtime/mcp`:

```sh
npm test -- src/agentEfficiency.test.ts
npm test -- src/workflowEfficiency.test.ts
npm test
npm run typecheck
npm run build
```

The task scenarios execute the real discovery, request admission, sandbox,
projection and saved-state code with synthetic provider responses. Tokens are
counted using `js-tiktoken` and `o200k_base`, including serialized initialization
instructions, tool schemas, requests and results. The frozen baseline contains
public tool context from commit `172d638`; no credentials or customer data are
included. Token counts exclude model reasoning, user prompts and host-specific
message framing.

| Scenario | Previous tokens | Current tokens | Tool calls, previous → current | Provider calls, previous → current |
| --- | ---: | ---: | ---: | ---: |
| Fixed tool context | 1,100 | 835 | — | — |
| Read titles and status | 1,419 | 1,076 | 2 → 2 | 1 → 1 |
| Shape probe followed by data exception | 1,717 | 1,086 | 3 → 2 | 2 → 1 |

The first read baseline already uses an efficient script. The inspection
scenario reproduces the reported debug-by-throwing pattern; it is not a claim
that every previous agent needed those extra calls. Assertions enforce correct
answers, provider-call counts, at least 24% less fixed context, at least 20% less
read-task context, and at least 30% less inspection-task context. Additional
tests cover field projection, saved reads, large-result continuation, mode
conflicts, quoted inputs, uncertain outcomes, and transport authorization.

These are deterministic contract regressions, not live-model success-rate
measurements. A separate [fresh-context model evaluation](../runtime/mcp/evals/results/2026-10-23/README.md)
records the first model-generated calls and host-reported usage against both
versions. All eight runs completed correctly, including lost-response recovery,
but the revised contract did not reduce call counts. Across three ordinary runs
per version, it used about 8.5% fewer total tokens and more uncached input
tokens; this does not establish a billing-cost saving.

A subsequent [server-guidance comparison](../runtime/mcp/evals/results/2026-10-23-guidance/README.md)
isolated the workflow guidance changes against a snapshot of the already
simplified runtime. Its final ordinary runs averaged 5.67 MCP calls instead of
9.00 and about 31% fewer total reported tokens. All final runs completed,
including recovery without duplicate writes. The initial incomplete recovery
trial is retained in the report. Both evaluations are small diagnostic samples
on synthetic providers, not general performance guarantees.

### Writing a workflow

The workflow regression uses three discovered operations to create reminder
drafts for overdue invoices. The script checks invoice status and due dates,
looks up customer contact preferences, skips excluded customers, and supplies
each customer's email and invoice amount to the draft operation. It returns
draft IDs and skipped invoice IDs. All provider responses and writes are
synthetic; no messages are sent.

The executable example is
[`reminder-workflow.js`](../runtime/mcp/testdata/reminder-workflow.js).
Its date is fixed to make the regression reproducible. The test validates
provider inputs, resulting drafts, exclusions, empty queues, saved follow-up
reads, an accepted write whose response is lost, and a final summary failure.
The saved progress records acknowledged drafts and the pending invoice so that
an uncertain write can be inspected without repeating it. This is temporary
session state, not durable recovery or a transaction.

| Strategy | MCP calls | Provider calls | Serialized context tokens |
| --- | ---: | ---: | ---: |
| Discover three operations, then execute each step separately | 11 | 8 | 1,973 |
| Same discovery, then execute one workflow script | 4 | 8 | 1,774 |

This compares two strategies on the current runtime, not an old release against
the new one. Both perform identical provider work. The script saves seven MCP
round trips and about 10% of serialized context tokens, including its source
and comments. These counts include the user task and use the same tokenizer as
the checks above; they do not measure model reasoning or actual billed usage.

## Future Skills transport

MCP's official [Skills extension](https://modelcontextprotocol.io/extensions/skills/overview)
defines `skills/list`, `skills/get`, and skill-file retrieval through Resources.
It requires client support; ordinary prompts and resources do not themselves
constitute support for that extension.

Fused does not yet advertise `io.modelcontextprotocol/skills`. The shared guide
provides the content source for a future `SKILL.md`, with Agent Skills
frontmatter and a stable skill URI. Before advertising the extension, implement
both discovery methods, publish a complete file manifest with byte sizes and
SHA-256 digests computed from the served bytes, and verify authorization and
content parity across transports. Clients that lack Skills support will retain
the existing two tools, prompt and resource. Do not duplicate an independent
workflow manual in the future skill or silently activate provider-authored
skills.
