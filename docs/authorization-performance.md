# Authorization performance evidence

The authorization acceptance benchmarks cover the production PostgreSQL principal-loading query and the Engine GraphQL authorization boundary.

## Scenarios

- PostgreSQL cold authentication: 1, 10, and 50 active team memberships, each with 100 effective team role bindings.
- Credential cache hit: the same membership and binding scales, after one warm authentication.
- Authorization: `CheckAll` against all 100 service requirements from the cached authorization snapshot.
- GraphQL preflight: 1, 10, and 25 selected deployment fields, including static authorization, request parsing, batched bucket lookup, batched local service lookup, batched config-state lookup, one Registry fallback, and final scoped authorization.
- Total request: 1, 10, and 25 selected GraphQL fields from `X-API-Key` through a warmed production `Authenticator` cache, control-actor context hydration, GraphQL authorization, and resolver execution.

`TestGraphQLAuthorizationPreflightQueryCountIsBounded` is the regression assertion for N+1 behavior. All three selected-field sizes must use exactly three database/repository batches and one Registry batch. The benchmark also emits `db_queries/op` and `external_queries/op` so the evidence records both latency and call count.

## Reproduce the percentile report

Use an isolated PostgreSQL database. The benchmark resets authorization and workspace fixture tables; do not point it at a development or shared database. The explicit reset acknowledgement prevents an accidental run.

```sh
DATABASE_URL='postgres://opensync:password@localhost:5432/opensync_authz_benchmark?sslmode=disable' \
FUSED_BENCHMARK_ALLOW_DB_RESET=1 \
go run ./cmd/authz-perf-report -samples 20 -benchtime 100x
```

To retain an evidence artifact outside the repository, pass `-output /absolute/path/authz-performance.md`. Machine-specific output is intentionally not committed as a performance guarantee.

The report uses independent Go benchmark samples and nearest-rank percentiles. It prints p50, p95, and p99 for every cold-authentication, cache-hit, authorization, GraphQL-preflight, and total-request scenario. A run fails if any required phase is absent. Twenty samples is the default and minimum recommended evidence run; the command rejects fewer than ten.

For a fast compile and call-count check without PostgreSQL benchmark execution:

```sh
go test ./internal/engine/api ./internal/engine/store \
  -run 'TestGraphQLAuthorizationPreflightQueryCountIsBounded|^$' \
  -bench '^BenchmarkAuthorizationAcceptance$' -benchtime 1x
```

This short command is a correctness smoke test, not percentile evidence.
