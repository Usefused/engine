# Service deletion and history

For non-owners, **More actions → Remove from Workspace** confirms a local membership change. It does not delete the Registry definition.

A service owner uses the same **More actions → Remove from Workspace** action to:

- Delete the Registry service immediately, including all versions.
- Deprecate it and choose a deletion date in their local timezone.
- Change or cancel that date before it becomes effective.

Workspace `service.manage` permission is required for mutations through Engine. Registry independently requires ownership; being able to use a public service does not confer permission to delete it. Private lifecycle records and tombstones remain owner-only.

## CLI and agent skills

The matching CLI exposes Registry-wide lifecycle actions independently of workspace membership:

```shell
fused-cli service registry lifecycle <service> --json
fused-cli service registry delete <service> --yes --json
fused-cli service registry deprecate <service> --delete-at 2027-01-01T00:00:00Z --yes --json
fused-cli service registry cancel-deletion <service> --json
```

Repeat `deprecate` with a new future timestamp to change the deadline before it becomes effective. References may be an owned slug, `@provider/slug`, or UUID; after deletion use the UUID. The timestamp requires a timezone. The CLI's `fused-workspace` agent skill documents ownership, irreversible deletion, audit retention, and reconciliation. `service remove` remains workspace-only, and the older `service deprecate --at` remains advisory.

## API

- `GET /integrations/{service_id}/lifecycle` reads the current `delete_at` and `deleted_at` timestamps.
- `PUT /integrations/{service_id}/lifecycle` accepts `{ "delete_at": "2027-01-01T00:00:00Z" }`.
- `DELETE /integrations/{service_id}/lifecycle` cancels the schedule.
- `DELETE /integrations/{service_id}` deletes the definition now.

Scheduling is a service-wide deprecation. It leaves independently deprecated versions unchanged, including when the schedule is cancelled. Consumers receive the deletion date through the existing workspace notification path and service details banner.

## Retention and propagation

Registry commits identity tombstones, payload removal and the removed changelog entry in one transaction. It retains the service identity and owner, version IDs/names, operation IDs/names, and deletion/audit metadata. It removes contract rows, schemas, documentation, connection profiles, policies, import plans/revisions and historical diff payloads. Source objects enter a durable targeted cleanup queue; objects still referenced by another live service are preserved. Failed object deletions retain a retry lease.

The Registry deadline worker runs every 15 seconds. Engines normally poll the existing changelog every 30 seconds, with `FUSED_CHANGELOG_POLL_INTERVAL` retaining its override. Immediate deletion therefore removes Registry content immediately; connected Engines stop subsequent calls when they receive the removal. Calls already dispatched to a provider are not recalled.

An Engine rejects calls after a received deadline even while disconnected. It evicts cached schemas, but waits for Registry confirmation before permanently purging its stored snapshot: a cancellation or reschedule may have occurred while it was offline. On reconnect, it applies the newest lifecycle event, avoiding premature deletion from an obsolete schedule. Confirmed removal also clears workspace membership and admitted contract payloads, retaining names for Activity and analytics. Existing app configurations and historical execution receipts remain intact.

Both the Registry and Engine serialize contract admission against deletion so an in-flight import or refresh cannot recreate a confirmed deleted definition. These changes require updated Registry and Engine deployments; older Engines do not enforce the new lifecycle receipts.
