# Changelog

## Unreleased

- Remove SOAP execution. Operations with method `SOAP` now fail with `501` and `ErrSOAPUnsupported` instead of calling the provider; the `gosoap` and `goxml2json` dependencies are dropped.
- Consolidate Goja into `executionappvm`: one bounded runtime constructor now serves Unified App execution, bundle inspection, and MCP metadata (`search_docs`, tool listing). MCP metadata output is unchanged.
- Remove the unused `nats-server` apt package from Docker images; Engine continues to use its embedded NATS server or `NATS_URL`.
- Remove unreachable code (legacy unified-operation schemas, superseded SDK/workspace apply helpers, legacy model encryption with a hard-coded fallback key), unused UI components, and two stray macOS binaries under `internal/engine/sandbox/`.
- Serve stateless MCP tool discovery and documentation search directly from the authorized catalogue without spawning execution sandboxes or consuming sandbox-start limits. Preserve shared search semantics, request rate limits, and explicit execution-state isolation.
