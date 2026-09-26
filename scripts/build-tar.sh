#!/bin/bash

# Build TAR.GZ packages for all platforms

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Configuration
PACKAGE_NAME="vsay-agent"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "1.0.0")}

# Fix version if it doesn't start with a digit
if [[ ! "$VERSION" =~ ^[0-9] ]]; then
    VERSION="0.0.0-${VERSION}"
fi

DIST_DIR="dist"
BUILD_DIR="$DIST_DIR/tar"

echo -e "${GREEN}Building TAR.GZ packages${NC}"
echo "Package: $PACKAGE_NAME"
echo "Version: $VERSION"
echo ""

mkdir -p "$BUILD_DIR"

# Function to create tarball
create_tarball() {
    local OS=$1
    local ARCH=$2
    local BIN_NAME=$3
    local BIN_SOURCE="$DIST_DIR/bin/${OS}-${ARCH}/${BIN_NAME}"

    if [ ! -f "$BIN_SOURCE" ]; then
        echo -e "${YELLOW}  Skipping $OS-$ARCH (binary not found)${NC}"
        return
    fi

    local TAR_DIR="$BUILD_DIR/${PACKAGE_NAME}-${VERSION}-${OS}-${ARCH}"
    rm -rf "$TAR_DIR"
    mkdir -p "$TAR_DIR"

    # Copy binary
    cp "$BIN_SOURCE" "$TAR_DIR/"
    chmod +x "$TAR_DIR/$BIN_NAME"

    # Copy systemd service (Linux only)
    if [ "$OS" = "linux" ]; then
        cp systemd/vsay-agent.service "$TAR_DIR/"
    fi

    # Copy README if exists
    [ -f README.md ] && cp README.md "$TAR_DIR/"

    # Create install script
    if [ "$OS" = "linux" ]; then
        cat > "$TAR_DIR/install.sh" << 'EOF'
#!/bin/bash
set -e

echo "Installing vsay-agent..."

# Install binary
sudo cp vsay-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/vsay-agent

# Create directories
sudo mkdir -p /etc/vsay
sudo mkdir -p /var/log/vsay
sudo chmod 755 /etc/vsay
sudo chmod 755 /var/log/vsay

# Install systemd service
sudo cp vsay-agent.service /etc/systemd/system/
sudo systemctl daemon-reload

echo "✓ Installation complete!"
echo ""
echo "Next steps:"
echo "1. Configure: sudo vsay-agent configure --token <token> --tenant <tenant> --org <org> --project <project> --user <email> --linux-user <username> --host <host> --allow-sudo"
echo "2. Start: sudo systemctl start vsay-agent"
EOF
    elif [ "$OS" = "darwin" ]; then
        cat > "$TAR_DIR/install.sh" << 'EOF'
#!/bin/bash
set -e

echo "Installing vsay-agent..."

# Install binary
sudo cp vsay-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/vsay-agent

echo "✓ Installation complete!"
echo ""
echo "To configure: vsay-agent configure --help"
EOF
    fi

    chmod +x "$TAR_DIR/install.sh" 2>/dev/null || true

    # Create tarball
    cd "$BUILD_DIR"
    tar -czf "${PACKAGE_NAME}-${VERSION}-${OS}-${ARCH}.tar.gz" "$(basename $TAR_DIR)"
    rm -rf "$(basename $TAR_DIR)"

    # Create checksum
    sha256sum "${PACKAGE_NAME}-${VERSION}-${OS}-${ARCH}.tar.gz" > "${PACKAGE_NAME}-${VERSION}-${OS}-${ARCH}.tar.gz.sha256"
    cd - > /dev/null

    echo -e "  ${GREEN}✓${NC} ${OS}-${ARCH}"
}

# Build for all platforms
echo "Creating tarballs..."

# Linux
create_tarball "linux" "amd64" "vsay-agent"
create_tarball "linux" "arm64" "vsay-agent"

# macOS
create_tarball "darwin" "amd64" "vsay-agent"
create_tarball "darwin" "arm64" "vsay-agent"

echo ""
echo -e "${GREEN}✓ TAR.GZ packages built:${NC}"
ls -la "$BUILD_DIR"/*.tar.gz 2>/dev/null || echo "  (none)"
