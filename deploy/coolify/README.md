# Deploy Fused Engine on Coolify

Coolify is self-hosted, so deployments start from your own Coolify dashboard using [docker-compose.yml](docker-compose.yml). The Compose stack bundles **Fused Engine** (`ghcr.io/usefused/engine`) together with **PostgreSQL 16** and uses Coolify magic variables (`SERVICE_URL_ENGINE_8081`, `SERVICE_USER_POSTGRES`, `SERVICE_PASSWORD_POSTGRES`) to configure HTTPS routing and database credentials automatically.

## Prerequisites

- A Coolify v4 instance connected to a Linux Docker host.
- A **[Fused license key](https://usefused.com/signup)** (choose **License key only**).
- A persistent 32-byte base64 encryption key generated once:

```bash
openssl rand -base64 32
```

## Deploy from GitHub (Recommended)

1. In Coolify, open your project and click **+ New Resource → Public Repository**.
2. Enter `https://github.com/Usefused/engine` and choose **Docker Compose** as the build pack.
3. Set **Base Directory** to `/deploy/coolify` and **Docker Compose Location** to `/docker-compose.yml`.
4. Open **Environment Variables** and set:
   - `FUSED_LICENSE_KEY`: Your Fused license key.
   - `FUSED_ENCRYPTION_KEY`: Your generated 32-byte base64 key (`openssl rand -base64 32`).
   - *(Optional)* `FUSED_ENGINE_VERSION`: Pin a release tag such as `v0.64.0` instead of `latest`.
5. Click **Deploy**. Coolify starts PostgreSQL, waits for it to be healthy, starts Fused Engine, and provisions HTTPS on your domain.

## Deploy by Pasting Compose

1. In Coolify, click **+ New Resource → Docker Compose Empty**.
2. Paste the contents of [docker-compose.yml](docker-compose.yml) and save.
3. Set `FUSED_LICENSE_KEY` and `FUSED_ENCRYPTION_KEY` in **Environment Variables**, then click **Deploy**.

## Optional: gRPC & Unified App Workers

- **gRPC (`50051`)**: To allow generated SDKs to connect over gRPC, expose port `50051` through Coolify's TCP proxy or a TLS-terminating load balancer and set `FUSED_ENGINE_PUBLIC_GRPC_URL`.
- **Unified App Linux isolation**: If you run Unified Apps on a Linux Docker host, extract `seccomp.json` from the image as described in [deploy/unified-app-workers/README.md](../unified-app-workers/README.md), attach `security_opt: ["seccomp=/path/to/seccomp.json"]`, and set `FUSED_UNIFIED_APP_WORKER_REQUIRED=true`.
- **Backups**: Always back up both the `fused-postgres-data` volume and your `FUSED_ENCRYPTION_KEY` together.
