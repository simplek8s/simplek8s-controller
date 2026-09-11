IMAGE ?= ghcr.io/simplek8s/simplek8s-controller
TAG ?= dev

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT)

# Multi-arch image (PLAN.md §3.11): buildx validates linux/amd64 +
# linux/arm64 (fails if either fails); the host-arch image is then
# loaded into the local store (a classic store holds one platform).
# BUILDER is created once (idempotent); builds address it explicitly so
# no global builder config is mutated.
BUILDER ?= simplek8s-builder
HOST_ARCH := $(shell uname -m | sed -e 's/^x86_64$$/amd64/' -e 's/^aarch64$$/arm64/')

.PHONY: all build test vet fmt image deploy undeploy clean

all: vet test build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/simplek8s-controller ./cmd/simplek8s-controller

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

image:
	@scripts/binfmt-check.sh
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || docker buildx create --name $(BUILDER) --driver docker-container >/dev/null
	docker buildx build --builder $(BUILDER) --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILT=$(BUILT) .
	docker buildx build --builder $(BUILDER) --platform linux/$(HOST_ARCH) --load \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILT=$(BUILT) \
		-t $(IMAGE):$(TAG) .

deploy:
	kubectl apply -k deploy/

undeploy:
	kubectl delete -k deploy/ --ignore-not-found

clean:
	rm -rf bin
