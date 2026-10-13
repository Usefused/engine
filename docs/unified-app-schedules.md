# Scheduled Unified App runs

Open a Unified App and select **Schedules** to create a one-time or recurring run with saved JSON input. App managers can create, edit, pause, resume, and delete schedules. Ordinary app readers and execution tokens cannot read saved inputs or manage schedules.

The input editor is a compact JSON code box with syntax highlighting, line numbers, formatting, a previewable schema-generated example, and the full input schema. It validates syntax, required properties, nested types, enums, and constraints against the active app version while you edit, with property paths for errors. Known validation errors block saving; values are never coerced, defaulted, or removed. If the schema cannot be loaded or checked in the browser, the editor reports that explicitly. Engine remains authoritative when enabling and executing schedules.

Choose a date and time for a one-time run. The UI labels the browser's timezone. For recurrence, enter a five-field cron expression and an IANA timezone:

| Expression | Runs |
| --- | --- |
| `0 9 * * *` | Every day at 09:00 |
| `0 9 * * 1-5` | Weekdays at 09:00 |
| `*/15 * * * *` | Every 15 minutes |
| `0 9 1 * *` | The first day of each month at 09:00 |

Fields are minute, hour, day of month, month, and weekday. Seconds, `@every`, and embedded timezone directives are not supported. When both day-of-month and weekday are restricted, cron matches either field. Local times that do not exist during a daylight-saving change are skipped; repeated local times can run twice. Use `UTC` for recurrence without daylight-saving changes.

Schedules are stored in Engine's PostgreSQL database. Input is encrypted with the Engine master key. Provider credentials remain in the app's configured buckets; schedules do not store an execution token. Input must be valid JSON of at most 512 KiB and match the active app's input schema when enabling the schedule. Authored validation also runs at execution time.

## Execution behavior

- Schedules belong to the app, continue after their creator signs out, and follow its active version. Review saved inputs when deploying or promoting a different input contract. No schedule can run while the family has no active version or is archived.
- Engine must be running with its Unified App worker available. Scheduling starts automatically with Engine; no separate cron service or environment flag is required. Polling occurs every five seconds, and execution capacity may delay a run. This is not a real-time deadline guarantee.
- Each occurrence is reserved with its execution record in one database transaction. Multiple Engine replicas cannot dispatch the same occurrence. A schedule does not overlap its previous run.
- A missed one-time run executes when Engine becomes available. Missed recurring times are combined into one catch-up run, then advance to the next future occurrence. Resuming a recurring schedule starts from its next future time without replaying paused occurrences.
- Failed runs are not automatically retried. Recurring schedules still reach their next scheduled occurrence. Runs with uncertain outcomes pause the schedule. Abandoned runs are marked indeterminate after five minutes; review provider state before resuming. Engine does not promise exactly-once provider side effects.
- A completed one-time schedule has no next run. To run again, edit it with a future time. Changes and deletion are rejected while that schedule is running. Editing preserves history, and deletion removes only future automation.
- Scheduled runs use the ordinary Requests and Analytics history, shared execution capacity, account suspension, heartbeat verification, buckets, and provider policies. Each invocation has a one-minute overall deadline. A family can retain up to 100 schedules.

## CLI

Requires an Engine and CLI build containing scheduling support. Use the ordinary saved Engine login. An explicit app name takes precedence over `-f`; omit the name to select the app from a Unified App YAML config.

```sh
# One-time run, with an explicit timestamp offset.
fused-cli unified-app schedule create "Billing report" \
  --name "December report" \
  --at '2026-12-01T09:00:00Z' \
  --input-file ./report-input.json

# Recurring local-time run.
fused-cli unified-app schedule create "Billing report" \
  --name "Weekday report" \
  --cron '0 9 * * 1-5' \
  --timezone Europe/London \
  --input-file ./report-input.json

fused-cli unified-app schedule list "Billing report"
fused-cli unified-app schedule update "Billing report" --id SCHEDULE_ID --input-file ./revised-input.json
fused-cli unified-app schedule pause "Billing report" --id SCHEDULE_ID
fused-cli unified-app schedule resume "Billing report" --id SCHEDULE_ID
fused-cli unified-app schedule delete "Billing report" --id SCHEDULE_ID
```

Use `--input '{"month":"2026-12"}'` for inline JSON, or `--input-file -` for stdin. Prefer a file or stdin for private inputs. Omitted input defaults to `{}` on creation. Updates preserve fields whose flags are omitted. `--paused` creates a disabled schedule; `--paused=false` explicitly enables an edited one. Changing `--at` clears recurrence, and changing `--cron` clears the one-time date. CLI recurrence defaults to `UTC`.

All commands accept `--json`. A JSON list includes private saved input; default terminal output omits it. Do not automatically retry an unconfirmed creation: list schedules first using the schedule ID reported by the CLI. Update and delete requests use the current revision to detect racing edits or dispatch.

## Management API

These routes use normal Engine management authentication and require `app.unified_app.manage` on the app family. `{app_id}` identifies any retained version for management routing; scheduled execution resolves the family's active version separately.

```text
GET    /apps/{app_id}/schedules
PUT    /apps/{app_id}/schedules
DELETE /apps/{app_id}/schedules/{schedule_id}?revision=1
```

Creation uses a client-generated UUID and `revision: 0`. Update uses the revision returned by the list endpoint. Only the fields below are accepted; ownership and run status are Engine-controlled.

```json
{
  "id": "8bea5382-42dd-44c8-9dc9-7b2710a24825",
  "name": "Weekday report",
  "input": {"recipient": "report-owner"},
  "cron": "0 9 * * 1-5",
  "timezone": "Europe/London",
  "run_at": null,
  "enabled": true,
  "revision": 0
}
```

For one-time runs, set `cron` to `""` and `run_at` to an RFC3339 timestamp. A stale revision or in-flight mutation returns HTTP 409. Saved inputs never enter logs, trace attributes, or execution-event payloads. Scheduled execution records retain `sourceScheduleId` for correlation.
