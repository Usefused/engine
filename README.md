<a id="fused-engine"></a>

# <img src=".github/readme/engine.svg" width="30" height="30" alt=""> Fused Engine

**Fused - Connect Your APIs Once, Use Everywhere**

Bring internal, partner, and third-party APIs into Fused. Your teams, apps, and AI agents can reuse those integrations while you stay in control of who can access what.

[Run locally](#install-and-start) · [Connect an app or agent](#create-an-integration-interface) · [Documentation](https://usefused.com/docs) · [Fused Cloud](https://usefused.com/docs/deploy-an-engine)

## How it works

| Component | What it does |
| --- | --- |
| **Registry** | Describes the operations and authentication requirements for each API version. |
| **Engine** | Checks permissions, resolves credentials, runs the API call, and records the result. |
| **[CLI](https://github.com/Usefused/cli)** | Lets developers select operations, review changes, and publish apps from their terminal. |

## Install and start

For a local install on macOS or Linux, have these ready:

- **PostgreSQL 16+** for workspace data.
- **A [Fused license key](https://usefused.com/signup)** to start Engine.
- **Node.js 24.x, version 24.15 or later**, for MCP processes and Unified App compilation. Docker images include it. For source builds, run `nvm install` and `nvm use` before npm commands.

### 1. Install Engine and the CLI

```bash
curl -fsSL https://raw.githubusercontent.com/Usefused/engine/main/install.sh | bash
curl -fsSL https://raw.githubusercontent.com/Usefused/cli/main/install.sh | bash
export PATH="$HOME/.local/bin:$PATH"
```

### 2. Start Engine

Generate an encryption key once with `openssl rand -base64 32`. Keep the same key across restarts so Engine can read stored credentials.

```bash
export FUSED_DATABASE_URL='postgres://fused:password@localhost:5432/fused?sslmode=disable'
export FUSED_LICENSE_KEY='<your-license-key>'
export FUSED_ENCRYPTION_KEY='<your-generated-encryption-key>'
fused-engine start
```

Open the **[Admin UI](http://localhost:8081)**. Engine creates its tables and starts embedded NATS automatically. The SDK gRPC endpoint listens on port `50051`.

### 3. Connect from a second terminal

```bash
fused-cli config set engine-url http://localhost:8081
fused-cli login
fused-cli whoami
```

<details>
<summary><strong>File configuration and Docker deployments</strong></summary>

**Configuration file.** Copy [engine.yaml](engine.yaml), replace its placeholders, and remove the local `registry_endpoint` to use the Fused Cloud Registry. Start Engine with `fused-engine start --config /path/to/engine.yaml`. Existing environment variables take precedence over YAML defaults. See the [configuration reference](docs/engine-configuration.md).

**Docker and hosted setup.** Follow the [deployment guide](https://usefused.com/docs/deploy-an-engine). For Unified Apps in Docker, extract the [bundled worker files](deploy/unified-app-workers/README.md) before launch. Both image variants include the seccomp profile, its license, and a Compose example at `/app/deploy/unified-app-workers`. Release archives include the same files. Apply the profile with Docker’s `--security-opt`, set `FUSED_UNIFIED_APP_WORKER_REQUIRED=true`, and verify that `/health` reports `unified_app_worker_ready: true`.

</details>

## Create an integration interface

Choose how your app or agent will use Linear. Each command lets you select operations and review the configuration before setup.

| Client | Run | Guide |
| --- | --- | --- |
| **Application code** | `fused-cli init support-sdk --sdk --service linear` | [Typed SDK](https://usefused.com/docs/quickstart) |
| **An AI agent** | `fused-cli init support-mcp --mcp --service linear` | [MCP server](https://usefused.com/docs/mcp/deploy-a-server) |
| **An HTTP client** | `fused-cli init support-api --api --service linear` | [REST API](https://usefused.com/docs/app/testing/over-rest) |

Omit the mode flag to choose interactively. To inspect the callable schema for an SDK version, see the [per-version OpenAPI export](docs/app-execution-rest.md#export-an-openapi-document).

## Can't find the service you're looking for?

Import your own API specification. Replace the example URL, review the plan, and apply it before creating your SDK.

```bash
fused-cli import plan --url https://api.example.com/openapi.yaml --name "Billing API" --slug billing-api
fused-cli import apply
fused-cli init billing-app --sdk --service billing-api
```

The [API import guide](https://usefused.com/docs/workspace/bring-your-own-api) covers local files and other supported formats.

## Scheduled Unified App runs

Use a Unified App’s **Schedules** tab or `fused-cli app schedule` to run it once or on a recurring schedule with saved JSON input. Schedules survive Engine restarts and use the active app version and its configured credentials. See [scheduled runs](docs/unified-app-schedules.md) for examples, permissions, and timezone behavior.
