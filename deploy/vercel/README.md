# Deploy Fused Engine with Vercel

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2FUsefused%2Fengine%2Ftree%2Fmain%2Fdeploy%2Fvercel&project-name=fused-engine&repository-name=fused-engine&env=FUSED_DATABASE_URL,FUSED_LICENSE_KEY,FUSED_ENCRYPTION_KEY&envDescription=PostgreSQL%2016%2B%20DSN%2C%20Fused%20license%20key%2C%20and%2032-byte%20base64%20encryption%20key%20(openssl%20rand%20-base64%2032)&envLink=https%3A%2F%2Fgithub.com%2FUsefused%2Fengine%2Fblob%2Fmain%2Fdocs%2Fengine-configuration.md)

Runs `ghcr.io/usefused/engine` on **Vercel Containers (Fluid Compute)** via [Dockerfile.vercel](Dockerfile.vercel) and [vercel.json](vercel.json).

> **Note:** Vercel's one-click button requires connecting GitHub, GitLab, or Bitbucket and clones `deploy/vercel` into a new repo in your account. To deploy without Git, use [CLI deploy](#cli-deploy-no-git) below.

## Prerequisites

- **PostgreSQL 16+** reachable from Vercel (Neon, Supabase, or Vercel Marketplace Postgres).
- **[Fused license key](https://usefused.com/signup)** (**License key only**).
- **Encryption key** generated once: `openssl rand -base64 32`

## Environment Variables

The Deploy button prompts for the required variables; you can also manage them in **Project Settings → Environment Variables**.

| Variable | Required | Description |
| --- | --- | --- |
| `FUSED_DATABASE_URL` | Yes | PostgreSQL 16+ DSN (`postgres://...?sslmode=require`). |
| `FUSED_LICENSE_KEY` | Yes | Fused Engine license key. |
| `FUSED_ENCRYPTION_KEY` | Yes | Persistent 32-byte base64 key (`openssl rand -base64 32`). |
| `FUSED_ENGINE_PUBLIC_URL` | Recommended | Public HTTPS URL (`https://fused-engine.vercel.app`) for Admin UI and OAuth callbacks. |
| `FUSED_DATABASE_MAX_CONNS` | Optional | Connection pool ceiling (default: `10`). |
| `FUSED_DATABASE_MAX_CONN_IDLE_TIME` | Optional | Idle connection timeout (default: `2m`). |

Fused Engine automatically binds to Vercel's injected `$PORT`.

## CLI Deploy (No Git)

```bash
cd deploy/vercel
npx vercel env add FUSED_DATABASE_URL production
npx vercel env add FUSED_LICENSE_KEY production
npx vercel env add FUSED_ENCRYPTION_KEY production
npx vercel deploy --prod
```

Pin a specific Engine version by changing `ARG FUSED_ENGINE_VERSION` in [Dockerfile.vercel](Dockerfile.vercel).

## Notes

- **Scale-to-zero:** Runs as a stateless Fluid Compute Container Function. Use [Railway](../railway/README.md), [Coolify](../coolify/README.md), or [Hostlier](../hostlier/README.md) for always-on schedules or Unified App workers.
- **HTTP only:** Admin UI, CLI, REST, webhooks, and MCP work over HTTPS; raw gRPC (`50051`) is not exposed.
- **Encryption key:** Keep the same `FUSED_ENCRYPTION_KEY` across redeployments to decrypt stored credentials.
