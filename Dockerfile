# --- Build stage ---
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Copy module files first for better build caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree. `.dockerignore` is the safety boundary here —
# .env*, .git, secrets, build artifacts, and host-only files (Makefile, *.md,
# docker-compose.yml, etc.) are excluded from the build context.
COPY . .

# One static binary for all roles; --mode selects the role at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/wallet ./cmd/wallet

# --- Runtime stage ---
FROM alpine:3.19

# Non-root user for safety; CA certs for outbound TLS.
RUN addgroup -S app && adduser -S app -G app && \
    apk add --no-cache ca-certificates

COPY --from=builder /out/wallet /app/wallet
COPY migration /app/migration

USER app
WORKDIR /app

EXPOSE 8080

# Default mode is "all"; docker-compose overrides per service (--mode=gateway, etc.).
ENTRYPOINT ["/app/wallet"]
