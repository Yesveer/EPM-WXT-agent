#!/bin/bash

# Build RPM package for RHEL/CentOS/Rocky Linux/Fedora using nfpm

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

# Fix version - RPM doesn't like hyphens in version
VERSION=$(echo "$VERSION" | sed 's/-/./g')
if [[ ! "$VERSION" =~ ^[0-9] ]]; then
    VERSION="1.0.0.${VERSION}"
fi

# Architecture
ARCH=${ARCH:-amd64}

# Map Go arch to RPM arch
case $ARCH in
    amd64) RPM_ARCH="x86_64" ;;
    arm64) RPM_ARCH="aarch64" ;;
    *)     RPM_ARCH="$ARCH" ;;
esac

# Directories
DIST_DIR="$PROJECT_ROOT/dist"
BUILD_DIR="$DIST_DIR/rpm"
BIN_SOURCE="$DIST_DIR/bin/linux-${ARCH}/wxt-agent"

echo -e "${GREEN}Building RPM package${NC}"
echo "Package: $PACKAGE_NAME"
echo "Version: $VERSION"
echo "Architecture: $RPM_ARCH"
echo ""

# Check if binary exists
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
    echo -e "${GREEN}Using nfpm for RPM package...${NC}"

    # Create nfpm config
    cat > "$PROJECT_ROOT/nfpm-rpm-${ARCH}.yaml" << EOF
name: ${PACKAGE_NAME}
arch: ${RPM_ARCH}
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

    # Build RPM
    nfpm package \
        --config "$PROJECT_ROOT/nfpm-rpm-${ARCH}.yaml" \
        --packager rpm \
        --target "$BUILD_DIR/${PACKAGE_NAME}-${VERSION}.${RPM_ARCH}.rpm"

    # Cleanup config
    rm -f "$PROJECT_ROOT/nfpm-rpm-${ARCH}.yaml"

    echo -e "${GREEN}✓ RPM package built: $BUILD_DIR/${PACKAGE_NAME}-${VERSION}.${RPM_ARCH}.rpm${NC}"

else
    echo -e "${YELLOW}nfpm not found. Install with: brew install goreleaser/tap/nfpm${NC}"
    echo -e "${YELLOW}Creating tarball instead...${NC}"

    # Fallback: Create a simple tar.gz with install script
    FALLBACK_DIR="$BUILD_DIR/${PACKAGE_NAME}-${VERSION}-${RPM_ARCH}"
    mkdir -p "$FALLBACK_DIR"
    cp "$BIN_SOURCE" "$FALLBACK_DIR/wxt-agent"
    cp "$PROJECT_ROOT/systemd/wxt-agent.service" "$FALLBACK_DIR/"

    cat > "$FALLBACK_DIR/install.sh" << 'INSTALL_EOF'
#!/bin/bash
set -e
echo "Installing wxt-agent..."
sudo cp wxt-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/wxt-agent
sudo mkdir -p /etc/vsay /var/log/vsay
sudo cp wxt-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
echo "✓ Installed! Configure with: sudo wxt-agent configure --help"
INSTALL_EOF
    chmod +x "$FALLBACK_DIR/install.sh"

    cd "$BUILD_DIR"
    tar czf "${PACKAGE_NAME}-${VERSION}-${RPM_ARCH}.tar.gz" "${PACKAGE_NAME}-${VERSION}-${RPM_ARCH}"
    rm -rf "${PACKAGE_NAME}-${VERSION}-${RPM_ARCH}"
    cd - > /dev/null

    echo -e "${GREEN}✓ TAR.GZ package built: $BUILD_DIR/${PACKAGE_NAME}-${VERSION}-${RPM_ARCH}.tar.gz${NC}"
    exit 0
fi

# Create checksum
cd "$BUILD_DIR"
RPM_FILE="${PACKAGE_NAME}-${VERSION}.${RPM_ARCH}.rpm"
sha256sum "$RPM_FILE" > "${RPM_FILE}.sha256" 2>/dev/null || \
    shasum -a 256 "$RPM_FILE" > "${RPM_FILE}.sha256"
cd - > /dev/null

echo "✓ Checksum: $BUILD_DIR/${RPM_FILE}.sha256"
