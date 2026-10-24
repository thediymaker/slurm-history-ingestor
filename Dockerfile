# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o slurm-ingestor cmd/ingest/main.go

# Runtime stage
FROM alpine:3.24

WORKDIR /app

# Install CA certificates for HTTPS connections
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -S -g 1000 ingestor && adduser -S -D -H -u 1000 -G ingestor ingestor

# Copy binary from builder (root-owned, so the runtime user cannot modify it)
COPY --from=builder /app/slurm-ingestor .

# Numeric UID:GID so Kubernetes runAsNonRoot can verify the user without
# resolving names from /etc/passwd.
USER 1000:1000

# Default command
ENTRYPOINT ["./slurm-ingestor"]
