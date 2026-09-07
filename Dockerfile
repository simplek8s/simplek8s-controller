FROM golang:1.27 AS build
ARG VERSION=dev
ARG COMMIT=none
ARG BUILT=unknown
WORKDIR /src
COPY go.mod go.sum* ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build \
    -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.builtAt=${BUILT}" \
    -o /out/simplek8s-controller ./cmd/simplek8s-controller

# The base image must contain nsenter AND reboot (util-linux): alpine,
# NOT distroless (PLAN 3.12).
FROM alpine:3.20
RUN apk add --no-cache util-linux
COPY --from=build /out/simplek8s-controller /usr/local/bin/simplek8s-controller
# Embedded GPG keyring (distro public key) — the default for release
# index verification; an optional Secret overrides it (PLAN-M2 3.6/3.14).
# ADD reads the smudged (real) bytes from the build context, not the LFS
# pointer.
ADD keys/simplek8s-pubring.gpg /etc/simplek8s/pubring.gpg
RUN chmod 0444 /etc/simplek8s/pubring.gpg
ENTRYPOINT ["/usr/local/bin/simplek8s-controller"]
