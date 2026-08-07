.PHONY: build build-headless ui-install build-ui embed-ui runtime-mcp-build run run-headless test test-headless tidy proto-gen release-local release release-with-tag docker-build docker-build-headless docker-push docker-push-headless docker-build-push docker-build-push-headless

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo "0.1.0")
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "dev")
IMAGE_NAME ?= ghcr.io/usefused/engine

build: embed-ui runtime-mcp-build
	go build -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=$(VERSION) -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=$(COMMIT)" -o fused-engine ./cmd/engine

build-headless: runtime-mcp-build
	go build -tags headless -ldflags="-s -w -X github.com/Usefused/engine/cmd/engine/cmd.Version=$(VERSION) -X github.com/Usefused/engine/cmd/engine/cmd.BuildHash=$(COMMIT)" -o fused-engine-headless ./cmd/engine

ui-install:
	cd ui && npm ci

build-ui: ui-install
	cd ui && npm run build

embed-ui: build-ui
	rm -rf ui-build
	cp -R ui/build/client ui-build

runtime-mcp-build:
	cd runtime/mcp && npm ci && npm run build

run: build
	./fused-engine start --config engine.yaml

run-headless: build-headless
	./fused-engine-headless start --config engine.yaml

test: embed-ui
	go test ./...

test-headless:
	go test -tags headless ./...

tidy:
	go mod tidy

# Regenerate Go gRPC stubs from engine.proto.
# Requires protoc, protoc-gen-go, and protoc-gen-go-grpc on PATH.
proto-gen:
	protoc \
		--proto_path=proto \
		--go_out=. \
		--go_opt=module=github.com/Usefused/engine \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/Usefused/engine \
		proto/engine/v1/engine.proto

release-local:
	goreleaser release --snapshot --clean

release:
	goreleaser release --clean

release-with-tag:
	@if [ -z "$(VERSION)" ]; then echo "VERSION is required, e.g. make release-with-tag VERSION=v0.2.0"; exit 1; fi
	@git tag -a $(VERSION) -m "Release $(VERSION)"
	@git push origin $(VERSION)
	@goreleaser release --clean

PLATFORMS ?= linux/amd64,linux/arm64

docker-build:
	docker buildx build --platform $(PLATFORMS) --target full --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_NAME):$(VERSION) --load .

docker-build-headless:
	docker buildx build --platform $(PLATFORMS) --target headless --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_NAME):$(VERSION)-headless --load .

docker-push:
	docker push $(IMAGE_NAME):$(VERSION)

docker-push-headless:
	docker push $(IMAGE_NAME):$(VERSION)-headless

docker-build-push:
	docker buildx build --platform $(PLATFORMS) --target full --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_NAME):$(VERSION) --push .

docker-build-push-headless:
	docker buildx build --platform $(PLATFORMS) --target headless --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_NAME):$(VERSION)-headless --push .
