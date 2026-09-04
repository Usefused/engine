# Fused Engine

Fused is an integration gateway for applications and AI agents. Connect to
internal and external services through typed SDKs, a direct API, or MCP servers,
with credentials, access policies, retries, and execution records handled by the Engine.

**[Documentation](https://docs.usefused.com)** ·
[Quickstart](https://docs.usefused.com/quickstart) ·
[Website](https://usefused.com)

## How it works

- **Registry** turns API descriptions into versioned service contracts.
- **Engine** runs approved operations in your workspace and manages provider credentials.
- **CLI** configures workspaces, SDKs, and MCP servers through a reviewed `plan` → `apply` workflow.

## Install and start

You need **PostgreSQL 16+** and a **[Fused license key](https://usefused.com/signup)**.
Install the Engine and CLI on macOS or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/Usefused/engine/main/install.sh | bash
curl -fsSL https://raw.githubusercontent.com/Usefused/cli/main/install.sh | bash
export PATH="$HOME/.local/bin:$PATH"
```

Set your database connection, license key, and encryption key, then start the Engine.
Generate an encryption key once with `openssl rand -base64 32` and keep it for
subsequent starts so stored credentials remain readable.

```bash
export FUSED_DATABASE_URL='postgres://fused:password@localhost:5432/fused?sslmode=disable'
export FUSED_LICENSE_KEY='<your-license-key>'
export FUSED_ENCRYPTION_KEY='<your-generated-encryption-key>'
fused-engine start
```

The Engine creates its database tables and starts embedded NATS automatically.
Open the Admin UI at [localhost:8081](http://localhost:8081); SDK gRPC listens on `:50051`.

For file-based configuration, see **[engine.yaml](https://github.com/Usefused/engine/blob/main/engine.yaml)**
and start with `fused-engine start --config /path/to/engine.yaml`.
Replace its placeholder values and remove the local `registry_endpoint` to use
the default Fused Cloud Registry.

In another terminal, connect the CLI:

```bash
fused-cli config set engine-url http://localhost:8081
fused-cli login
fused-cli whoami
```

See the [deployment guide](https://docs.usefused.com/deploy-an-engine) for Docker
and hosted Engine setup.

## Create an integration interface

With the Engine running, give your application or agent a way to call the
services it needs. `fused-cli init` creates an integration interface exposing
the operations you select, while the Engine handles credentials, access
policies, and execution.

Choose a typed SDK to call services from your code, an MCP server to make tools
available to an AI agent, or a REST API to send HTTP requests without installing
a generated package. The examples below use Linear:

| Interface | Command | Documentation |
|---|---|---|
| Typed SDK | `fused-cli init support-sdk --sdk --service linear` | [SDK quickstart](https://docs.usefused.com/quickstart) |
| MCP server | `fused-cli init support-mcp --mcp --service linear` | [MCP server guide](https://docs.usefused.com/mcp/deploy-a-server) |
| REST API | `fused-cli init support-api --api --service linear` | [REST API guide](https://docs.usefused.com/app/testing/over-rest) |

Each command guides you through selecting operations and approving setup.
Omit the mode flag to choose interactively.

## Can't find the service you're looking for?

Import its OpenAPI spec, review the plan, then apply it.
Replace the example URL with your OpenAPI spec URL:

```bash
fused-cli import plan --url https://api.example.com/openapi.yaml --name "Billing API" --slug billing-api
fused-cli import apply
fused-cli init billing-app --sdk --service billing-api
```

See the [API import guide](https://docs.usefused.com/workspace/bring-your-own-api)
for local file imports and other supported formats.
