FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/bucketmux ./cmd/bucketmux

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -h /app switcher
WORKDIR /app
COPY --from=build /out/bucketmux /usr/local/bin/bucketmux
COPY config.example.yaml /config/config.yaml
RUN mkdir -p /data && chown -R switcher:switcher /data /config
USER switcher
ENV CONFIG_PATH=/config/config.yaml \
    DB_PATH=/data/switcher.db \
    DATA_DIR=/data \
    ADMIN_ENABLED=false
VOLUME ["/data", "/config"]
EXPOSE 8080
ENTRYPOINT ["bucketmux"]
