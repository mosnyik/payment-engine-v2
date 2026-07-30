# Build context is the repo root (docker-compose.yml's `context: .`) since
# cmd/server and every internal/ package it imports share one go.mod.
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
# GODEBUG=http2client=0: Docker Desktop's network path to proxy.golang.org
# hits repeated HTTP/2 "stream error; INTERNAL_ERROR" on large module
# downloads (go-ethereum, grpc, ...) even though the same fetch works fine
# from the host directly — forcing HTTP/1.1 for this step avoids it.
RUN GODEBUG=http2client=0 go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

# alpine, not scratch/distroless: db.Migrate needs migrations/ on disk at a
# relative path, and CA certs are needed for HTTPS calls to CoinMarketCap/
# Resend/settlement-provider APIs.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/server ./server
COPY migrations ./migrations
COPY docs ./docs
EXPOSE 3700
ENTRYPOINT ["./server"]
