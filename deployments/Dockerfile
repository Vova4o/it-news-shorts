# syntax=docker/dockerfile:1

FROM golang:1.27.4-alpine3.24 AS builder
WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cli ./cmd/cli

FROM alpine:3.24.4 AS runtime-base
RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -u 10001 appuser
USER appuser
WORKDIR /app

FROM runtime-base AS api
COPY --from=builder /out/api /usr/local/bin/api
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/api"]

FROM runtime-base AS worker
COPY --from=builder /out/worker /usr/local/bin/worker
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/worker"]

FROM runtime-base AS cli
COPY --from=builder /out/cli /usr/local/bin/cli
ENTRYPOINT ["/usr/local/bin/cli"]
