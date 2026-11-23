import { defineRailway, postgres, project, service, volume } from "railway/iac";

/** Declares a Railway project with managed PostgreSQL 16, persistent Engine storage, and Fused Engine. */
export default defineRailway(() => {
  // Provision Railway's managed PostgreSQL service so credentials, backups, and startup ordering are handled automatically.
  const db = postgres("postgres");

  // Retain embedded NATS state and runtime scratch caches across container restarts.
  const engineData = volume("fused-engine-data", { sizeMB: 1024 });

  // Wire Fused Engine to the managed PostgreSQL DSN while keeping secrets in Railway project variables.
  const engine = service("fused-engine", {
    healthcheck: "/health",
    volumeMounts: {
      "/app/data": engineData,
    },
    env: {
      FUSED_DATABASE_URL: db.env.DATABASE_URL,
      FUSED_ENGINE_ENVIRONMENT: "production",
      FUSED_DATABASE_MAX_CONNS: "10",
      FUSED_DATABASE_MAX_CONN_IDLE_TIME: "2m",
    },
  });

  return project("fused-engine", {
    resources: [db, engineData, engine],
  });
});
