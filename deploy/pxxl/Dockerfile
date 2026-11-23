# syntax=docker/dockerfile:1

# Thin wrapper over the published Fused Engine OCI image for Pxxl (pxxl.app).
# Engine binds to 0.0.0.0:$PORT automatically via config.ResolveServerConfig.
ARG FUSED_ENGINE_VERSION=latest
FROM ghcr.io/usefused/engine:${FUSED_ENGINE_VERSION}

ENV PORT=8081 \
    FUSED_ENGINE_ENVIRONMENT=production \
    FUSED_DATABASE_MAX_CONN_IDLE_TIME=2m

EXPOSE 8081
