# Build stage — always runs on the host's native platform to avoid emulation.
# The binary is cross-compiled by Go for the target platform.
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Re-declare after FROM so the build args are in scope for this stage.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /k8s-service-proxy ./cmd/proxy

# Runtime stage
FROM alpine:3.23

RUN apk add --no-cache ca-certificates

COPY --from=builder /k8s-service-proxy /usr/local/bin/k8s-service-proxy

ENV HTTP_LISTEN=:8080 \
    DNS_LISTEN=:53 \
    SOCKS5_LISTEN=:1080

# DNS port
EXPOSE 53/udp
EXPOSE 53/tcp

# SOCKS5 proxy port
EXPOSE 1080/tcp

# Control server port
EXPOSE 8080/tcp

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/readyz || exit 1

ENTRYPOINT ["/usr/local/bin/k8s-service-proxy"]
