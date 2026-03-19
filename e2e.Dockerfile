# Build stage — compile the test binary.
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.work go.work.sum go.mod go.sum ./
COPY tests/e2e/go.mod tests/e2e/go.sum ./tests/e2e/
RUN go mod download all

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go test -c -o /e2e-tests ./tests/e2e

# Runtime stage — minimal image with the test binary + Docker CLI (for kind).
FROM alpine:3.23

RUN apk add --no-cache ca-certificates curl docker-cli

COPY --from=builder /e2e-tests /e2e-tests

ENTRYPOINT ["/e2e-tests", "-test.v", "-test.timeout=90m"]
