import guidance from "./guidance.json" with { type: "json" };
import { SESSION_AGENT_RULE } from "./sessionContract.js";

// Both Engine transports and the Node runtime consume these same versioned instructions.
export const SEARCH_DOCS_TOOL_DESCRIPTION = guidance.searchDescription;
export const SEARCH_DOCS_ARGUMENT_DESCRIPTIONS = guidance.searchArguments;
export const EXECUTE_TOOL_DESCRIPTION = guidance.executeDescription;
export const EXECUTE_ARGUMENT_DESCRIPTIONS = guidance.executeArguments;
export const MCP_SERVER_INSTRUCTIONS = guidance.instructions + " " + SESSION_AGENT_RULE;
