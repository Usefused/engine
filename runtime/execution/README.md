# Unified App compiler snapshot

The source of truth is the Registry repository's `execution/` package at the
Git revision in `SOURCE_REVISION`. Engine vendors the pinned package because
the authoring package is currently private and Engine images must build without
fetching another repository or installing dependencies at runtime.

After a Registry compiler release, update this snapshot from a clean Registry
checkout:

```sh
scripts/vendor-execution-compiler.sh --sync /path/to/registry-checkout
scripts/vendor-execution-compiler.sh --verify /path/to/registry-checkout
```

Only the Registry package should be edited. `src/`, `test/`, `scripts/`, `examples/`,
`package.json`, `package-lock.json`, and `tsconfig.json` are copied verbatim.
