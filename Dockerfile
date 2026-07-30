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
COPY runtime/mcp/ ./
RUN npm run build

FROM golang:1.25-alpine AS engine-base

WORKDIR /app

RUN apk upgrade --no-cache && apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
ARG VERSION=dev
ARG COMMIT=dev

FROM engine-base AS engine-headless-builder

RUN CGO_ENABLED=0 GOOS=linux go build -tags headless \
    -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=${VERSION} -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=${COMMIT}" \
    -o /out/fused-engine ./cmd/engine

FROM engine-base AS engine-embedded-builder

COPY --from=ui-builder /app/build/client ./ui-build
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=${VERSION} -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=${COMMIT}" \
    -o /out/fused-engine ./cmd/engine

FROM node:24-alpine AS engine-runtime-base

WORKDIR /app

# Node and npm remain part of the slim runtime because MCP sessions execute in
# isolated Node processes and initialize their shared runtime dependencies.
RUN apk upgrade --no-cache && \
    apk add --no-cache bash su-exec nats-server tini

COPY --from=engine-base /app/runtime /app/runtime
COPY --from=mcp-runtime-builder /app/runtime/mcp/dist /app/runtime/mcp/dist
RUN cd /app/runtime/mcp && npm ci --omit=dev

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
