#!/bin/bash

# Generate Go code from protobuf definitions

set -e

# Add Go bin to PATH
export PATH="$PATH:$(go env GOPATH)/bin:$HOME/go/bin"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}Generating protobuf code...${NC}"

# Check if protoc is installed
if ! command -v protoc &> /dev/null; then
    echo -e "${YELLOW}protoc not found. Installing...${NC}"
    echo "Please install protoc: https://grpc.io/docs/protoc-installation/"
    exit 1
fi

# Check if protoc-gen-go is installed
if ! command -v protoc-gen-go &> /dev/null; then
    echo -e "${YELLOW}protoc-gen-go not found. Installing...${NC}"
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
fi

# Check if protoc-gen-go-grpc is installed
if ! command -v protoc-gen-go-grpc &> /dev/null; then
    echo -e "${YELLOW}protoc-gen-go-grpc not found. Installing...${NC}"
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
fi

# Generate common types
echo "Generating common types..."
protoc \
    --go_out=. \
    --go_opt=paths=source_relative \
    --go-grpc_out=. \
    --go-grpc_opt=paths=source_relative \
    --proto_path=proto \
    proto/common/v1/types.proto

# Generate agent service
echo "Generating agent service..."
protoc \
    --go_out=. \
    --go_opt=paths=source_relative \
    --go-grpc_out=. \
    --go-grpc_opt=paths=source_relative \
    --proto_path=proto \
    proto/agent/v1/agent.proto

echo -e "${GREEN}✓ Protobuf code generated successfully${NC}"
