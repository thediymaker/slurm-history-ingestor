# Build stage
FROM golang:1.22-alpine AS builder

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
FROM alpine:3.19

WORKDIR /app

# Install CA certificates for HTTPS connections
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN adduser -D -u 1000 ingestor
USER ingestor

# Copy binary from builder
COPY --from=builder /app/slurm-ingestor .

# Default command
ENTRYPOINT ["./slurm-ingestor"]
