IMAGE ?= ghcr.io/simplek8s/simplek8s-controller
TAG ?= dev

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT)

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
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILT=$(BUILT) -t $(IMAGE):$(TAG) .

deploy:
	kubectl apply -k deploy/

undeploy:
	kubectl delete -k deploy/ --ignore-not-found

clean:
	rm -rf bin
