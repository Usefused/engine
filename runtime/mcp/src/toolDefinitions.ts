import { z } from "zod";
import { SEARCH_DOCS_MAX_LIMIT, isDocumentationSection } from "./searchDocs.js";
import { EXECUTE_MIN_OUTPUT_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";
import { SESSION_CONTRACT_METADATA } from "./sessionContract.js";
import { EXECUTE_ARGUMENT_DESCRIPTIONS, EXECUTE_TOOL_DESCRIPTION, SEARCH_DOCS_ARGUMENT_DESCRIPTIONS, SEARCH_DOCS_TOOL_DESCRIPTION } from "./toolDescriptions.js";

export const SEARCH_DOCS_DEFINITION = {
  title: "Search available operations",
  description: SEARCH_DOCS_TOOL_DESCRIPTION,
  inputSchema: {
    query: z
      .string()
      .max(512)
      .optional()
      .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.query),
    operationId: z
      .string()
      .max(256)
      .optional()
      .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.operationId),
    limit: z
      .number()
      .int()
      .min(1)
      .max(SEARCH_DOCS_MAX_LIMIT)
      .optional()
      .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.limit),
    section: z
      .custom(isDocumentationSection, "invalid public documentation section")
      .optional()
      .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.section),
    schemaPath: z
      .string()
      .max(2048)
      .optional()
      .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.schemaPath),
  },
};

export const EXECUTE_DEFINITION = {
  title: "Execute a script",
  description: EXECUTE_TOOL_DESCRIPTION,
  _meta: { "com.usefused/session": SESSION_CONTRACT_METADATA },
  inputSchema: {
    outputBudgetBytes: z.number().int().min(EXECUTE_MIN_OUTPUT_BYTES).max(EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes).optional()
      .describe(EXECUTE_ARGUMENT_DESCRIPTIONS.outputBudgetBytes),
    script: z
      .string()
      .describe(EXECUTE_ARGUMENT_DESCRIPTIONS.script),
  },
};
