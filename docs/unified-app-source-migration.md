# Unified App source format migration

The only supported authoring format is:

```ts
export default buildUnifiedApp({
  config: {
    "name": "customer-app",
    "version": "1.0.0",
    "bucket": "default",
    "services": {}
  },
  input,
  output,
  execute,
});
```

Engine startup runs the source migration before serving private app source. Source format version 2 converts both YAML-derived implementation-only source and the previous exported metadata declaration into the inline `config` property. The retired declaration is understood only by this one-time data conversion. Editor, CLI and compiler admission have no fallback for it.

`fused_config_states.unified_source_format_version` records completion independently of the original `unified_source_migrated_at` timestamp. Apps marked by the first migration are therefore converted too. Current inline source is marked without rewriting its bytes; later starts do not revisit completed rows.

Each transaction locks at most 100 pending rows through the version-specific partial index. The source and version marker commit together. Concurrent Engine starts cannot convert the same version twice, and a failed page rolls back completely before a later startup retries it.

Conversion uses the persisted desired-state metadata and preserves unrelated executable source and comments. Only `desired_state.source` and migration progress fields change. Source hashes, config generations, app and family identities, selected scope, compiled bundles, execution tokens and traffic pointers remain unchanged. No recompilation or provider operation runs during migration.

Missing, oversized, malformed or unsafe-to-rewrite source remains unmarked and contributes to the startup `skipped` count. For example, executable references to the retired metadata binding require explicit source repair rather than removing a binding still used by the implementation. Skipped source receives no compatibility interpretation in the editor or compiler. Startup logs contain aggregate counts only.

The PostgreSQL tests cover both predecessor formats, already stamped records, multiple pages, concurrent starts, failure rollback, unchanged immutable state and repeat-start idempotence.
