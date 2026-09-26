#!/bin/bash

# Build DEB package for Debian/Ubuntu using nfpm (preferred) or dpkg-deb

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PACKAGE_NAME="wxt-agent"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "1.0.0")}

# Fix version if it doesn't start with a digit
if [[ ! "$VERSION" =~ ^[0-9] ]]; then
    VERSION="1.0.0-${VERSION}"
fi

# Architecture (can be overridden)
ARCH=${ARCH:-amd64}

# Map Go arch to Debian arch
case $ARCH in
    amd64) DEB_ARCH="amd64" ;;
    arm64) DEB_ARCH="arm64" ;;
    *)     DEB_ARCH="$ARCH" ;;
esac

# Directories
DIST_DIR="$PROJECT_ROOT/dist"
BUILD_DIR="$DIST_DIR/deb"
BIN_SOURCE="$DIST_DIR/bin/linux-${ARCH}/wxt-agent"

echo -e "${GREEN}Building DEB package${NC}"
echo "Package: $PACKAGE_NAME"
echo "Version: $VERSION"
echo "Architecture: $DEB_ARCH"
echo ""

# Check if binary exists, if not build it
if [ ! -f "$BIN_SOURCE" ]; then
    echo -e "${YELLOW}Binary not found, building...${NC}"
    mkdir -p "$DIST_DIR/bin/linux-${ARCH}"
    cd "$PROJECT_ROOT"
    CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build \
        -ldflags "-s -w -X main.version=${VERSION} -extldflags '-static'" \
        -o "$BIN_SOURCE" \
        ./cmd/agent
fi

mkdir -p "$BUILD_DIR"

# Use nfpm if available (preferred method)
if command -v nfpm &> /dev/null; then
    echo -e "${GREEN}Using nfpm for DEB package...${NC}"

    # Create nfpm config
    cat > "$PROJECT_ROOT/nfpm-deb-${ARCH}.yaml" << EOF
name: ${PACKAGE_NAME}
arch: ${DEB_ARCH}
platform: linux
version: "${VERSION}"
maintainer: VSay Team <support@vsay.in>
description: |
  VSay Agent - Remote Terminal Access for Linux Machines.
  Lightweight agent that enables secure remote terminal access
  to Linux machines with RBAC and audit logging.
vendor: VSay
homepage: https://vsay.in
license: Proprietary

depends:
  - systemd

contents:
  - src: ${BIN_SOURCE}
    dst: /usr/local/bin/wxt-agent
    file_info:
      mode: 0755

  - src: ${PROJECT_ROOT}/systemd/wxt-agent.service
    dst: /etc/systemd/system/wxt-agent.service
    file_info:
      mode: 0644

  - dst: /etc/vsay
    type: dir
    file_info:
      mode: 0755

  - dst: /var/log/vsay
    type: dir
    file_info:
      mode: 0755

scripts:
  postinstall: ${PROJECT_ROOT}/scripts/pkg/postinstall.sh
  preremove: ${PROJECT_ROOT}/scripts/pkg/preremove.sh
  postremove: ${PROJECT_ROOT}/scripts/pkg/postremove.sh
EOF

    # Build DEB
    nfpm package \
        --config "$PROJECT_ROOT/nfpm-deb-${ARCH}.yaml" \
        --packager deb \
        --target "$BUILD_DIR/${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb"

    # Cleanup config
    rm -f "$PROJECT_ROOT/nfpm-deb-${ARCH}.yaml"

else
    # Fallback to dpkg-deb
    echo -e "${YELLOW}nfpm not found, using dpkg-deb...${NC}"

    DEB_DIR="$BUILD_DIR/${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}"
    rm -rf "$DEB_DIR"
    mkdir -p "$DEB_DIR"

    # Create directory structure
    mkdir -p "$DEB_DIR/usr/local/bin"
    mkdir -p "$DEB_DIR/etc/vsay"
    mkdir -p "$DEB_DIR/var/log/vsay"
    mkdir -p "$DEB_DIR/etc/systemd/system"
    mkdir -p "$DEB_DIR/DEBIAN"

    # Copy binary
    cp "$BIN_SOURCE" "$DEB_DIR/usr/local/bin/wxt-agent"
    chmod 755 "$DEB_DIR/usr/local/bin/wxt-agent"

    # Copy systemd service
    cp "$PROJECT_ROOT/systemd/wxt-agent.service" "$DEB_DIR/etc/systemd/system/"

    # Create control file
    cat > "$DEB_DIR/DEBIAN/control" << EOF
Package: $PACKAGE_NAME
Version: $VERSION
Architecture: $DEB_ARCH
Maintainer: VSay Team <support@vsay.in>
Depends: systemd
Description: VSay Agent - Remote Terminal Access for Linux Machines
 Lightweight agent that enables secure remote terminal access
 to Linux machines with RBAC and audit logging.
Homepage: https://vsay.in
EOF

    # Copy scripts
    cp "$PROJECT_ROOT/scripts/pkg/postinstall.sh" "$DEB_DIR/DEBIAN/postinst"
    cp "$PROJECT_ROOT/scripts/pkg/preremove.sh" "$DEB_DIR/DEBIAN/prerm"
    cp "$PROJECT_ROOT/scripts/pkg/postremove.sh" "$DEB_DIR/DEBIAN/postrm"
    chmod 755 "$DEB_DIR/DEBIAN/postinst" "$DEB_DIR/DEBIAN/prerm" "$DEB_DIR/DEBIAN/postrm"

    # Build DEB package
    if command -v dpkg-deb >/dev/null 2>&1; then
        dpkg-deb --root-owner-group --build "$DEB_DIR" "$BUILD_DIR/${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb"
    elif command -v docker >/dev/null 2>&1; then
        echo -e "${YELLOW}dpkg-deb not found. Using Docker...${NC}"
        docker run --rm -v "$PROJECT_ROOT:/src" -w /src debian:stable-slim \
            bash -c "dpkg-deb --root-owner-group --build '$DEB_DIR' '$BUILD_DIR/${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb'"
    else
        echo -e "${RED}Error: Cannot build DEB package. Install nfpm: brew install goreleaser/tap/nfpm${NC}"
        exit 1
    fi

    # Cleanup
    rm -rf "$DEB_DIR"
fi

echo -e "${GREEN}✓ DEB package built: $BUILD_DIR/${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb${NC}"

# Create checksum
cd "$BUILD_DIR"
sha256sum "${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb" > "${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb.sha256" 2>/dev/null || \
    shasum -a 256 "${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb" > "${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb.sha256"
cd - > /dev/null

echo "✓ Checksum: $BUILD_DIR/${PACKAGE_NAME}_${VERSION}_${DEB_ARCH}.deb.sha256"
