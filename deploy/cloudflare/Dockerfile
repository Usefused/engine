# syntax=docker/dockerfile:1

# Thin wrapper over the published Fused Engine OCI image so Cloudflare Containers
# builds do not require private Go module deploy keys.
ARG FUSED_ENGINE_VERSION=latest
FROM ghcr.io/usefused/engine:${FUSED_ENGINE_VERSION}

# Cloudflare Containers routes Worker traffic to the container's defaultPort (8081).
ENV FUSED_ENGINE_HTTP_PORT=8081
EXPOSE 8081
