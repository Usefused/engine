# Recovering a published import

An import has two owners:

1. Registry builds the reviewed import in a rollback-only transaction. Engine
   validates that exact runtime candidate, and Registry binds apply to its hash.
2. Registry validates and commits the service/version, operations, webhooks,
   source metadata, and import-plan state in its publication transaction.
3. Engine fetches and independently validates the published runtime snapshot,
   then activates that exact version in the workspace.

A failure with `phase: engine_preflight` and `commit_state: not_committed`
occurs before publication. Correct the reviewed contract and retry the exact
receipt command only when the response marks it retryable.

A Registry pre-commit write failure rolls back its transaction. A lost commit
acknowledgement is ambiguous, not proof of rollback. An Engine activation
failure after publication does **not** undo the committed Registry data.

## If apply returns HTTP 424

Check the structured error for `import_workspace_activation_failed`,
`phase: workspace_activation`, and `commit_state: committed`. Retain the
operation/request IDs and run the exact `recovery` command returned by Engine
after fixing the reported contract or access issue. It targets workspace
activation, not a second publication. Neither the CLI nor the UI should report
this result as complete success.

The import operation-status endpoint describes Registry publication. An
`applied` publication alone does not prove Engine workspace activation. Verify
the exact service version in the workspace after recovery.

## If startup reports deferred recovery

`Owned service recovery deferred` means an owned version was deliberately left
unactivated. The log includes the requested service/version IDs and the
`blocking_service_version_id`. The Engine can start without that version; its
existing pins are not updated or deleted.

An Engine-side validation rejection can be isolated from valid peers in the
same response. A Registry-side typed rejection fails the entire batch, so all
missing versions in that batch remain deferred until its blocking version is
repaired. Startup does not make one fallback request per service. An older
Registry returning only an unclassified message still fails closed.

Repair the reviewed contract through the normal owner-authorized import path,
then activate the corrected version. Do not remove security requirements,
rewrite capability arrays, or delete services as an automatic recovery step.
Startup retry alone cannot repair malformed source data.

## Verification for maintainers

From the Engine repository:

```bash
env -u DATABASE_URL go test ./internal/engine/sandbox ./internal/engine/api ./internal/engine/store
```

Run `TestMissingOwnedServicesSQL` with `FUSED_TEST_DATABASE_URL` pointing to an
isolated test PostgreSQL. It uses a session-local temporary table and checks
the SQL membership selection through the cache wrapper. It never uses the
application `DATABASE_URL` implicitly.

From the UI directory, run `npm test` and `npm run build`. The regression tests
cover committed partial-error recovery fields and the absence of duplicate
workspace activation. Registry tests cover pre-write inbound-security
rejection and typed GraphQL contract failures. Authentication, identity,
transport, and storage failures must remain fatal in the recovery tests.

Recovery emits one `engine.owned_services.reconcile` OTEL span with bounded
counts and `complete`, `partial`, or `failed` outcome. Raw contracts, errors,
license keys, and recovery commands are excluded from trace attributes.
