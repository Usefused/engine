import { Container } from "@cloudflare/containers";

export interface Env {
  FUSED_ENGINE: DurableObjectNamespace<FusedEngineContainer>;
  FUSED_DATABASE_URL?: string;
  FUSED_LICENSE_KEY?: string;
  FUSED_ENCRYPTION_KEY?: string;
  FUSED_ENGINE_PUBLIC_URL?: string;
  FUSED_ENGINE_ENVIRONMENT?: string;
  FUSED_DATABASE_MAX_CONNS?: string;
  FUSED_DATABASE_MAX_CONN_IDLE_TIME?: string;
  FUSED_REGISTRY_ENDPOINT?: string;
  NATS_URL?: string;
  NATS_TOKEN?: string;
  NATS_USERNAME?: string;
  NATS_PASSWORD?: string;
}

const REQUIRED_SECRETS = [
  "FUSED_DATABASE_URL",
  "FUSED_LICENSE_KEY",
  "FUSED_ENCRYPTION_KEY",
] as const;

const FORWARDED_ENV_KEYS = [
  "FUSED_DATABASE_URL",
  "FUSED_LICENSE_KEY",
  "FUSED_ENCRYPTION_KEY",
  "FUSED_ENGINE_PUBLIC_URL",
  "FUSED_ENGINE_ENVIRONMENT",
  "FUSED_DATABASE_MAX_CONNS",
  "FUSED_DATABASE_MAX_CONN_IDLE_TIME",
  "FUSED_REGISTRY_ENDPOINT",
  "NATS_URL",
  "NATS_TOKEN",
  "NATS_USERNAME",
  "NATS_PASSWORD",
] as const;

/** Collects defined Worker secrets and vars to forward into the Fused Engine container. */
function buildEngineContainerEnv(env: Env): Record<string, string> {
  const vars: Record<string, string> = {
    FUSED_ENGINE_HTTP_PORT: "8081",
  };
  for (const key of FORWARDED_ENV_KEYS) {
    const value = env[key];
    // Skip unset or blank values so Engine preserves its built-in defaults instead of reading empty strings.
    if (typeof value === "string" && value.trim() !== "") {
      vars[key] = value;
    }
  }
  return vars;
}

/** Returns the names of any required Fused Engine secrets missing from the Worker environment. */
function missingRequiredSecrets(env: Env): string[] {
  return REQUIRED_SECRETS.filter((key) => {
    const value = env[key];
    // Reject missing or whitespace-only secrets before waking the container.
    return typeof value !== "string" || value.trim() === "";
  });
}

/** Runs a singleton Fused Engine container behind a Cloudflare Durable Object proxy. */
export class FusedEngineContainer extends Container<Env> {
  defaultPort = 8081;
  sleepAfter = "15m";

  /** Binds Worker secrets and configuration into the container process environment. */
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.envVars = buildEngineContainerEnv(env);
  }
}

export default {
  /** Validates required Engine secrets and forwards HTTP traffic to the singleton Fused Engine container. */
  async fetch(request: Request, env: Env): Promise<Response> {
    const missing = missingRequiredSecrets(env);
    // Surface missing secret names clearly without leaking values or starting an unconfigured Engine.
    if (missing.length > 0) {
      return Response.json(
        {
          error: "Fused Engine is missing required Cloudflare Worker secrets.",
          missing_secrets: missing,
        },
        { status: 503 }
      );
    }

    // Route traffic to one named container instance so embedded NATS remains coherent.
    const id = env.FUSED_ENGINE.idFromName("primary");
    const stub = env.FUSED_ENGINE.get(id);
    return stub.fetch(request);
  },
};
