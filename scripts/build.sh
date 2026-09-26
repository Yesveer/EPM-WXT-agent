#!/bin/bash

# Build script for vsay-agent
# Builds the agent for multiple platforms

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Build directory
BUILD_DIR="dist"
BINARY_NAME="vsay-agent"

# Platforms to build for
PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "linux/armv7"
)

# Version (from git tag or default)
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# Build flags
LDFLAGS="-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$BUILD_TIME"
LDFLAGS="$LDFLAGS -extldflags '-static'"

echo -e "${GREEN}Building vsay-agent${NC}"
echo "Version: $VERSION"
echo "Commit: $COMMIT"
echo "Build Time: $BUILD_TIME"
echo ""

# Create build directory
mkdir -p $BUILD_DIR

# Build for each platform
for PLATFORM in "${PLATFORMS[@]}"; do
    GOOS=${PLATFORM%/*}
    GOARCH=${PLATFORM#*/}
    
    export GOOS
    export GOARCH
    export GOARM=""

    if [ "$GOARCH" == "armv7" ]; then
        export GOARCH="arm"
        export GOARM="7"
    fi
    
    echo -e "${YELLOW}Building for $GOOS/$GOARCH${GOARM:+v$GOARM}...${NC}"
    
    OUTPUT_NAME="$BUILD_DIR/${BINARY_NAME}-${GOOS}-${GOARCH}${GOARM:+v$GOARM}"
    
    if [ "$GOOS" = "windows" ]; then
        OUTPUT_NAME="${OUTPUT_NAME}.exe"
    fi
    
    CGO_ENABLED=0 go build \
        -ldflags "$LDFLAGS" \
        -o "$OUTPUT_NAME" \
        ./cmd/agent
    
    # Create checksum
    if command -v sha256sum &> /dev/null; then
        sha256sum "$OUTPUT_NAME" > "${OUTPUT_NAME}.sha256"
    elif command -v shasum &> /dev/null; then
        shasum -a 256 "$OUTPUT_NAME" > "${OUTPUT_NAME}.sha256"
    fi
    
    echo -e "${GREEN}✓ Built: $OUTPUT_NAME${NC}"
done

echo ""
echo -e "${GREEN}Build complete!${NC}"
echo "Binaries are in: $BUILD_DIR/"
ls -lh $BUILD_DIR/
