# Deploy Fused Engine on Railway (with Managed PostgreSQL)

This directory packages **Fused Engine** (`ghcr.io/usefused/engine`) together with **PostgreSQL 16** provisioning for **[Railway](https://railway.com)**. Because Fused Engine honors Railway's injected `PORT` automatically and wires `FUSED_DATABASE_URL` via `${{Postgres.DATABASE_URL}}`, Railway brings up PostgreSQL first and routes HTTPS traffic to `/health` with zero manual networking setup.

## Prerequisites

- A [Railway](https://railway.com) account.
- A **[Fused license key](https://usefused.com/signup)** (choose **License key only**).

## Option 1: Publish a One-Click Railway Template Button

Railway replaced legacy `plugins=postgresql` clone links with published templates at `https://railway.com/new/template/<TEMPLATE_ID>`. To create a one-click **Deploy on Railway** button for your workspace or public docs:

1. Open Railway's **Template Composer** (`https://railway.com/new/template`) and add two services using [template.json](template.json) as your blueprint:
   - **Postgres**: Managed PostgreSQL 16 database (`+ Database → PostgreSQL`).
   - **FusedEngine**: Docker image `ghcr.io/usefused/engine:latest`, volume mounted at `/app/data`, HTTP healthcheck path `/health`, and these variables:
     - `FUSED_DATABASE_URL=${{Postgres.DATABASE_URL}}`
     - `FUSED_ENGINE_PUBLIC_URL=https://${{RAILWAY_PUBLIC_DOMAIN}}`
     - `FUSED_ENCRYPTION_KEY=${{secret(42, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/")}}${{secret(1, "AEIMQUYcgkosw048")}}=`
     - `FUSED_LICENSE_KEY` *(user-supplied on deploy)*
     - `FUSED_ENGINE_ENVIRONMENT=production`
     - `FUSED_DATABASE_MAX_CONNS=10`
     - `FUSED_DATABASE_MAX_CONN_IDLE_TIME=2m`
2. Publish the template to obtain its `TEMPLATE_ID`, then embed the button:

```markdown
[![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/new/template/<TEMPLATE_ID>)
```

The `FUSED_ENCRYPTION_KEY` expression generates a canonical RFC 4648 32-byte base64 key once per deployment and persists it in Railway's service variables.

## Option 2: Deploy via Railway IaC (`.railway/railway.ts`)

Railway's Infrastructure as Code file at [.railway/railway.ts](.railway/railway.ts) provisions managed **PostgreSQL**, a persistent `/app/data` volume, and **Fused Engine** in one workflow:

```bash
cd deploy/railway
railway login
railway init
railway variables --set "FUSED_LICENSE_KEY=<your-license-key>" \
  --set "FUSED_ENCRYPTION_KEY=$(openssl rand -base64 32)"
railway config plan
railway config apply
```

## Option 3: Manual Canvas Deploy (2 Minutes)

1. In a new Railway project, click **+ New → Database → Add PostgreSQL**.
2. Click **+ New → Docker Image** and enter `ghcr.io/usefused/engine:latest` (or pin a release tag such as `ghcr.io/usefused/engine:v0.64.0`).
3. Attach a persistent volume at `/app/data` and configure `/health` as the Healthcheck Path (also declared in [railway.json](railway.json) if deploying from GitHub with Root Directory `deploy/railway`).
4. Under **Variables**, add:
   - `FUSED_DATABASE_URL=${{Postgres.DATABASE_URL}}`
   - `FUSED_ENGINE_PUBLIC_URL=https://${{RAILWAY_PUBLIC_DOMAIN}}`
   - `FUSED_LICENSE_KEY=<your-license-key>`
   - `FUSED_ENCRYPTION_KEY=<output of: openssl rand -base64 32>`
5. Under **Settings → Networking**, click **Generate Domain** to expose the Admin UI, CLI, REST API, webhooks, and hosted MCP over HTTPS.

## Platform Notes

- **Automatic PostgreSQL ordering**: Referencing `${{Postgres.DATABASE_URL}}` ensures Railway provisions and starts PostgreSQL before booting Fused Engine.
- **Always-on runtime**: Unlike scale-to-zero serverless platforms, Railway keeps your Engine container running continuously with persistent `/app/data` storage, supporting scheduled Unified App runs and background changelog polling.
- **Optional gRPC (`50051`)**: Add a Railway **TCP Proxy** targeting port `50051` and set `FUSED_ENGINE_PUBLIC_GRPC_URL` if generated SDKs connect over gRPC.
- **Persistent encryption key**: Back up `FUSED_ENCRYPTION_KEY` alongside your PostgreSQL backups so restored databases remain decryptable.
