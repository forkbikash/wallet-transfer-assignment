# --- Build stage ---
FROM golang:1.24-alpine AS builder

WORKDIR /src

# Copy module files first for better build caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree.
COPY . .

# Build a static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- Runtime stage ---
FROM alpine:3.19

# Non-root user for safety.
RUN addgroup -S app && adduser -S app -G app

# CA certs needed for outbound TLS (e.g. future webhooks).
RUN apk add --no-cache ca-certificates

COPY --from=builder /out/server /app/server
COPY migrations /app/migrations

USER app
WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["/app/server"]
