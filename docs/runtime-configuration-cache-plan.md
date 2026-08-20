# Runtime Configuration Cache Implementation Plan

## Goal

Reduce the per-execution Engine overhead caused by repeatedly loading immutable runtime configuration, while preserving immediate correctness after changes made through the UI, CLI, REST, GraphQL, or deployment workflows.

The cache is an Engine-local optimization. PostgreSQL remains the source of truth, and NATS is used only to accelerate cross-instance invalidation.

## Scope

Cache these complete query results for at most 30 seconds:

- app runtime configuration returned by `GetAppRuntime`;
- workspace connection bindings returned by `ListWorkspaceBindingsForExecution`;
- workspace execution-policy overrides returned by `GetEffectiveWorkspaceExecutionPolicyOverride`.

Keep atomic secret-set selection uncached. `GetFirstCompleteSecretSet` must continue to select a complete alternative in one database query so authentication never mixes credentials or introduces N+1 reads.

## Cache Semantics

- Expiration is absolute from insertion. Reads never extend the deadline, so a missed NATS event cannot keep stale data alive indefinitely.
- An expired entry is synchronously reloaded before the caller continues. There is no background refresh or stale-while-revalidate path.
- Concurrent misses for the same key and cache generation are coalesced with `singleflight`.
- Every invalidation advances a local generation before clearing entries. The generation is part of cache and singleflight keys, preventing a request that starts after a committed mutation from joining an older in-flight load.
- Database values are cached as complete results and copied at the cache boundary where they contain mutable slices or maps.

## Invalidation Matrix

| Committed change | Local action | Cross-instance action | Other cache action |
| --- | --- | --- | --- |
| App runtime/apply notification | Advance generation and clear runtime cache | Publish runtime-config invalidation | Publish exact SDK-scope invalidation |
| App deprecation/undeprecation | Advance generation and clear runtime cache | Publish runtime-config invalidation | Publish exact SDK-scope invalidation |
| App hard deactivation | Advance generation and clear runtime cache | Publish runtime-config invalidation | Publish exact SDK-scope invalidation |
| App-family bucket assignment | Advance generation and clear runtime cache | Publish runtime-config invalidation | None |
| Workspace profile upsert/reset/reconcile | Advance generation and clear runtime cache | Publish runtime-config invalidation | None |
| Execution-policy override upsert/reset | Advance generation and clear runtime cache | Publish runtime-config invalidation | None |

Invalidation is deliberately broad because these writes are rare, while their dependency graph spans apps, bindings, auth selections, and execution policy. A single path is easier to audit and prevents a new mutation surface from retaining a partially stale runtime view.

Local invalidation occurs synchronously after the database commit. NATS subscribers repeat the local invalidation on peer Engine instances. If NATS publish or delivery fails, the fixed 30-second deadline remains the correctness backstop.

## Observability

Use the existing request or mutation span; do not add a second telemetry pipeline.

- Add `engine.runtime_cache.lookup` events with bounded `cache.kind` and `cache.result` attributes.
- Add `engine.runtime_cache.invalidated` events after successful mutations with bounded propagation outcomes.
- Never attach app IDs, workspace IDs, tokens, secret values, raw errors, or payloads.
- Preserve existing audit events for user/agent-triggered mutations; cache telemetry supplements rather than replaces them.

## Code Structure

- Keep the generic absolute-TTL implementation in `internal/shared/cache`.
- Keep runtime cache loading, copying, generation fencing, and OTEL helpers in a focused store file.
- Keep mutation wrappers in `cachedStore`, immediately adjacent to the store capabilities they decorate.
- Split decision helpers so every changed function stays at cyclomatic complexity 10 or below.

## Verification

1. Unit-test absolute expiration, including repeated reads before expiry.
2. Unit-test cache hits, expiry reloads, defensive copies, and concurrent miss coalescing.
3. Unit-test local invalidation for app runtime, profile, policy, reconciliation, and hard deactivation mutations.
4. Integration-test NATS fan-out between two cached store instances and the fixed-TTL fallback when no invalidation arrives.
5. Run affected store, sandbox, API, and lifecycle tests, then the complete Engine test suite.
6. Run the repository complexity check and confirm every changed Go function is at or below 10.
7. Re-run the production execution benchmark after deployment; no external E2E contract changes are required before deployment.

## Documentation

Update the Engine app-lifecycle and observability skills with the absolute-TTL, invalidation, hard-deactivation, and bounded-telemetry invariants so future changes preserve the design.
