<div align="center">
  <h1>Fused Engine</h1>
  <p>The high-performance Data Plane for Fused</p>
</div>

---

**Fused Engine** is the secure, high-performance execution environment and data plane for your generated SDKs, Webhooks, and MCP servers. 

While the Fused Registry (Control Plane) manages your configurations, the Engine lives directly inside your infrastructure, securely proxying API requests and executing data integrations without your sensitive payloads ever touching external servers.

## Features
- **Zero-Trust Execution**: Process webhooks and MCP requests inside your own VPC.
- **Single-Binary UI**: The standard Engine binary comes fully bundled with a local monitoring dashboard.
- **Headless Mode**: An ultra-lightweight Docker variant (`fused-alpine`) optimized for serverless and Kubernetes deployments.
- **Resilient**: Fully caches execution metadata locally to withstand network partitions.

## Installation

### Option 1: Native CLI
Download the latest release for your operating system directly from our [Releases](https://github.com/Usefused/engine/releases) page.

```bash
# macOS / Linux
curl -sL https://github.com/Usefused/engine/releases/latest/download/fused-engine_$(uname -s)_$(uname -m).tar.gz | tar -xz
mv fused-engine /usr/local/bin/
```

### Option 2: Docker
For containerized environments, we provide an ultra-lean Alpine image that strips the embedded UI for maximum performance.

```bash
docker pull ghcr.io/usefused/engine:latest
```

## Quick Start

The Fused Engine requires a **License Key** to successfully handshake with your Fused account. 

### 1. Set your License Key
```bash
export FUSED_LICENSE_KEY="your-license-key-here"
```

### 2. Start the Engine
```bash
fused-engine start
```

By default, the Engine will spin up the REST API & Dashboard on port `8081`, and the SDK gRPC connection on port `50051`. 

## Configuration

You can configure the Engine via a YAML configuration file (`engine.yaml`), environment variables, or CLI flags. 

CLI Flags always take the highest precedence:

```text
Usage:
  fused-engine start [flags]

Flags:
      --grpc-port string     gRPC port for SDK connections (default "50051")
  -h, --help                 help for start
      --license-key string   License Key for Registry handshake
      --port string          HTTP port for API and UI (default "8081")
      --ui-url string        URL for the UI (overrides engine.yaml)

Global Flags:
      --config string   Path to configuration file (default "engine.yaml")
```

## Architecture

Fused uses a split Control Plane / Data Plane architecture:
1. **Registry (Control Plane)**: Manages your schemas, auth configurations, and generates SDKs.
2. **Engine (Data Plane)**: This repository. It pulls metadata from the Registry on startup, but handles all execution routing locally.

*Note: The Engine will refuse to boot if a valid `FUSED_LICENSE_KEY` is not provided to complete the handshake.*
