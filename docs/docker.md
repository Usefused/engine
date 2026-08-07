# Docker

Engine publishes two image variants:

- `ghcr.io/usefused/engine:<version>`: Engine with embedded Admin UI.
- `ghcr.io/usefused/engine:<version>-headless`: Engine API/runtime without the
  embedded UI.

The moving tags `latest` and `headless` are convenient for testing. Production
deployments should pin a version tag.

```bash
docker pull ghcr.io/usefused/engine:v0.0.1
docker pull ghcr.io/usefused/engine:v0.0.1-headless
```

## Local Builds

```bash
make docker-build
make docker-build-headless
```

The full image builds the UI and embeds `ui-build` into the Go binary. The
headless image compiles with `-tags headless`.

Both images include `runtime/mcp/dist/server.js`, because MCP sessions execute
through a small Node runtime process.

## Runtime Requirements

Containers need:

- `FUSED_LICENSE_KEY`
- `FUSED_DATABASE_URL`
- `FUSED_DATABASE_MAX_CONNS` when the operator needs a ceiling lower than the
  standalone default of `10`
- `FUSED_DATABASE_MAX_CONN_IDLE_TIME` to release unused pool connections;
  standalone defaults to `30m`, while Fused-hosted starter Engines use `2m`
- `FUSED_ENCRYPTION_KEY`
- `FUSED_REGISTRY_ENDPOINT` only when Fused support directs you away from the
  production Fused Cloud Registry default

`FUSED_ENCRYPTION_KEY` must be a production secret. Do not use the checked-in
example value outside local development.
