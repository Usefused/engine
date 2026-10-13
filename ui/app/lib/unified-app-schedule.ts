export interface ScheduleInput {
  id: string;
  name: string;
  input: unknown;
  cron: string;
  timezone: string;
  run_at: string | null;
  enabled: boolean;
  revision: number;
}

export interface UnifiedAppSchedule extends ScheduleInput {
  next_run_at: string | null;
  last_execution_id: string | null;
  last_app_id: string | null;
  last_status: string;
  last_started_at: string | null;
}

/** Excludes worker-owned fields from edits and pause/resume requests. */
export function scheduleInput(item: UnifiedAppSchedule): ScheduleInput {
  return { id: item.id, name: item.name, input: item.input, cron: item.cron, timezone: item.timezone, run_at: item.run_at, enabled: item.enabled, revision: item.revision };
}

/** Presents an absolute one-time timestamp in the browser's local datetime control. */
export function localScheduleTime(value: string): string {
  const date = new Date(value);
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

/** Rejects nonexistent daylight-saving wall times instead of silently moving a one-time run. */
export function absoluteScheduleTime(value: string): string {
  const date = new Date(value);
  // Browsers normalize nonexistent local times; require the selected wall time to survive round-trip.
  if (!Number.isFinite(date.getTime()) || localScheduleTime(date.toISOString()) !== value) throw new Error("Choose a valid local date and time.");
  return date.toISOString();
}

/** Retains sub-minute precision when editing another field on a schedule created through the CLI/API. */
export function preserveScheduleTime(value: string, original: string | null): string {
  // The minute-only browser control must not silently move an unchanged occurrence earlier.
  if (original && localScheduleTime(original) === value) return original;
  return absoluteScheduleTime(value);
}
