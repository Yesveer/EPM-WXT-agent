# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git make

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o wxt-agent ./cmd/agent

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy binary from builder
COPY --from=builder /app/wxt-agent .

# Copy systemd service file
COPY systemd/wxt-agent.service /etc/systemd/system/

# Run
ENTRYPOINT ["./wxt-agent"]
CMD ["start", "--config", "/etc/vsay/agent.yaml"]
