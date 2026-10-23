# Fresh-context workflow evaluation

This opt-in evaluation runs a live model against Fused's real two-tool MCP
runtime. It is separate from the deterministic Vitest suite and consumes model
usage through the existing Codex login.

```sh
node evals/live-workflow.mjs /absolute/path/to/results
```

To isolate a guidance change against an already built runtime snapshot, pass its
CommonJS bundle as a third argument:

```sh
node evals/live-workflow.mjs /absolute/path/to/results /absolute/path/to/before.cjs
```

The snapshot's SHA-256 is recorded as the baseline identity. Preserve all trial
results, including incomplete tasks, when revising guidance after an evaluation.

Run from `runtime/mcp`, with its dependencies installed and `codex` available.
The runner builds both the current runtime and baseline commit
`172d638007d5ba2843d2125738cb711319dbdb8a` using the same installed dependencies.
It does not change the working tree or install another dependency set.

## What the model receives

- The natural-language task in `billing-fixture.mjs`.
- The tested runtime's MCP initialization instructions.
- Its actual `search_docs` and `execute` descriptions and input schemas.
- The unmodified text/error status returned by its own MCP calls.

Each run uses a new ephemeral conversation, an empty working directory, no
selected environments, and no selected capability roots. Repository instruction
files, memory, plugins, apps, shell tools, browsing and delegation are disabled.
The run aborts if the host reports loading instruction files. The fixture,
provider state, grading criteria, earlier conversation, and reference script
are never placed in the model prompt. The runner supplies no solution hints or
corrective messages during the task.

The model host uses Codex's
[app-server dynamic tool protocol](https://learn.chatgpt.com/docs/app-server)
to forward the two tools through an MCP SDK client to the actual runtime's
stdio server. This tests discovery and execution, but not a production Engine,
real billing credentials, provider authorization, or an entire MCP host UI.
Codex's own host context and protocol framing may still contribute to usage;
the evaluation does not claim that its complete model request consists solely
of Fused text.

## Experiment and grading

Three paired ordinary runs vary account identifiers and amounts. A fourth pair
drops the HTTP response after the second accepted draft. Baseline/current order
alternates. Every run has a fresh account and MCP session. The first run selects
the host's configured default model; subsequent runs pin that model. Reports
record the model and reasoning effort so comparisons can be checked.

The hidden provider records attempts and accepted writes. The oracle checks
whether every eligible invoice received a draft, whether excluded invoices were
untouched, and whether any invoice received duplicate drafts. Effects are
graded independently of the model's claims. Review the saved final answer as
well: effect completion alone does not establish that the report is accurate.

Limits are 20 tool calls and 180 seconds per run. Setup failures are recorded
separately and stop the experiment. The runner never retries an agent task or
changes its prompt to obtain a passing result.

Reports contain tool arguments, responses, generated scripts, final messages,
provider effects, elapsed timestamps, and host-reported input, cached-input,
output, reasoning-output and total token counts. Total usage sums model
requests, including repeated context; it differs from tokenizing a transcript
once. Cached input is reported separately. These are usage counters, not a
price quote or a billing invoice.

Three ordinary runs per variant are a small diagnostic sample. They can reveal
call patterns, errors and counterexamples, but cannot establish a general
success rate or reliable savings across tasks, models and hosts.
