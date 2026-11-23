# Deploy Fused Engine on Hostlier

This directory provides ready-to-validate Hostlier v2 deployment manifests for **Fused Engine** (`ghcr.io/usefused/engine`) with persistent `/app/data` storage, streaming HTTP / WebSocket / MCP support, and either **Hostlier-managed PostgreSQL** ([.hostlier](.hostlier)) or a **bundled PostgreSQL 16 sidecar** ([sidecar.hostlier](sidecar.hostlier)).

## Prerequisites

- The `hostlier` CLI connected to your Hostlier controller (`hostlier connect https://your-controller --name production`).
- A **[Fused license key](https://usefused.com/signup)** (choose **License key only**).
- For Option 1 (managed PostgreSQL), the Hostlier `postgres` resource package installed on your controller (`hostlier package list --server production`).

## Option 1: Deploy with Managed PostgreSQL (Recommended)

[.hostlier](.hostlier) provisions a dedicated PostgreSQL resource in the same approved deployment plan and injects its DSN into `FUSED_DATABASE_URL` automatically via `bindings`.

### 1. Store your Engine secrets on the controller

```bash
export FUSED_LICENSE_KEY='<your-license-key>'
hostlier secret set fused-license-key --from-env FUSED_LICENSE_KEY --server production
openssl rand -base64 32 | hostlier secret set fused-encryption-key --stdin --server production
```

Keep a secure offline backup of `fused-encryption-key` so PostgreSQL backups remain decryptable across restores.

### 2. Validate, plan, and deploy

```bash
cd deploy/hostlier
hostlier app validate --file .hostlier
hostlier app deploy --file .hostlier \
  --route https://engine.apps.example.com \
  --server production
```

Review the deployment plan and complete the printed `hostlier approve CODE --key FILE` confirmation from your trusted terminal. Replace `https://engine.apps.example.com` with a route authorized on your controller (and set `env.FUSED_ENGINE_PUBLIC_URL` in `.hostlier` to match for OAuth callbacks and webhook URLs).

## Option 2: Deploy with Bundled PostgreSQL 16 Sidecar

If your Hostlier controller does not have the `postgres` resource package installed, use [sidecar.hostlier](sidecar.hostlier). Hostlier starts `postgres:16-alpine` on loopback (`127.0.0.1:5432`) with persistent storage before starting Engine.

```bash
export FUSED_LICENSE_KEY='<your-license-key>'
PG_PASS="$(openssl rand -hex 24)"

hostlier secret set fused-license-key --from-env FUSED_LICENSE_KEY --server production
openssl rand -base64 32 | hostlier secret set fused-encryption-key --stdin --server production
printf '%s' "$PG_PASS" | hostlier secret set fused-postgres-password --stdin --server production
printf 'postgres://fused:%s@127.0.0.1:5432/fused?sslmode=disable' "$PG_PASS" \
  | hostlier secret set fused-database-url --stdin --server production

cd deploy/hostlier
hostlier app validate --file sidecar.hostlier
hostlier app deploy --file sidecar.hostlier \
  --route https://engine.apps.example.com \
  --server production
```

## Optional: Native gRPC (`50051`) Endpoint

Hostlier supports native HTTP/2 gRPC ingress directly alongside HTTP. Add a `grpc` endpoint and set `FUSED_ENGINE_PUBLIC_URL` / `FUSED_ENGINE_PUBLIC_GRPC_URL` in `.hostlier`:

```yaml
endpoints:
  http:
    exposure: public
    port: 8081
    route: https://engine.apps.example.com
    protocol: http
    streaming: true
    websocket: true
    httpVersions: [h1, h2]
  grpc:
    exposure: public
    port: 50051
    route: https://grpc.apps.example.com
    protocol: grpc
    idleTimeoutSeconds: 600
env:
  FUSED_ENGINE_ENVIRONMENT: production
  FUSED_ENGINE_PUBLIC_URL: https://engine.apps.example.com
  FUSED_ENGINE_PUBLIC_GRPC_URL: https://grpc.apps.example.com
```

## Optional: Unified App Workers & Minion Integration

- **Confined Unified App workers**: Hostlier Docker deployments support `security.seccompConfig` pointing to a managed config file containing the release's `seccomp.json` (see [deploy/unified-app-workers/README.md](../unified-app-workers/README.md)), together with `command: [/usr/bin/tini, --, /app/fused-engine]`, `args: [start]`, and `env.FUSED_UNIFIED_APP_WORKER_REQUIRED: "true"`.
- **Connect Hostlier Minion apps**: Once Engine is live, bind your Hostlier controller to it with `hostlier fused connect fused --engine-url https://engine.apps.example.com --server production`.
- **Pin a release tag**: Replace `ghcr.io/usefused/engine:latest` with a specific release tag (for example `ghcr.io/usefused/engine:v0.64.0`) or immutable digest before deploying to production.
