# Server-owned guidance comparison — 23 October 2026

The final comparison used eight fresh GPT-6 Astra conversations at high
reasoning effort: three ordinary tasks and one lost-response task per variant.
Every scored final run completed correctly, included the requested IDs in its
answer, respected contact exclusions, and created no duplicate drafts.

The baseline was a saved bundle from immediately before the guidance change,
with direct execution and compact schemas already implemented. Its SHA-256 is
recorded in every run. The new bundle changed public server instructions and
tool descriptions to teach concise discovery, script composition, and recovery
that continues only unattempted steps. It also offers an optional prompt and
guide resource; these were **not loaded by the evaluation agent**. The live
comparison therefore measures the essential tool/server guidance, not an extra
guide injected into the model's context.

## Ordinary tasks

| Mean of three runs | Before guidance change | Final guidance |
| --- | ---: | ---: |
| MCP calls | 9.00 | 5.67 |
| Provider calls | 8.67 | 9.00 |
| Total reported tokens | 56,455 | 38,981 |
| Uncached input tokens | 12,437 | 9,621 |
| Output tokens | 541 | 688 |
| Correct completions | 3/3 | 3/3 |

MCP calls fell about **37%** and total reported usage about **31%** on this task.
Uncached input also fell, while output grew because the model wrote more code
inside its workflow script. Provider work did not decrease: reads to check
existing drafts account for the small difference in provider calls. The change
reduces model/tool round trips rather than skipping required provider work.

| Account | Before: MCP calls / tokens | Final: MCP calls / tokens |
| --- | ---: | ---: |
| 11 | 8 / 72,630 | 5 / 32,830 |
| 22 | 12 / 40,168 | 6 / 42,274 |
| 33 | 7 / 56,567 | 6 / 41,838 |

One pair used more tokens despite fewer calls. Token counts include repeated
context, host framing and cache effects; call counts alone do not determine
model usage. These are host-reported usage counters, not billing invoices.

## Recovery and the failed first trial

The first guidance trial reduced ordinary call counts but left one task
incomplete: after confirming the uncertain second write with a read, the agent
stopped instead of creating the remaining draft. It reported the incomplete
work honestly and did not duplicate a write. All eight first-trial traces are
preserved under [trial-1](trial-1/current-44-lost-response.json).

The essential server/tool descriptions were clarified to continue only
unattempted steps after reconciliation. The same complete experiment was run
again with fresh contexts, without changing the task, fixture or grading rules.
In the final lost-response pair, both agents reconciled the uncertain write,
finished the unattempted step, and reported the result accurately:

| Variant | MCP calls | Provider calls | Total tokens | Duplicate writes |
| --- | ---: | ---: | ---: | ---: |
| Before guidance change | 10 | 10 | 66,017 | 0 |
| Final guidance | 8 | 10 | 61,816 | 0 |

See the [exact final recovery trace](final/current-44-lost-response.json).

## Reproduction and limits

Use the [evaluation runner](../../live-workflow.mjs) with the before-change
CommonJS bundle as its third argument. The [methodology](../../README.md)
describes isolation, tool forwarding and grading. No prior conversation,
repository files, expected result, or reference workflow was passed to the
model. Each session reported no instruction files and no selected environment.
The real MCP runtime handled discovery and execution against synthetic billing
providers. No production account or provider action was used.

This is a small, iteratively developed benchmark on one task family, not an
independent held-out evaluation or a general guarantee. Runtime variance was
visible even for the unchanged baseline. Additional task families, larger
catalogues and clients should be evaluated before extrapolating the savings.

The [machine-readable summary](final/summary.json) records model settings,
runtime/guidance hashes, counters and grading. Each run's JSON preserves exact
tool arguments, returned data and the final answer, without model reasoning
text. For an ordinary example, see [current-11.json](final/current-11.json).

Validation: 201 runtime tests, TypeScript checking, bundle build, and Engine MCP
and imported-capability regression tests passed. The guide's prompt and resource
return identical text; authorization, private catalogue caching, retained event
resources, and the two-tool interface remain covered by tests.
