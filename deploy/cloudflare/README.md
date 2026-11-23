# Deploy Fused Engine to Cloudflare Containers

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button?paid=true)](https://deploy.workers.cloudflare.com/?url=https://github.com/Usefused/engine/tree/main/deploy/cloudflare&paid=true)

This template deploys the published `ghcr.io/usefused/engine` container on **Cloudflare Containers** behind a Cloudflare Worker and Durable Object proxy.

## Prerequisites

- A **Cloudflare Workers Paid** plan (Cloudflare Containers are not available on the free tier).
- **PostgreSQL 16+** reachable over the public internet (for example Neon, Supabase, or Cloudflare Hyperdrive-backed Postgres).
- A **[Fused license key](https://usefused.com/signup)** (choose **License key only**).
- A persistent 32-byte base64 encryption key generated once:

```bash
openssl rand -base64 32
```

## Required Worker Secrets

Set these in the Cloudflare dashboard (**Workers & Pages → fused-engine → Settings → Variables and Secrets**) or with Wrangler:

```bash
npx wrangler secret put FUSED_DATABASE_URL
npx wrangler secret put FUSED_LICENSE_KEY
npx wrangler secret put FUSED_ENCRYPTION_KEY
```

### Optional Variables

- `FUSED_ENGINE_PUBLIC_URL`: Your public Workers URL or custom domain (for example `https://engine.example.com`) so the Admin UI and OAuth callbacks advertise the right origin.
- `FUSED_ENGINE_ENVIRONMENT`: Label echoed on `/health` and OTel telemetry (defaults to `production`).
- `FUSED_DATABASE_MAX_CONNS` / `FUSED_DATABASE_MAX_CONN_IDLE_TIME`: Pool bounds (defaults to `10` and `2m`).
- `NATS_URL` + `NATS_TOKEN` (or `NATS_USERNAME`/`NATS_PASSWORD`): Required only if you increase `max_instances` above `1`.

## Manual CLI Deployment

```bash
cd deploy/cloudflare
npm install
npx wrangler deploy
```

To pin a specific Fused Engine release, update `ARG FUSED_ENGINE_VERSION` in [Dockerfile](Dockerfile).

## Platform Notes

- **HTTP only**: Admin UI, CLI (`fused-cli`), REST API, webhooks, and hosted MCP work over HTTPS. Raw gRPC on port `50051` is not exposed by Cloudflare Workers.
- **Container sleep**: The container sleeps after 15 minutes of inactivity (`sleepAfter = "15m"` in [src/index.ts](src/index.ts)). Scheduled Unified App runs and background pollers pause while the container is asleep.
- **Persistent encryption key**: Keep the exact same `FUSED_ENCRYPTION_KEY` across redeployments so Fused can decrypt stored credentials.
