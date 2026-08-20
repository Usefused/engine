<div align="center">
  <h1>Fused Engine</h1>
  <p>Self-hosted data plane for <a href="https://usefused.com">Fused</a></p>
  <br />
  <a href="https://usefused.com"><strong>Explore usefused.com</strong></a>
</div>

---

[Fused](https://usefused.com) is the integration gateway — one place to define, run, and control the integrations your apps and agents depend on. The Engine is the self-hosted control plane that applies workspace policies, injects credentials, and routes configured API calls, MCP sessions, and webhook ingress.

The **Fused Engine** is the self-hosted runtime that applies workspace policies created in Fused. Deploy it in your own infrastructure to run configured API calls, MCP sessions, and webhook ingress close to your systems.

Credentials are not scraped from arbitrary traffic. Engine only injects credentials into requests that are executed through Fused-configured SDK, MCP, webhook, or proxy routes and only from secrets that you explicitly store or connect. Runtime request payloads stay in the environment where you deploy the Engine.

## Features
- **Self-hosted runtime**: Process configured webhooks, SDK calls, and MCP requests inside your own infrastructure.
- **Explicit credential routing**: Inject credentials only for Fused-managed routes using secrets you configure.
- **Embedded NATS JetStream**: Reliably queues incoming webhooks and coordinates durable delivery without requiring an external NATS cluster or Redis instance alongside it.
- **Headless Mode**: A no-UI Docker variant (`ghcr.io/usefused/engine:headless`) optimized for serverless and Kubernetes deployments.
- **Resilient**: Fully caches execution metadata locally to withstand network partitions.

## Prerequisites

- **PostgreSQL 16+**
- **Fused License Key** (Provided by your Fused onboarding contact)

## Installation

### Option 1: Native Binary
Download the release for your operating system from the [Releases](https://github.com/Usefused/engine/releases) page.

```bash
# macOS / Linux
VERSION=v0.0.1
OS=$(uname -s)
ARCH=$(uname -m | sed 's/aarch64/arm64/')
ARCHIVE="fused_${OS}_${ARCH}.tar.gz"

curl -LO "https://github.com/Usefused/engine/releases/download/${VERSION}/${ARCHIVE}"
curl -LO "https://github.com/Usefused/engine/releases/download/${VERSION}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
tar -xzf "${ARCHIVE}"
mv fused-engine /usr/local/bin/
```

### Option 2: Docker
For containerized environments, we provide both full and slim/headless images. Prefer a pinned version in production.

```bash
docker pull ghcr.io/usefused/engine:v0.0.1
docker pull ghcr.io/usefused/engine:v0.0.1-headless
```

If either pull fails with `unauthorized`, the `ghcr.io/usefused/engine` package isn't (or isn't yet) public. Authenticate first with a GitHub personal access token that has `read:packages` scope:

```bash
echo "$GITHUB_TOKEN" | docker login ghcr.io -u <your-github-username> --password-stdin
```

If you don't have access and aren't sure why, contact your Fused onboarding contact.

## Quick Start

The Fused Engine requires a **License Key** to successfully handshake with your Fused Cloud account. You'll also need to point it to your PostgreSQL database.

```bash
# 1. Export your database URL and License Key
export FUSED_DATABASE_URL="postgres://fused:password@localhost:5432/fused?sslmode=disable"
export FUSED_LICENSE_KEY="<provided-by-fused>"

# 2. Start the Engine
fused-engine start
```

By default, the Engine will automatically detect that no external NATS cluster is configured and will boot its embedded NATS server on port `4222`. It will spin up the REST API on port `8081`, and the SDK gRPC connection on port `50051`. The Registry endpoint defaults to Fused Cloud; only set `FUSED_REGISTRY_ENDPOINT` when Fused support asks you to use a different endpoint.

MCP runtime dependencies are bundled into the Engine binary at build time.
Containers do not run `npm install` at startup or persist shared
`node_modules` under `/app/data`; upgrading from an older release removes only
that retired shared dependency cache while preserving per-app data.

### Docker Compose Example (with UI)
Our full Docker image (`latest`) bundles both the Engine and the Admin Dashboard. The headless image (`headless`) runs the same Engine API without the embedded UI.

```yaml
version: '3.8'
services:
  postgres:
    image: postgres:16
    environment:
      - POSTGRES_USER=fused
      - POSTGRES_PASSWORD=password
      - POSTGRES_DB=fused
    ports:
      - "5432:5432"

  engine:
    image: ghcr.io/usefused/engine:latest
    ports:
      - "8081:8081"   # Engine API and embedded Admin Dashboard
      - "50051:50051" # SDK gRPC
    environment:
      - FUSED_DATABASE_URL=postgres://fused:password@postgres:5432/fused?sslmode=disable
      - FUSED_LICENSE_KEY=<provided-by-fused>
    depends_on:
      - postgres
```

Once running, you can access the Admin Dashboard and Engine API at `http://localhost:8081`.

## Use the Engine with `fused-cli`

Install `fused-cli` from the [CLI releases](https://github.com/Usefused/cli/releases), then point it at this Engine with `fused-cli config set engine-url <engine-url>`.

## Configuration

You can configure the Engine via a YAML configuration file (`engine.yaml`),
environment variables, or CLI flags. For the Registry license specifically,
precedence is `--license-key`, local `.env`, `engine.yaml`, then the inherited
`FUSED_LICENSE_KEY` environment variable. `FUSED_API_KEY` is a caller
control-plane credential and is never used to start the Engine.

For API and audit details, see [REST app execution and per-version OpenAPI export](docs/app-execution-rest.md), [Registry contract](docs/registry-contract.md), [license key behavior](docs/license-key.md), [telemetry](docs/telemetry.md), [Docker](docs/docker.md), and [threat model](THREAT_MODEL.md).

### Essential Environment Variables

```bash
# Database connection
FUSED_DATABASE_URL="postgres://fused:password@localhost:5432/fused?sslmode=disable"

# Optional pool ceiling. Hosted starter Engines default this to 2.
FUSED_DATABASE_MAX_CONNS=10

# Optional idle retention. MinConns is zero; periodic safety probes may keep
# one connection warm while burst/request connections remain eligible to close.
FUSED_DATABASE_MAX_CONN_IDLE_TIME=30m

# Your organization's Fused Cloud license key for the Engine (Required)
FUSED_LICENSE_KEY="<provided-by-fused>"

# Public addresses shown in the Engine Settings page. These do not create DNS
# or ingress routes; set them to the routes configured by your operator.
FUSED_ENGINE_PUBLIC_URL="https://engine.example.com"
FUSED_ENGINE_PUBLIC_GRPC_URL="https://engine-exec.example.com"

# (Optional) If you have a separate external NATS cluster, set this to override the embedded server.
# Leave blank to boot the internal embedded NATS.
# NATS_URL="nats://nats:4222" 

# External NATS authentication: configure exactly one of these methods.
# Keep credentials out of NATS_URL so startup errors and diagnostics cannot expose them.
# NATS_CREDS_FILE="/run/secrets/fused-engine.creds" # NATS operator/account JWT credentials
# NATS_NKEY_SEED_FILE="/run/secrets/fused-engine.nk"
# NATS_TOKEN="<secret>"
# NATS_USERNAME="fused-engine"
# NATS_PASSWORD="<secret>"

# Optional TLS/mTLS for external NATS. A client certificate may be combined
# with any one application authentication method above.
# NATS_TLS_CA_FILE="/run/secrets/nats-ca.pem"
# NATS_TLS_CERT_FILE="/run/secrets/nats-client.pem"
# NATS_TLS_KEY_FILE="/run/secrets/nats-client-key.pem"
# NATS_TLS_SERVER_NAME="nats.internal.example.com"

# Replicate live provider rate-limit state across this many JetStream nodes.
# Keep 1 for embedded NATS; otherwise match the JetStream replica count.
FUSED_NATS_RATE_LIMIT_REPLICAS=1

# OpenTelemetry configuration for distributed tracing
OTEL_SERVICE_NAME="engine"
OTEL_EXPORTER_OTLP_ENDPOINT="http://jaeger:4318"

# Optional: override the Fused Cloud Registry endpoint only when directed by Fused support.
# FUSED_REGISTRY_ENDPOINT="https://registry.usefused.com/graphql"
```

Engine is PostgreSQL-provider neutral and resolves one standard PostgreSQL DSN.
Hosted deployments supply it through `FUSED_DATABASE_URL`; existing
self-hosted YAML and `DATABASE_URL` settings are alternative configuration
sources, not additional connections. Engine contains no provider-specific
routing, port, or username rules. On startup, it creates or upgrades the tables
it owns through that connection before serving requests. Moving an existing
Engine database and its data between providers remains an operator-run
migration.

Newly generated TypeScript and Python SDK versions embed
`FUSED_ENGINE_PUBLIC_GRPC_URL` as their default Engine target. Applications can
override it with the SDK constructor option (`engineUrl` / `engine_url`) or the
`FUSED_ENGINE_GRPC_URL` and legacy `FUSED_ENGINE_URL` environment variables.
Existing generated versions remain immutable when this setting changes.

Provider quota admission uses JetStream KV as the live cluster-wide authority.
PostgreSQL receives an asynchronous, monotonic projection for recovery and
reporting, so normal provider calls do not wait on the database.

### CLI Flags

```text
Usage:
  fused-engine start [flags]

Flags:
      --grpc-port string     gRPC port for SDK connections (default "50051")
  -h, --help                 help for start
      --license-key string   License Key for Registry handshake
      --port string          HTTP port for API (default "8081")
      --webhook-port string  Dedicated HTTP port for Webhook Ingress (optional)

Global Flags:
      --config string   Path to configuration file (default "engine.yaml")
```

## Running Multiple Engine Replicas

Embedded NATS supports one Engine process. To run replicas of the same Engine
installation, give every replica the same PostgreSQL database, license and
encryption keys, and authenticated external JetStream account:

```bash
FUSED_DATABASE_URL="postgres://fused:<password>@postgres:5432/fused?sslmode=require"
FUSED_LICENSE_KEY="<provided-by-fused>"
FUSED_ENCRYPTION_KEY="<shared-encryption-key>"
NATS_URL="tls://nats-1:4222,tls://nats-2:4222,tls://nats-3:4222"
NATS_CREDS_FILE="/run/secrets/fused-engine.creds"
NATS_TLS_CA_FILE="/run/secrets/nats-ca.pem"
FUSED_NATS_RATE_LIMIT_REPLICAS=3
```

Apply these settings to every replica and place the HTTP and gRPC endpoints
behind load balancers. Independent Engine installations require separate
databases and NATS accounts.
