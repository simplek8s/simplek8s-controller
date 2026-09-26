IMAGE ?= ghcr.io/simplek8s/simplek8s-controller
TAG ?= latest

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
TS      := $(shell date -u +%Y%m%d%H%M)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT)

# Multi-arch image (PLAN.md §3.11): buildx validates linux/amd64 +
# linux/arm64 (fails if either fails); the host-arch image is then
# loaded into the local store (a classic store holds one platform).
# BUILDER is created once (idempotent); builds address it explicitly so
# no global builder config is mutated.
BUILDER ?= simplek8s-builder
HOST_ARCH := $(shell uname -m | sed -e 's/^x86_64$$/amd64/' -e 's/^aarch64$$/arm64/')

.PHONY: all build build-simplek8s-controller build-nodectl publish-nodectl test test-go test-gocyclo test-misspell vet fmt image deploy undeploy clean mrproper

all: vet test build

# build compiles everything (both arches, like the distro ships it):
# the controller plus the node CLI, all static.
build: build-simplek8s-controller build-nodectl

build-simplek8s-controller:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/simplek8s-controller.$(TS).x86-64 ./cmd/simplek8s-controller
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/simplek8s-controller.$(TS).arm64 ./cmd/simplek8s-controller

# nodectl node CLI (PLAN-M6): static binaries for both arches.
CLI ?= nodectl

# Stamp the channel release TS into the CLI binary (same TS as the
# output filename, hence the published index; PLAN.md §3.14 D14):
# a stamped local build newer than the index is skipped by
# selfupdate instead of downgraded. Unstamped binaries keep the
# checksum-only rule.
CLI_LDFLAGS := $(LDFLAGS) -X main.releaseTS=$(TS)

build-nodectl: cli-keyring
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(CLI_LDFLAGS)" -o build/$(CLI).$(TS).x86-64 ./cmd/nodectl
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(CLI_LDFLAGS)" -o build/$(CLI).$(TS).arm64 ./cmd/nodectl

# The go:embed keyring copy (single source of truth: keys/, LFS).
# Fails if the file is still an LFS pointer (no smudge) — an
# embedded pointer would silently ship without verification.
cli-keyring:
	@if head -c 23 keys/simplek8s-pubring.gpg | grep -q '^version https://git-lfs'; then \
		echo "keys/simplek8s-pubring.gpg is an LFS pointer (run: git lfs pull)" >&2; exit 1; \
	fi
	@cp keys/simplek8s-pubring.gpg cmd/nodectl/pubring.gpg

# No kube-coupled imports in the local-only CLI (PLAN-M6 reuse
# boundary, enforced by `go list`). Depends on cli-keyring so the
# embedded file exists; a `go list` load failure fails the check
# instead of passing vacuously.
cli-no-kube: cli-keyring
	@deps=$$(go list -deps ./cmd/nodectl) || { echo "go list failed" >&2; exit 1; }; \
	echo "$$deps" | grep -E 'internal/(kube|engine)' && { echo "cmd/nodectl imports kube-coupled packages" >&2; exit 1; } || true

# Publish CLI binaries (PLAN-M6 D10 publish-nodectl, ported from the
# legacy simplek8s-update Makefile; recipe not yet run against the
# live endpoint): upx + signed publish.json + upload.
PUBLISH_URL ?= https://publisher.simplek8s.org/upload/simplek8s-nodectl
PUBLISH_FINGERPRINT ?= 33BAAC4BFB20C2327429730A9F16C69F2B9DD678
# Space- and/or comma-separated channels; all three at once:
#   make publish-nodectl PUBLISH_TAGS="dev rolling stable"
# (commas accepted too: "dev,rolling,stable").
PUBLISH_TAGS ?= dev
SPACE := $() $()
COMMA := ,
# Normalize separators (commas -> spaces, collapse runs) so every
# element becomes exactly one tag. NOTE: the join MUST inject
# backslash-escaped quotes (\"${COMMA}\"): make splices the value
# into the recipe textually and the shell would strip bare quotes,
# collapsing ["dev","rolling"] into ["dev,rolling"].
PUBLISH_TAG_LIST = $(subst ${SPACE},\"${COMMA}\",$(strip $(subst ${COMMA},${SPACE},${PUBLISH_TAGS})))

publish-nodectl: build-nodectl
	@for arch in x86-64 arm64; do \
		bin=$$(ls -t build/$(CLI).[0-9]*.$${arch} 2>/dev/null | head -1); \
		test -n "$$bin" || { echo "no artifact for $${arch} (run: make build-nodectl)" >&2; exit 1; }; \
		upx --best --lzma --no-progress -o "$${bin}.upx" "$${bin}"; \
		sum=$$(sha256sum "$${bin}.upx" | cut -d" " -f1); \
		echo -n "{\"filenames\":[\"$$(basename $${bin})\",\"$(CLI).latest.$${arch}\"],\"checksum\":\"$${sum}\",\"tags\":[\"${PUBLISH_TAG_LIST}\"]}" > "$${bin}.publish.json"; \
		gpg --quiet --local-user "$(PUBLISH_FINGERPRINT)!" --sign --detach-sign --armor --output "$${bin}.publish.json.signature" "$${bin}.publish.json"; \
		curl -F "json=@$${bin}.publish.json" -F "signature=@$${bin}.publish.json.signature" -F "release=@$${bin}.upx" "$(PUBLISH_URL)"; \
	done

test: test-go test-gocyclo test-misspell

test-go:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# Test tooling: fetched into ./bin (survives `clean`, unlike build/).
GOCYCLO  := bin/gocyclo
MISSPELL := bin/misspell

$(GOCYCLO):
	GOBIN=$(CURDIR)/bin go install github.com/fzipp/gocyclo/cmd/gocyclo@latest

$(MISSPELL):
	GOBIN=$(CURDIR)/bin go install github.com/client9/misspell/cmd/misspell@latest

test-gocyclo: $(GOCYCLO)
	$(GOCYCLO) -over 15 .

test-misspell: $(MISSPELL)
	$(MISSPELL) -error ./...

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

HELM_RELEASE ?= simplek8s-controller
HELM_NAMESPACE ?= simplek8s

deploy:
	helm upgrade --install $(HELM_RELEASE) ./chart -n $(HELM_NAMESPACE) --create-namespace --set image.tag=$(TAG)

undeploy:
	helm uninstall $(HELM_RELEASE) -n $(HELM_NAMESPACE)

clean:
	rm -rf build

mrproper:
	rm -rf bin build
