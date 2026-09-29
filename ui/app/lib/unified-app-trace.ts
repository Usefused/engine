import type { EngineExecutionEventEntry } from "./api";

const phases = [
  ["compilation", "Prepare executable"],
  ["initialization", "Initialize app"],
  ["input_validation", "Validate input"],
  ["execute", "Run TypeScript"],
  ["output_validation", "Validate output"],
] as const;

// unifiedAppTraceRows admits only measured hosted phases, preserving missing and sub-millisecond values.
export function unifiedAppTraceRows(event: Pick<EngineExecutionEventEntry, "timings" | "latency_ms">) {
  return phases.flatMap(([name, label]) => {
    const timing = event.timings?.find((entry) => entry.name === `unified_app_${name}`);
    // Invalid or absent evidence must never appear as a successful zero-duration stage.
    if (!timing || !Number.isFinite(timing.duration_ms) || timing.duration_ms < 0) return [];
    return [{ name, label, duration_ms: timing.duration_ms, percent: Math.min(100, timing.duration_ms / Math.max(event.latency_ms, 1) * 100) }];
  });
}
