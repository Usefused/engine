# Deploy Fused Engine with Vercel

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2FUsefused%2Fengine&root-directory=deploy%2Fvercel&project-name=fused-engine&env=FUSED_DATABASE_URL,FUSED_LICENSE_KEY,FUSED_ENCRYPTION_KEY&envDescription=PostgreSQL%2016%2B%20DSN%2C%20Fused%20license%20key%2C%20and%2032-byte%20base64%20encryption%20key%20(openssl%20rand%20-base64%2032)&envLink=https%3A%2F%2Fgithub.com%2FUsefused%2Fengine%2Fblob%2Fmain%2Fdocs%2Fengine-configuration.md)

This template deploys the published `ghcr.io/usefused/engine` container on **Vercel Containers (Fluid Compute)** using [Dockerfile.vercel](Dockerfile.vercel).

## Prerequisites

- **PostgreSQL 16+** reachable from Vercel (for example Neon, Supabase, or a Vercel Marketplace Postgres integration).
- A **[Fused license key](https://usefused.com/signup)** (choose **License key only**).
- A persistent 32-byte base64 encryption key generated once:

```bash
openssl rand -base64 32
```

## Environment Variables

Configure these during the one-click clone flow or under **Project Settings → Environment Variables**:

| Variable | Required | Description |
| --- | --- | --- |
| `FUSED_DATABASE_URL` | Yes | PostgreSQL 16+ connection string (`postgres://...?sslmode=require`). |
| `FUSED_LICENSE_KEY` | Yes | Your Fused Engine license key. |
| `FUSED_ENCRYPTION_KEY` | Yes | Persistent 32-byte base64 AES key (`openssl rand -base64 32`). |
| `FUSED_ENGINE_PUBLIC_URL` | Recommended | Your deployed Vercel URL or custom domain (for example `https://fused-engine.vercel.app`) for Admin UI links and OAuth callbacks. |
| `FUSED_DATABASE_MAX_CONNS` | Optional | Connection pool ceiling (defaults to `10`). |
| `FUSED_DATABASE_MAX_CONN_IDLE_TIME` | Optional | Idle connection timeout (defaults to `2m` in this template). |

Vercel injects `PORT` automatically, and Fused Engine binds to `$PORT` when `FUSED_ENGINE_HTTP_PORT` is unset.

## Platform Notes

- **Stateless / scale-to-zero**: Vercel Containers run on Fluid Compute and scale to zero when idle. Always-on background schedules, long-lived changelog polling, and Linux user-namespace Unified App workers are better suited for a persistent Docker host such as [Coolify](../coolify/README.md).
- **HTTP only**: Admin UI, CLI (`fused-cli`), REST API, webhooks, and hosted MCP work over HTTPS. Raw gRPC on port `50051` is not exposed by Vercel.
- **Persistent encryption key**: Keep the exact same `FUSED_ENCRYPTION_KEY` across redeployments so Fused can decrypt stored credentials.
