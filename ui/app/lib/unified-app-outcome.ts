import type { EngineExecutionEventEntry } from "./api";

const failures = {
  compilation: { label: "Prepare executable", message: "The app could not be compiled.", action: "Review the app source." },
  initialization: { label: "Initialize app", message: "The app could not finish initialization.", action: "Review the app setup." },
  input_validation: { label: "Validate input", message: "The request did not match the app's input schema.", action: "Check the required fields and types." },
  execute: { label: "Run TypeScript", message: "The app stopped while running its code.", action: "Review the app logic and operation outcomes." },
  output_validation: { label: "Validate output", message: "The result did not match the app's output schema.", action: "Check the returned fields and types." },
} as const;

type OutcomeEvent = Pick<EngineExecutionEventEntry, "status" | "failure_code" | "failure_reason">;

// unifiedAppFailureStage accepts only explicit Engine-owned evidence, never guesses from the last timing.
export function unifiedAppFailureStage(event: OutcomeEvent): keyof typeof failures | undefined {
  // Successful receipts cannot acquire a failure label from stale metadata.
  if (event.status === "success") return undefined;
  for (const name of Object.keys(failures) as Array<keyof typeof failures>) {
    // Exact matches prevent arbitrary exception strings from becoming receipt content.
    if (event.failure_reason === `unified_app_${name}_failed`) return name;
  }
  // Older validation failures already carry an authoritative phase in their public code.
  if (event.failure_code === "input_validation_failed") return "input_validation";
  if (event.failure_code === "output_validation_failed") return "output_validation";
  return undefined;
}

// unifiedAppOutcome provides useful fixed explanations without requesting private diagnostic payloads.
export function unifiedAppOutcome(event: OutcomeEvent) {
  // Success is based on the durable root outcome, even when authored code handled a failed provider call.
  if (event.status === "success") return { title: "Execution completed", message: "The app returned a valid result.", action: "" };
  // Interrupted provider work must retain uncertainty rather than suggesting a safe automatic retry.
  if (event.failure_code === "execution_interrupted") return { title: "Execution interrupted", message: "Some operation outcomes may be unknown.", action: "Check provider state before retrying." };
  // A timeout is an execution limit, not evidence of an authored exception in the last recorded stage.
  if (event.failure_code === "execution_timeout") return { title: "Execution timed out", message: "The app exceeded its execution time limit.", action: "Review the execution trace." };
  const stage = unifiedAppFailureStage(event);
  // Historical receipts remain useful without inventing a root cause or revealing unknown error text.
  if (!stage) return { title: "Execution failed", message: "Failure stage not recorded.", action: "Review the recorded operation outcomes." };
  return { title: `Failed · ${failures[stage].label}`, message: failures[stage].message, action: failures[stage].action };
}
