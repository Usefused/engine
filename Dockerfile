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

# Compile reviewed Unified App source inside Engine. The compiler and its
# pinned dependencies stay in the image, never in a tenant app sandbox.
FROM node:24-alpine AS execution-compiler-builder

WORKDIR /app/runtime/execution
RUN apk upgrade --no-cache
COPY runtime/execution/package.json runtime/execution/package-lock.json ./
RUN npm ci
COPY runtime/execution/tsconfig.json ./
COPY runtime/execution/src ./src
RUN npm run build && npm prune --omit=dev

# Build with the patched standard library required by go.mod.
FROM golang:1.26.6-alpine AS engine-base

WORKDIR /app

RUN apk upgrade --no-cache && apk add --no-cache git openssh-client

COPY go.mod go.sum ./
# Pin GitHub's published host key instead of trusting a key fetched over the build network.
COPY .github/actions/setup-private-core/known_hosts /etc/ssh/ssh_known_hosts
# BuildKit mounts the deploy key only for this download; no credential enters an image layer.
RUN --mount=type=secret,id=fused_open_core_key,required=true \
    GOPRIVATE=github.com/Usefused/fused-open-core \
    GIT_SSH_COMMAND="ssh -i /run/secrets/fused_open_core_key -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes" \
    GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0=url.ssh://git@github.com/Usefused/fused-open-core.insteadOf \
    GIT_CONFIG_VALUE_0=https://github.com/Usefused/fused-open-core \
    go mod download

COPY . ./
ARG VERSION=dev
ARG COMMIT=dev

# The MCP package is bundled before Go compilation so its complete dependency
# graph is embedded in the Engine binary instead of installed into tenant data
# at startup. Copying it into this shared stage also prevents a stale checked-in
# dist file from entering either image variant.
FROM engine-base AS engine-source

# Both embedded MCP runtime artifacts must land here: bundle.js (the Node
# server runtime) and metadata-bundle.js (the browser-side catalogue/tool
# declarations embed). Missing either one fails the go:embed directives in
# runtime/embed.go during the later builder stages.
COPY --from=mcp-runtime-builder /app/runtime/mcp/dist/bundle.js /app/runtime/mcp/dist/bundle.js
COPY --from=mcp-runtime-builder /app/runtime/mcp/dist/metadata-bundle.js /app/runtime/mcp/dist/metadata-bundle.js

FROM engine-source AS engine-headless-builder

RUN CGO_ENABLED=0 GOOS=linux go build -tags headless \
    -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=${VERSION} -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=${COMMIT}" \
    -o /out/fused-engine ./cmd/engine

# The authored-code process must be a small, standalone binary so each
# invocation does not map the full Engine server into its memory limit.
FROM engine-source AS execution-worker-builder

RUN CGO_ENABLED=0 GOOS=linux go build -tags headless \
    -ldflags="-s -w" -o /out/fused-execution-worker ./cmd/execution-worker

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
    apk add --no-cache bash ca-certificates su-exec nats-server tini

RUN addgroup -S fused && adduser -S -G fused fused && \
    mkdir -p /app/data/sandboxes && \
    chown -R fused:fused /app

EXPOSE 8081 50051

FROM engine-runtime-base AS engine-runtime

COPY --from=engine-headless-builder /out/fused-engine /app/fused-engine
COPY --from=execution-compiler-builder /app/runtime/execution /app/runtime/execution
COPY engine.yaml /app/engine.yaml
COPY entrypoint.sh /app/entrypoint.sh

FROM engine-runtime AS headless

COPY --from=execution-worker-builder /out/fused-execution-worker /app/fused-execution-worker

ENTRYPOINT ["/sbin/tini", "--", "/app/entrypoint.sh"]
CMD ["/app/fused-engine", "start"]

FROM engine-runtime-base AS embedded

COPY --from=engine-embedded-builder /out/fused-engine /app/fused-engine
COPY --from=execution-worker-builder /out/fused-execution-worker /app/fused-execution-worker
COPY --from=execution-compiler-builder /app/runtime/execution /app/runtime/execution
COPY engine.yaml /app/engine.yaml
COPY entrypoint.sh /app/entrypoint.sh

ENTRYPOINT ["/sbin/tini", "--", "/app/entrypoint.sh"]
CMD ["/app/fused-engine", "start"]

FROM headless AS slim

FROM embedded AS full
