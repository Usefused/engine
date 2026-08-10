# syntax=docker/dockerfile:1

FROM node:24-alpine AS ui-builder

WORKDIR /app

RUN apk upgrade --no-cache
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm rebuild && NODE_OPTIONS="--max-old-space-size=1536" npm run build

FROM node:24-alpine AS mcp-runtime-builder

WORKDIR /app/runtime/mcp

RUN apk upgrade --no-cache
COPY runtime/mcp/package.json runtime/mcp/package-lock.json ./
RUN npm ci
# Why: copying the whole development directory can replace the container's
# platform-specific npm binaries with host node_modules (for example Mach-O
# esbuild in a Linux/ARM64 build). Keep this stage limited to build inputs.
COPY runtime/mcp/tsconfig.json runtime/mcp/tsconfig.build.json ./
COPY runtime/mcp/src ./src
RUN npm run build

FROM golang:1.25-alpine AS engine-base

WORKDIR /app

RUN apk upgrade --no-cache && apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
ARG VERSION=dev
ARG COMMIT=dev

# The MCP package is bundled before Go compilation so its complete dependency
# graph is embedded in the Engine binary instead of installed into tenant data
# at startup. Copying it into this shared stage also prevents a stale checked-in
# dist file from entering either image variant.
FROM engine-base AS engine-source

COPY --from=mcp-runtime-builder /app/runtime/mcp/dist/bundle.js /app/runtime/mcp/dist/bundle.js

FROM engine-source AS engine-headless-builder

RUN CGO_ENABLED=0 GOOS=linux go build -tags headless \
    -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=${VERSION} -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=${COMMIT}" \
    -o /out/fused-engine ./cmd/engine

FROM engine-source AS engine-embedded-builder

COPY --from=ui-builder /app/build/client ./ui-build
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=${VERSION} -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=${COMMIT}" \
    -o /out/fused-engine ./cmd/engine

FROM node:24-alpine AS engine-runtime-base

WORKDIR /app

# Node remains part of the slim runtime because MCP sessions execute in
# isolated processes. Their JavaScript dependencies are already bundled into
# the Go binary, so containers never run npm against tenant storage.
RUN apk upgrade --no-cache && \
    apk add --no-cache bash su-exec nats-server tini

RUN addgroup -S fused && adduser -S -G fused fused && \
    mkdir -p /app/data/sandboxes && \
    chown -R fused:fused /app

EXPOSE 8081 50051

FROM engine-runtime-base AS engine-runtime

COPY --from=engine-headless-builder /out/fused-engine /app/fused-engine
COPY engine.yaml /app/engine.yaml
COPY entrypoint.sh /app/entrypoint.sh

FROM engine-runtime AS headless

ENTRYPOINT ["/sbin/tini", "--", "/app/entrypoint.sh"]
CMD ["/app/fused-engine", "start"]

FROM engine-runtime-base AS embedded

COPY --from=engine-embedded-builder /out/fused-engine /app/fused-engine
COPY engine.yaml /app/engine.yaml
COPY entrypoint.sh /app/entrypoint.sh

ENTRYPOINT ["/sbin/tini", "--", "/app/entrypoint.sh"]
CMD ["/app/fused-engine", "start"]

FROM headless AS slim

FROM embedded AS full
