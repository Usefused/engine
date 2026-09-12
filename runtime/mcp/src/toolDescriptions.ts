import { SESSION_AGENT_RULE } from "./sessionContract.js";

export const SEARCH_DOCS_TOOL_DESCRIPTION =
  "Find callable operations. Use query for ranked intent search, no arguments to browse, operationId for exact detail, and operationId plus section for lazy detail. Execute only when execution_ready=true; otherwise run next_action. For physical operations, params_schema is the flat call() object. Follow that operation's pagination guidance.";

export const SEARCH_DOCS_ARGUMENT_DESCRIPTIONS = {
  query: "Concise natural-language intent for ranked operation search.",
  operationId: "Exact public operation ID for detail or section retrieval.",
  limit: "Maximum ranked results; defaults to 3.",
  section: "Exact advertised section. Use params_schema for the flat physical call shape and definitions with schemaPath for shared references.",
  schemaPath: "Optional RFC 6901 JSON Pointer within the selected section.",
} as const;

export const EXECUTE_TOOL_DESCRIPTION =
  SESSION_AGENT_RULE +
  " Run TypeScript that invokes only operations discovered with execution_ready=true through await call(). Physical calls follow params_schema; Unified calls use {input,targets,selectors?,pagination?,idempotencyKey?}. Follow exact pagination guidance, await every call, and return the final value. A timeout or unknown outcome does not prove a mutation failed; never replay it automatically.";

export const EXECUTE_ARGUMENT_DESCRIPTIONS = {
  outputBudgetBytes: "Visible JSON byte budget for this execution and retained-result continuations.",
  script: "TypeScript ending with return. Invoke operations through await call(); current-session state helpers are session.get, session.set, and session.page. Never pass a session ID.",
} as const;

export const MCP_SERVER_INSTRUCTIONS = [
  "This server exposes exactly two tools: search_docs and execute.",
  SESSION_AGENT_RULE,
  "Use search_docs before execute. Query with one concise intent; use no arguments only to browse, exact operationId for known detail, and the returned next_action when execution_ready=false. Prefer a complete Unified operation when it covers the goal.",
  "Inside execute, route provider calls only through await call(). The Engine supplies authentication, connected-user identity, and resource routing; never invent or pass credentials, auth selectors, fused_end_user_ref, or fused_resource_id in call params.",
  "Physical calls use the returned flat params_schema. Unified calls use {input,targets,selectors?,pagination?,idempotencyKey?} with dependency-closed targets. Use physical pagination options only when exact operationId detail permits them. Never reuse policy across operations.",
  "Await every call and end with return. Execution limits cover calls, delays, and serialization. A timeout or unknown outcome does not undo accepted provider actions; never replay mutations automatically.",
  "Use decodeBase64/encodeBase64 for UTF-8 and atob for standard-base64 binary. Node Buffer, fetch, require, and process are unavailable.",
  "Follow structured recovery exactly. Run supplied next_request unchanged; provider_execution=complete means do not repeat the provider call. A new connection cannot read retained results from an earlier session.",
  "Return only needed fields. Use session.page for retained arrays and session.get for narrower inspection; adjust the projection instead of replaying a completed provider operation.",
].join(" ");
