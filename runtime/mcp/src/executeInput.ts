import { z } from "zod";
import { EXECUTE_MIN_OUTPUT_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";
import { EXECUTE_ARGUMENT_DESCRIPTIONS } from "./toolDescriptions.js";

/** Both wire transports advertise this shape; cross-field admission happens before provider execution. */
export const EXECUTE_INPUT_SHAPE = {
  operationId: z.string().min(1).max(256).optional().describe(EXECUTE_ARGUMENT_DESCRIPTIONS.operationId),
  params: z.record(z.unknown()).optional().describe(EXECUTE_ARGUMENT_DESCRIPTIONS.params),
  select: z.array(z.string().min(1).max(256)).min(1).max(64).optional().describe(EXECUTE_ARGUMENT_DESCRIPTIONS.select),
  saveAs: z.string().min(1).max(128).optional().describe(EXECUTE_ARGUMENT_DESCRIPTIONS.saveAs),
  script: z.string().min(1).optional().describe(EXECUTE_ARGUMENT_DESCRIPTIONS.script),
  outputBudgetBytes: z.number().int().min(EXECUTE_MIN_OUTPUT_BYTES).max(EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes).optional()
    .describe(EXECUTE_ARGUMENT_DESCRIPTIONS.outputBudgetBytes),
};
