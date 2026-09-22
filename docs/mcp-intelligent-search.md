# MCP intelligent search

MCP versions use local lexical search by default. Opt in when creating a version:

```yaml
apiVersion: fused/v1
kind: mcp
name: repository-assistant
version: "1.0.0"
description: Find and review repositories in GitHub.
bucket: default
fused-intelligent-classifier: true
services:
  github:
    version: v1
    operations: [listRepos, getRepo]
```

The equivalent creation flag is `fused-cli init repository-assistant --mcp
--fused-intelligent-classifier --description '<capability summary>'`
with the usual service and operation selection flags. The UI offers **Intelligent
search with Jev** when creating an MCP server. The boolean is immutable version
configuration; change it by publishing a successor version. Set
`fused-intelligent-classifier: false` or omit it on the successor to use local search.

Intelligent search uses Jev through Fused Registry. Search intent and authorized
operation names and descriptions are sent to Jev. Fused manages the Jev API key;
customers need no additional key. The CLI plan/apply output, UI creation control,
and exact-version detail page disclose this processing.

Only non-empty `search_docs.query` calls use the classifier. An exact
`operationId`, section lookup, or empty-query browse remains local. Jev chooses
one best matching physical or Unified operation, or no match. The result retains
query-mode schema packing and pagination guidance; it never authorizes execution
or upgrades to exact-lookup pagination controls. The Engine checks the returned
name against the token-authorized catalogue before returning any documentation.

Classifier failures produce an explicit tool error without silently switching to
local ranking. Exact lookup and browsing remain usable during a Jev or Registry
outage. Calls are cancellation-aware and bounded; no provider request is retried
automatically. Queries are limited to 4096 UTF-8 bytes and catalogues to 2048
operations. Only the first 512 UTF-8 bytes of each public description are shared.
Larger catalogues must use a narrower MCP selection or local search. Catalogues
above 254 operations use multiple Choice questions and a second decision over
batch winners, keeping every authorized candidate in consideration.

## Registry operation

Set `FUSED_CLASSIFIER_JEV_API_KEY` on the Registry process. Never place it in an
Engine config, MCP config, bucket, generated artifact, or browser. The Registry
calls the fixed TypeSafe endpoint with the pinned `jev-1.13.0` model, following
[TypeSafe's Choice contract](https://docs.typesafe.ai/primitives/choice).

Engine calls `POST /api/engine/fused-intelligent-classifier` with its existing
Fused license through the shared Registry transport. Suspended or unauthorized
accounts are rejected by the existing Engine authentication middleware. No new
customer key or unrestricted Jev proxy is introduced. This endpoint currently
supports operation selection only:

```json
{
  "intent": "find a repository",
  "operations": [
    {"operationName": "listRepos", "description": "List accessible repositories"}
  ]
}
```

A successful response contains `provider: "fused-intelligent-classifier"` and
`operationName`, which is empty for no match. Request bodies are capped at 2 MiB,
and the Registry permits at most 32 concurrent classifier requests per process.
Provider credentials, provider error bodies, operation schemas, runtime tokens,
and private Unified mappings never appear in the response. Search telemetry
retains the existing content-free search observation contract.

## Prompt operation discovery

`fused-cli prompt '<goal>'` uses the same licensed Registry endpoint to select
operations for SDK, MCP, and REST app proposals. Goal parsing still uses the
Registry's configured chat model; operation selection uses Jev. The CLI displays
the Jev disclosure before resolution and requires interactive proposal review
before applying anything.

CLI calls Engine's `classifyPromptOperation` query with service ID, exact version,
and one operation intent. Engine requires `catalogue.read`, obtains the complete
visible version's operation names and descriptions from Registry, and sends the
bounded catalogue to the existing classifier endpoint. Customers provide neither
candidate catalogues nor a Jev key. Exact operation IDs bypass inference; no-match,
outage, and names outside the catalogue fail rather than falling back to lexical
ranking. Missing operation intent requires clarification: all operations must be
explicitly requested. Event-only services retain event-only scope.

Prompt's creation-time classification does not enable intelligent runtime MCP
search. The separate MCP version setting remains opt-in.
