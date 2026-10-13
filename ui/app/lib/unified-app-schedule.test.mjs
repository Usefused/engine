import assert from "node:assert/strict";
import test from "node:test";
import { absoluteScheduleTime, localScheduleTime, scheduleInput } from "./unified-app-schedule.ts";

// Explicit mutation projection cannot send worker-owned fields back as editable configuration.
test("schedule edits exclude execution ownership and preserve the reviewed revision", () => {
  const item = { id: "schedule", name: "Daily", input: { customer: "fixture" }, cron: "0 9 * * *", timezone: "UTC", run_at: null, enabled: false, revision: 4, last_status: "running", last_execution_id: "execution", last_app_id: "app", next_run_at: "future", last_started_at: "past" };
  const result = scheduleInput(item);
  assert.equal(result.revision, 4);
  assert.equal(result.enabled, false);
  assert.equal("last_status" in result, false);
  assert.equal("last_execution_id" in result, false);
});

// Date conversion preserves local wall time and rejects daylight-saving gaps instead of silently shifting runs.
test("one-time dates round-trip and reject nonexistent local hours", () => {
  const previous = process.env.TZ;
  process.env.TZ = "Europe/London";
  try {
    assert.equal(absoluteScheduleTime("2026-10-12T09:00"), "2026-10-12T08:00:00.000Z");
    assert.equal(localScheduleTime("2026-10-12T08:00:00.000Z"), "2026-10-12T09:00");
    assert.throws(() => absoluteScheduleTime("2026-03-29T01:30"), /valid local date/);
    assert.throws(() => absoluteScheduleTime(""), /valid local date/);
  } finally {
    // Restore process-wide timezone so unrelated UI tests keep their own environment.
    if (previous === undefined) delete process.env.TZ;
    else process.env.TZ = previous;
  }
});

// Editing a label or input must preserve the exact time saved through the CLI.
test("one-time edits preserve seconds until the displayed date changes", async () => {
  const { preserveScheduleTime } = await import('./unified-app-schedule.ts');
  const original = '2026-12-01T09:00:45.123Z';
  assert.equal(preserveScheduleTime(localScheduleTime(original), original), original);
  assert.equal(preserveScheduleTime(localScheduleTime('2026-12-02T09:00:00Z'), original), '2026-12-02T09:00:00.000Z');
});
