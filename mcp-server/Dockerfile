# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mcp-server .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app
USER app
WORKDIR /app
COPY --from=build /out/mcp-server /app/mcp-server

ENV LISTEN_ADDR=":9000" \
    ENDPOINT_PATH="/mcp"
EXPOSE 9000

HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
    CMD wget -qO- http://127.0.0.1:9000/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/mcp-server"]
