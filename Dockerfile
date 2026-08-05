# syntax=docker/dockerfile:1
# The syntax directive guarantees a frontend that understands the cache mounts below.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Set by buildx for each target platform; the defaults only apply to a plain
# `docker build` without buildx.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /controller /controller
ENTRYPOINT ["/controller"]
