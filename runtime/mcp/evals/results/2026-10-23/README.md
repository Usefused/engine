# Live workflow evaluation — 23 October 2026

Eight scored fresh-context runs used **GPT-6 Astra**, reasoning effort **high**:
three ordinary tasks per contract and one lost-response task per contract.
All eight completed correctly, respected exclusions, and produced no duplicate
drafts. Final answers included the created invoice/draft IDs and all skipped
invoice IDs. These observations are a small diagnostic sample, not a general
success-rate estimate.

The revised contract **did not reduce MCP calls** in these runs. It reduced
total reported token usage, but used more uncached input tokens. The experiment
therefore does not establish a billing-cost saving.

## Ordinary tasks

| Metric, mean of three runs | Previous contract | Revised contract |
| --- | ---: | ---: |
| Correct completions | 3/3 | 3/3 |
| MCP tool calls | 8.00 | 8.67 |
| Provider calls | 8.33 | 9.00 |
| Total reported tokens | 69,115 | 63,237 |
| Uncached input tokens | 13,272 | 15,009 |
| Cached input tokens | 55,168 | 47,659 |
| Duplicate writes | 0 | 0 |

Total reported usage fell about **8.5%**, while MCP calls rose about **8.3%**.
Counts include repeated context across model requests and host framing, unlike
the earlier deterministic tests that tokenized a transcript once. Cache usage
varied across runs; total-token reduction is not equivalent to cost reduction.

| Account | Previous MCP calls / total tokens | Revised MCP calls / total tokens |
| --- | ---: | ---: |
| 11 | 9 / 72,881 | 9 / 65,998 |
| 22 | 7 / 67,141 | 8 / 66,395 |
| 33 | 8 / 67,322 | 9 / 57,317 |

## What the agents actually did

Both contracts led the model to combine customer lookups in a script, but to
create the three drafts in separate executions. The revised contract used
direct `operationId` requests for these writes. It also read existing drafts
before writing in all three ordinary runs; the baseline did that in one run.
Neither contract produced the single complete workflow from the fixed-script
test. The earlier four-call result was a demonstrated execution strategy, not
observed fresh-agent behavior.

Exact generated arguments and returned results are saved in
[current-11.json](current-11.json), with the corresponding previous-contract
trace in [baseline-11.json](baseline-11.json). Every other scored run also has
its own JSON trace in this directory. These public traces omit model reasoning
text and host diagnostics; they include final answers and usage counters.

## Lost-response tasks

The synthetic provider accepted the second draft and dropped its HTTP response.
Both agents read existing drafts to confirm the uncertain write, then created
the remaining draft. Neither repeated the uncertain mutation. Both final
answers accurately explained the reconciliation.

| Contract | MCP calls | Provider calls | Total reported tokens | Duplicate writes |
| --- | ---: | ---: | ---: | ---: |
| Previous | 8 | 9 | 76,359 | 0 |
| Revised | 8 | 9 | 74,068 | 0 |

See [the revised recovery trace](current-44-lost-response.json).

## Isolation and limitations

Each run received the task, the tested runtime's public MCP instructions and
tools, and responses to its own calls. There was no conversation inheritance,
solution script, fixture access, or repository access. The host reported zero
instruction files and no selected environments. Only the two forwarded MCP
tools were called; there were no unexpected capability requests or host errors.

The harness used the real previous/current MCP runtimes with synthetic billing
providers. It did not exercise a production Engine or real billing account.
The host's default model was selected for the first run and pinned thereafter;
all recorded runs used the same model and reasoning setting. Codex host context
may contribute to the counts. The methodology and rerun command are in the
[evaluation README](../../README.md).

A setup pilot ran one baseline task successfully, then the revised runtime
failed before any model call because its CommonJS bundle was launched with a
`.js` extension under a `type=module` package. After fixing the launcher, all
eight scored runs started fresh. No prompt, task, catalogue or provider data was
changed based on the pilot. The pilot and runtime hash are recorded in
[summary.json](summary.json).

The evidence supports correct discovery, execution and recovery on this task.
It does **not** support the earlier claim that the prompt changes make fresh
agents complete workflows with fewer calls. Workflow composition guidance
still needs improvement and a separate evaluation.
