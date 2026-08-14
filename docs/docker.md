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

Both images build `runtime/mcp/dist/bundle.js` before Go compilation. The Go
binary embeds that dependency-complete bundle, so MCP sessions still execute
through a small Node process without installing `node_modules` at container
startup or writing shared dependencies into `/app/data`.

## Runtime Requirements

Containers need:

- `FUSED_LICENSE_KEY`
- `FUSED_DATABASE_URL`
- `FUSED_DATABASE_MAX_CONNS` when the operator needs a ceiling lower than the
  standalone default of `10`
- `FUSED_DATABASE_MAX_CONN_IDLE_TIME` to release unused pool connections;
  standalone defaults to `30m`, while Fused-hosted starter Engines use `2m`
- `FUSED_ENCRYPTION_KEY`
- `FUSED_ENGINE_PUBLIC_URL` and `FUSED_ENGINE_PUBLIC_GRPC_URL` when the
  embedded UI should show copy-ready external HTTP and gRPC addresses; these
  values describe routes and do not create DNS or load-balancer configuration
- `FUSED_REGISTRY_ENDPOINT` only when Fused support directs you away from the
  production Fused Cloud Registry default

Engine accepts one standard PostgreSQL DSN and has no provider-specific
database branching. It creates or upgrades its own tables through that
connection during startup. Moving an existing database between providers is a
separate operator-run data migration.

External NATS is required when horizontally replicating Engine. Set `NATS_URL`
without embedded credentials, then configure exactly one of
`NATS_CREDS_FILE`, `NATS_NKEY_SEED_FILE`, `NATS_TOKEN`, or the paired
`NATS_USERNAME`/`NATS_PASSWORD`. TLS uses `NATS_TLS_CA_FILE`, optional paired
`NATS_TLS_CERT_FILE`/`NATS_TLS_KEY_FILE` for mTLS, and optional
`NATS_TLS_SERVER_NAME`. Mount credential and TLS files read-only from the
container secret mechanism; do not bake them into an image.

Scheduled authorization, usage-report, package-lease, and public-insight
probes share a short database gate. This lets one connection serve quiet
maintenance while foreground requests can still expand the pool to its
configured maximum; actual maintenance writes and Registry calls do not hold
the gate.

`FUSED_ENCRYPTION_KEY` must be a production secret. Do not use the checked-in
example value outside local development.
