# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway cmd/gateway/main.go

# Runtime stage
FROM alpine:3.21

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata docker-cli docker-compose git

# Install nodejs and npm for frontend builds via the admin panel
# RUN apk add --no-cache nodejs npm

# Copy the binary from builder
COPY --from=builder /gateway /usr/local/bin/gateway

# Copy migrations for reference
COPY --from=builder /app/migrations ./migrations

# Create non-root user and add to docker group (for admin update panel)
ARG DOCKER_GID=999
RUN addgroup -g ${DOCKER_GID} docker && \
    adduser -D -g '' appuser && \
    adduser appuser docker
USER appuser

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Run the application
CMD ["/usr/local/bin/gateway"]
