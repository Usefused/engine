# Deploy Fused Engine to Cloudflare Containers

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button?paid=true)](https://deploy.workers.cloudflare.com/?url=https://github.com/Usefused/engine/tree/main/deploy/cloudflare&paid=true)

Runs `ghcr.io/usefused/engine` on **Cloudflare Containers** behind a thin Worker + Durable Object proxy ([src/index.ts](src/index.ts), [wrangler.jsonc](wrangler.jsonc)).

> **Note:** Cloudflare's one-click button requires connecting GitHub or GitLab because Cloudflare clones this template into a repo in your account for Workers Builds. To deploy without Git, use [CLI deploy](#cli-deploy-no-git) below.

## Prerequisites

- **Cloudflare Workers Paid** plan (required for Containers).
- **PostgreSQL 16+** reachable from Cloudflare (Neon, Supabase, etc.).
- **[Fused license key](https://usefused.com/signup)** (**License key only**).
- **Encryption key** generated once: `openssl rand -base64 32`

## Secrets & Variables

The Deploy button prompts for the required secrets from [.dev.vars.example](.dev.vars.example). You can also manage them in **Workers & Pages → fused-engine → Settings → Variables and Secrets**.

- **Required secrets:** `FUSED_DATABASE_URL`, `FUSED_LICENSE_KEY`, `FUSED_ENCRYPTION_KEY`
- **Optional vars:**
  - `FUSED_ENGINE_PUBLIC_URL` — Public HTTPS origin for Admin UI and OAuth callbacks.
  - `FUSED_ENGINE_ENVIRONMENT` — Environment label (default: `production`).
  - `FUSED_DATABASE_MAX_CONNS` / `FUSED_DATABASE_MAX_CONN_IDLE_TIME` — Pool bounds (defaults: `10`, `2m`).
  - `NATS_URL` (+ `NATS_TOKEN` or `NATS_USERNAME`/`NATS_PASSWORD`) — Only needed when `max_instances > 1`.

## CLI Deploy (No Git)

```bash
cd deploy/cloudflare
npm install
npx wrangler deploy
npx wrangler secret put FUSED_DATABASE_URL
npx wrangler secret put FUSED_LICENSE_KEY
npx wrangler secret put FUSED_ENCRYPTION_KEY
```

Pin a specific Engine version by changing `ARG FUSED_ENGINE_VERSION` in [Dockerfile](Dockerfile).

## Notes

- **HTTP only:** Admin UI, CLI, REST, webhooks, and MCP work over HTTPS; raw gRPC (`50051`) is not exposed.
- **Container sleep:** Sleeps after `15m` idle (`sleepAfter` in [src/index.ts](src/index.ts)); scheduled runs pause while asleep.
- **Encryption key:** Keep the same `FUSED_ENCRYPTION_KEY` across redeployments to decrypt stored credentials.
