# One build for every Go binary in the module:
#   docker build -f deploy/go.Dockerfile --build-arg SERVICE=controlplane .
#   docker build -f deploy/go.Dockerfile --build-arg SERVICE=mcpbus .
# Requires cmd/${SERVICE}/main.go (build steps 4 and 5). Each binary must implement a `healthcheck`
# subcommand that exits 0 when its own HTTP listener answers; compose uses it as the container health probe.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
ARG SERVICE
RUN test -n "${SERVICE}" || (echo "build-arg SERVICE is required" >&2; exit 1)
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/${SERVICE} ./cmd/${SERVICE}

FROM alpine:3.21
ARG SERVICE
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 venture && adduser -S -u 10001 -G venture venture
WORKDIR /app
COPY --from=build /out/${SERVICE} /app/${SERVICE}
USER venture
ENV SERVICE=${SERVICE}
ENTRYPOINT ["/bin/sh", "-c", "exec /app/${SERVICE} \"$@\"", "--"]
