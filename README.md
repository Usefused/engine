<div align="center">
  <h1>Fused Engine</h1>
  <p>Self-hosted data plane for <a href="https://usefused.com">Fused</a></p>
  <br />
  <a href="https://usefused.com"><strong>Explore usefused.com</strong></a>
</div>

---

[Fused](https://usefused.com) is an integration control plane for managing third-party APIs, webhooks, and AI-agent tools.

The **Fused Engine** is the self-hosted runtime that applies workspace policies created in Fused. Deploy it in your own infrastructure to run configured API calls, MCP sessions, and webhook ingress close to your systems.

Fused Engine cannot run as a standalone/offline product. It requires a Fused Cloud license key from your Fused account, and startup exits if the key is missing or the Registry handshake fails.

After startup, Engine sends signed heartbeat checks to Fused Cloud so your workspace remains marked as a verified self-hosted runtime; missed heartbeats may cause support/cloud-managed features to treat the runtime as unverified until it reconnects.

Credentials are not scraped from arbitrary traffic. Engine only injects credentials into requests that are executed through Fused-configured SDK, MCP, webhook, or proxy routes and only from secrets that you explicitly store or connect. Runtime request payloads stay in the environment where you deploy the Engine.

## License

Fused Engine is **source-available, not open source**. The public source is
licensed under the [PolyForm Noncommercial License 1.0.0](LICENSE). Commercial
or production use requires a separate written agreement with Fused and a Fused
Cloud license key. See [Commercial Use](COMMERCIAL-USE.md).

## Features
- **Self-hosted runtime**: Process configured webhooks, SDK calls, and MCP requests inside your own infrastructure.
- **Explicit credential routing**: Inject credentials only for Fused-managed routes using secrets you configure.
- **Embedded NATS JetStream**: Instantly and reliably queues incoming webhooks and broadcasts WebSocket events without requiring an external NATS cluster or Redis instance to be deployed alongside it.
- **Headless Mode**: A no-UI Docker variant (`ghcr.io/usefused/engine:headless`) optimized for serverless and Kubernetes deployments.
- **Resilient**: Fully caches execution metadata locally to withstand network partitions.

## Prerequisites

- **PostgreSQL 16+**
- **Fused License Key** (Provided by your Fused onboarding contact)
- **Node.js 18+** (Required in your `$PATH` *only* if you are running the bare binary and plan to use MCP functionality. Docker images already include it.)

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
grep " ${ARCHIVE}$" checksums.txt > "${ARCHIVE}.sha256"
if command -v sha256sum >/dev/null; then
  sha256sum -c "${ARCHIVE}.sha256"
else
  shasum -a 256 -c "${ARCHIVE}.sha256"
fi
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

By default, the Engine will automatically detect that no external NATS cluster is configured and will boot its embedded NATS server on port `4222`. It will spin up the REST API on port `8081`, and the SDK gRPC connection on port `50051`. The Engine always talks to the Fused Cloud Registry -- there's nothing to configure here.

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

You can configure the Engine via a YAML configuration file (`engine.yaml`), environment variables, or CLI flags. CLI Flags always take the highest precedence.

For audit details, see [Registry contract](docs/registry-contract.md), [license key behavior](docs/license-key.md), [telemetry](docs/telemetry.md), [Docker](docs/docker.md), and [threat model](THREAT_MODEL.md).

### Essential Environment Variables

```bash
# Database connection
FUSED_DATABASE_URL="postgres://fused:password@localhost:5432/fused?sslmode=disable"

# Your organization's Fused Cloud license key for the Engine (Required)
FUSED_LICENSE_KEY="<provided-by-fused>"

# Tell the Engine where the Admin UI is hosted to configure CORS properly.
# (Defaults to the embedded Engine UI on http://localhost:8081, or you can use the --ui-url flag)
FUSED_UI_URL="https://admin.your-company.com"

# (Optional) If you have a separate external NATS cluster, set this to override the embedded server.
# Leave blank to boot the internal embedded NATS.
# NATS_URL="nats://nats:4222" 

# OpenTelemetry configuration for distributed tracing
OTEL_SERVICE_NAME="engine"
OTEL_EXPORTER_OTLP_ENDPOINT="http://jaeger:4318"
```

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
