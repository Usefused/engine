<div align="center">
  <h1>Fused Engine</h1>
  <p>The high-performance Data Plane and API Gateway for <a href="https://usefused.com">Fused</a></p>
  <br />
  <a href="https://usefused.com"><strong>Explore usefused.com</strong></a>
</div>

---

[Fused](https://usefused.com) is the unified integration control plane that helps engineering teams manage third-party APIs, webhooks, and AI-agent tools.

The **Fused Engine** is the enterprise **data plane** and **API gateway** that executes these integrations securely at scale. Deployed directly within your own infrastructure, the Engine acts as a unified proxy for all your third-party traffic. It intercepts outbound requests to automatically inject credentials and enforce rate limits, while securely receiving, validating, and queuing incoming webhooks. 

For businesses, this zero-trust architecture means your engineering teams can move faster without compromising security. Your sensitive runtime payloads—such as customer PII or AI context—never leave your VPC, and all integration observability is centralized in one place.

## Features
- **Zero-Trust Execution**: Process webhooks and MCP requests inside your own VPC.
- **Embedded NATS JetStream**: Instantly and reliably queues incoming webhooks and broadcasts WebSocket events without requiring an external NATS cluster or Redis instance to be deployed alongside it.
- **Headless Mode**: An ultra-lightweight Docker variant (`ghcr.io/usefused/fused:alpine`) optimized for serverless and Kubernetes deployments.
- **Resilient**: Fully caches execution metadata locally to withstand network partitions.

## Prerequisites

- **PostgreSQL 16+**
- **Fused License Key** (Provided by your Fused onboarding contact)

## Installation

### Option 1: Native CLI
Download the latest release for your operating system directly from our [Releases](https://github.com/Usefused/engine/releases) page.

```bash
# macOS / Linux
curl -sL https://github.com/Usefused/engine/releases/latest/download/fused-engine_$(uname -s)_$(uname -m).tar.gz | tar -xz
mv fused-engine /usr/local/bin/
```

### Option 2: Docker
For containerized environments, we provide both full and slim/alpine images. The Alpine image is optimized for maximum performance and minimal footprint.

```bash
docker pull ghcr.io/usefused/fused:latest
docker pull ghcr.io/usefused/fused:alpine
```

## Quick Start

The Fused Engine requires a **License Key** to successfully handshake with your Fused account. You'll also need to point it to your PostgreSQL database.

```bash
# 1. Export your database URL and License Key
export FUSED_DATABASE_URL="postgres://fused:password@localhost:5432/fused?sslmode=disable"
export FUSED_LICENSE_KEY="<provided-by-fused>"

# 2. Start the Engine
fused-engine start
```

By default, the Engine will automatically detect that no external NATS cluster is configured and will boot its embedded NATS server on port `4222`. It will spin up the REST API on port `8081`, and the SDK gRPC connection on port `50051`.

### Docker Compose Example (with UI)
Our full Docker image (`latest`) bundles both the Engine and the Admin Dashboard. The headless image (`alpine`) runs the same Engine API without the embedded UI.

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
    image: ghcr.io/usefused/fused:latest
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

### Essential Environment Variables

```bash
# Database connection
FUSED_DATABASE_URL="postgres://fused:password@localhost:5432/fused?sslmode=disable"

# Your organization's license key for the Engine (Required)
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
