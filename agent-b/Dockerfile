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
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent-b .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app
USER app
WORKDIR /app
COPY --from=build /out/agent-b /app/agent-b

ENV AGENT_B_LISTEN_ADDR=":8080" \
    AGENT_B_ENDPOINT_PATH="/mcp"
EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/agent-b"]
