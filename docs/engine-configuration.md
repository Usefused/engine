# Engine configuration

Start Fused with `fused-engine start --config /path/to/engine.yaml`.
A missing or empty file retains the existing environment-only startup behavior.

At startup, Fused loads and validates YAML, then publishes explicitly declared
operator settings to the process environment using `os.Setenv`. Database, NATS,
and telemetry consumers keep reading their existing `os.Getenv` settings.
The bridge runs before telemetry, database connections, NATS, or runtime workers
initialize. It does not write a `.env` file or change the parent shell.

For the settings below, precedence is:

1. Explicit CLI flags, where supported.
2. Existing nonempty environment variables, including loaded `.env` values.
3. Explicit YAML values.
4. Existing subsystem defaults.

Omitted YAML keys do not export their defaults or clear environment variables.
Restart the Engine to apply changes. The existing license-key source resolution
is unchanged and is separate from this operator-settings bridge.

```yaml
database:
  max_conns: 10
  max_conn_idle_time: 30m
server:
  http_port: 8081
  grpc_host: 127.0.0.1
  grpc_port: 50051
  webhook_port: ""
nats:
  url: nats://broker:4222
  rate_limit_replicas: 1
  credentials_file: /run/secrets/nats.creds
observability:
  environment: staging
  service_name: fused
  otel_target: http://collector:4318
engine:
  unified_apps:
    max_concurrency: 1
    queue_capacity: 256
    queue_timeout_seconds: 5
```

## Database and listeners

| YAML field | Environment variable | Default / CLI flag |
| --- | --- | --- |
| `database.url` | `FUSED_DATABASE_URL` | Existing `DATABASE_URL` alias also takes priority over YAML |
| `database.max_conns` | `FUSED_DATABASE_MAX_CONNS` | `10`, positive integer |
| `database.max_conn_idle_time` | `FUSED_DATABASE_MAX_CONN_IDLE_TIME` | `30m`, positive Go duration |
| `server.http_port` | `FUSED_ENGINE_HTTP_PORT` | `8081`; `--port` |
| `server.grpc_host` | `FUSED_ENGINE_GRPC_HOST` | `127.0.0.1`; `--grpc-host` |
| `server.grpc_port` | `FUSED_ENGINE_GRPC_PORT` | `50051`; `--grpc-port` |
| `server.webhook_port` | `FUSED_ENGINE_WEBHOOK_PORT` | Empty shares HTTP; `--webhook-port` |

Listener environment variables are also supported when there is no YAML file.
Pool minimum connections remain zero so idle Engines can release database slots.

## NATS

| YAML field | Existing environment variable |
| --- | --- |
| `nats.url` | `NATS_URL` |
| `nats.store_dir` | `FUSED_NATS_STORE_DIR` |
| `nats.rate_limit_replicas` | `FUSED_NATS_RATE_LIMIT_REPLICAS` |
| `nats.credentials_file` | `NATS_CREDS_FILE` |
| `nats.nkey_seed_file` | `NATS_NKEY_SEED_FILE` |
| `nats.tls.ca_file` | `NATS_TLS_CA_FILE` |
| `nats.tls.cert_file` | `NATS_TLS_CERT_FILE` |
| `nats.tls.key_file` | `NATS_TLS_KEY_FILE` |
| `nats.tls.server_name` | `NATS_TLS_SERVER_NAME` |

Omit the URL for embedded NATS. `store_dir` applies to the embedded broker.
Replication defaults to one; values 1–5 require sufficient JetStream servers.
Existing authentication and TLS validation still applies, including paired client
certificate/key files and only one authentication method.

Tokens and username/password credentials remain available through `NATS_TOKEN`,
`NATS_USERNAME`, and `NATS_PASSWORD`. An environment authentication method takes
priority over YAML credential-file choices as a group; Fused will not combine an
inherited token with a lower-priority YAML credentials file.

## Observability

| YAML field | Existing environment variable |
| --- | --- |
| `observability.environment` | `FUSED_ENGINE_ENVIRONMENT` (`--environment` wins) |
| `observability.service_name` | `OTEL_SERVICE_NAME` |
| `observability.otel_target` | `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `observability.traces_endpoint` | `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` |
| `observability.metrics_endpoint` | `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` |
| `observability.logs_endpoint` | `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` |

Use a shared OTLP HTTP base URL, such as `http://collector:4318`, or full
per-signal URLs including `/v1/traces`, `/v1/metrics`, or `/v1/logs`.
Legacy shared `host:port` targets are exported as `http://host:port`.
An inherited shared OTLP endpoint takes priority over YAML signal endpoints;
otherwise YAML signal endpoints take priority over the YAML shared target.
Standard OTel headers, TLS, and other exporter environment options continue to work.

## Unified Apps

The existing `engine.unified_apps.max_concurrency`, `queue_capacity`, and
`queue_timeout_seconds` fields also publish to their corresponding
`FUSED_UNIFIED_APP_MAX_CONCURRENCY`, `FUSED_UNIFIED_APP_QUEUE_CAPACITY`, and
`FUSED_UNIFIED_APP_QUEUE_TIMEOUT_SECONDS` variables. Their existing validation
and environment precedence remain unchanged, including rejection of explicitly
empty admission overrides.

## Environment references inside YAML

YAML scalar values may reference an existing variable using `${VARIABLE}`:

```yaml
database:
  url: "${DEPLOYMENT_DATABASE_URL}"
  max_conns: "${DEPLOYMENT_DATABASE_POOL_SIZE}"
```

References are expanded before validation. Missing references fail startup;
numeric and boolean fields accept whole-value references. Substitution is a
single pass over values, so environment contents cannot inject YAML keys or
trigger further substitution. Direct environment configuration does not require
any reference or corresponding YAML entry.

Container CPU, memory, and process/PID limits remain deployment settings in
Docker or Kubernetes, outside `engine.yaml`.
