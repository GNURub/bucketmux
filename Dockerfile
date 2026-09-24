# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32

FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -tags=musl -trimpath -ldflags="-s -w" -o /out/bucketmux ./cmd/bucketmux \
    && mkdir /out/data \
    && chown 10001:10001 /out/data

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates libgcc

WORKDIR /app
COPY --from=build --chown=10001:10001 /out/data /data
COPY --from=build --chown=0:0 --chmod=0555 /out/bucketmux /usr/local/bin/bucketmux

USER 10001:10001
ENV DB_PATH=/data/switcher.db \
    DATA_DIR=/data \
    TURSO_GO_CACHE_DIR=/data/.turso-cache \
    ADMIN_ENABLED=false

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/bucketmux", "healthcheck"]

STOPSIGNAL SIGTERM
ENTRYPOINT ["bucketmux"]
