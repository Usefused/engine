# Deploy Fused Engine on Pxxl (`pxxl.app`)

This template deploys the published `ghcr.io/usefused/engine` container as a Web Service on **[Pxxl](https://pxxl.app)** using [Dockerfile](Dockerfile) and [pxxl.toml](pxxl.toml).

## Prerequisites

- A [Pxxl](https://pxxl.app) account.
- **PostgreSQL 16+** (a Pxxl-managed Postgres database or an external Postgres provider such as Neon or Supabase).
- A **[Fused license key](https://usefused.com/signup)** (choose **License key only**).
- A persistent 32-byte base64 encryption key generated once:

```bash
openssl rand -base64 32
```

## Option 1: Deploy from GitHub in the Pxxl Dashboard

1. Log in at [pxxl.app](https://pxxl.app) and click **New Project → GitHub**.
2. Select your fork or `Usefused/engine` and set the **Base Directory** to `deploy/pxxl`.
3. Add the required environment variables below.
4. Click **Deploy Project**. Pxxl builds the thin wrapper image, injects `$PORT`, runs its port health check, and issues an HTTPS `*.pxxl.app` URL.

## Option 2: Deploy the Prebuilt Docker Image Directly

1. Log in at [pxxl.app](https://pxxl.app) and click **New Project → Docker Image**.
2. Set the image to `ghcr.io/usefused/engine:latest` (or a pinned release tag such as `ghcr.io/usefused/engine:v0.64.0`).
3. Set target port to `8081` (or rely on Pxxl's injected `PORT`, which Fused Engine honors automatically).
4. Add the required environment variables below and click **Deploy Project**.

## Option 3: Deploy with the Pxxl CLI

```bash
npm install -g @pxxlapp/pxxl
pxxl login
cd deploy/pxxl
pxxl deploy
```

## Environment Variables

| Variable | Required | Description |
| --- | --- | --- |
| `FUSED_DATABASE_URL` | Yes | PostgreSQL 16+ connection string. |
| `FUSED_LICENSE_KEY` | Yes | Your Fused Engine license key. |
| `FUSED_ENCRYPTION_KEY` | Yes | Persistent 32-byte base64 AES key (`openssl rand -base64 32`). |
| `FUSED_ENGINE_PUBLIC_URL` | Recommended | Your assigned `https://<project>.pxxl.app` or custom domain for Admin UI links and OAuth callbacks. |
| `FUSED_DATABASE_MAX_CONNS` | Optional | Connection pool ceiling (defaults to `10`). |
| `FUSED_DATABASE_MAX_CONN_IDLE_TIME` | Optional | Idle connection timeout (defaults to `2m`). |

## Platform Notes

- **Port binding**: Fused Engine listens on `0.0.0.0:$PORT` automatically whenever Pxxl injects `PORT` and `FUSED_ENGINE_HTTP_PORT` is unset.
- **HTTP only**: Admin UI, CLI (`fused-cli`), REST API, webhooks, and hosted MCP work over HTTPS. Raw gRPC on port `50051` is not exposed by Pxxl Web Services.
- **Persistent encryption key**: Keep the exact same `FUSED_ENCRYPTION_KEY` across redeployments so Fused can decrypt stored credentials.
