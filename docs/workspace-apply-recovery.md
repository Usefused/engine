# Workspace apply recovery

Workspace configuration apply uses the existing saved plan and runs synchronously. Each service's local activation, versions, connection profiles, and policy changes commit in one PostgreSQL transaction with a success receipt. Independent services continue after a proven service-specific rollback.

A partial result returns HTTP 409 with `status: partially_applied`, the original `plan_id`, and per-group `services` results. The CLI prints these outcomes and exits unsuccessfully instead of printing a completed-apply message. Successful groups remain active. The applied configuration and managed resources include only committed changes; the complete requested source remains in the saved plan until all required work finishes.

After correcting a transient failure, reuse the same config and receipt:

```sh
fused-cli workspace apply -f .fused/workspace.yaml
```

An explicit saved plan can also be selected with the existing flag:

```sh
fused-cli workspace apply --plan-id <plan-id> -f .fused/workspace.yaml
```

Engine skips groups with durable success receipts. It does not resolve newer service versions during resume. A completed plan can be repeated to recover a lost success response without replaying its writes. Reading the existing plan resource exposes `apply_results` for durable progress inspection.

Generic bucket secrets form a shared prerequisite; failure prevents dependent service work. Whole-service removals retain their composite transaction boundary. Local group failure preserves the service's earlier committed configuration. Leases and database-owned configuration revisions stop obsolete workers or retries from overwriting later workspace changes. When state has changed, create a fresh reviewed plan instead of resuming stale intent.

Registry publication, visibility, deprecation, and archiving are external writes. They run after all local groups succeed. Engine persists a `registry` dispatch marker before the first external mutation. A missing completion receipt remains `needs_reconciliation` and is never automatically replayed, even after lease expiry. A successful local activation therefore does not imply successful Registry publication. This implementation does not add Registry idempotency receipts or a command to resolve uncertain external actions; those require separate reconciliation before further publication. Pending uncertain external work also blocks superseding the plan.

The schema update is additive. Pending plans created before this protocol have no trustworthy per-service history and must be replanned. Historical applied plans remain readable. Older clients receive a non-success HTTP response for partial outcomes rather than an apparent success.

Verification includes real PostgreSQL failures during policy, applied-state, and success-receipt persistence in a 100-service plan, selective retry after restarting repository objects, a lost COMMIT acknowledgement, replaced leases, direct configuration changes, old-plan rejection, and a Registry response lost after dispatch through the HTTP handler. CLI tests check partial output, non-success return behavior, and reuse of the same plan.
