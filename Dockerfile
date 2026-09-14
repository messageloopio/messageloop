# syntax=docker/dockerfile:1
#
# Multi-stage production image for MessageLoop. The runtime default config is
# configs/docker.yaml; every key in it can be overridden with MESSAGELOOP_*
# environment variables (docs/dokploy.md).

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

FROM golang:${GO_VERSION} AS builder
WORKDIR /src

# Copy the module manifests first so dependency download stays cached across
# source changes. The ./shared module is pulled in through the replace
# directive in go.mod.
COPY go.mod go.sum ./
COPY shared/go.mod shared/go.sum ./shared/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/messageloop ./cmd/server

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 messageloop \
 && adduser -S -D -H -u 10001 -G messageloop messageloop

COPY --from=builder /out/messageloop /usr/local/bin/messageloop
COPY configs/docker.yaml /etc/messageloop/docker.yaml

USER messageloop

# 8080 health/metrics HTTP, 9080 WebSocket, 9090 client gRPC, 9091 admin gRPC
EXPOSE 8080 9080 9090 9091

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/health"]

# Override the config path with `command: ["--config", "/your.yaml"]`.
ENTRYPOINT ["/usr/local/bin/messageloop"]
CMD ["--config", "/etc/messageloop/docker.yaml"]
