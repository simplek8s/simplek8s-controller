IMAGE ?= ghcr.io/simplek8s/simplek8s-controller
TAG ?= dev

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT)

.PHONY: all build test vet fmt image deploy deploy-minikube undeploy clean

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

# minikube kubelets cache images by tag (imagePullPolicy: IfNotPresent),
# so "image load" over an existing tag does NOT refresh running nodes.
# Replace the tag atomically: drain the DaemonSet, rm, load, restore.
deploy-minikube: image
	minikube kubectl -- apply -k deploy/
	minikube kubectl -- patch daemonset/simplek8s-controller -n simplek8s --type merge -p '{"spec":{"template":{"spec":{"nodeSelector":{"no-such-key":"true"}}}}}'
	@sleep 10
	-minikube image rm $(IMAGE):$(TAG)
	minikube image load $(IMAGE):$(TAG)
	minikube kubectl -- patch daemonset/simplek8s-controller -n simplek8s --type merge -p '{"spec":{"template":{"spec":{"nodeSelector":null}}}}'

undeploy:
	kubectl delete -k deploy/ --ignore-not-found

clean:
	rm -rf bin
