#!/bin/bash

# Build TAR.GZ packages for all platforms

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Configuration
PACKAGE_NAME="wxt-agent"
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

    # The session helper ships alongside on macOS, where remote control is
    # supported. Linux is terminal-only, so there is nothing to include.
    local HELPER_SOURCE="$DIST_DIR/bin/${OS}-${ARCH}/wxt-agent-session"
    if [ "$OS" = "darwin" ] && [ -f "$HELPER_SOURCE" ]; then
        cp "$HELPER_SOURCE" "$TAR_DIR/"
        chmod +x "$TAR_DIR/wxt-agent-session"
    fi

    # Copy systemd service (Linux only)
    if [ "$OS" = "linux" ]; then
        cp systemd/wxt-agent.service "$TAR_DIR/"
    fi

    # Copy README if exists
    [ -f README.md ] && cp README.md "$TAR_DIR/"

    # Create install script
    if [ "$OS" = "linux" ]; then
        cat > "$TAR_DIR/install.sh" << 'EOF'
#!/bin/bash
set -e

echo "Installing wxt-agent..."

# Install binary
sudo cp wxt-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/wxt-agent

# Create directories
sudo mkdir -p /etc/vsay
sudo mkdir -p /var/log/vsay
sudo chmod 755 /etc/vsay
sudo chmod 755 /var/log/vsay

# Install systemd service
sudo cp wxt-agent.service /etc/systemd/system/
sudo systemctl daemon-reload

echo "✓ Installation complete!"
echo ""
echo "Next steps:"
echo "1. Configure: sudo wxt-agent configure --token <token> --tenant <tenant> --org <org> --project <project> --user <email> --linux-user <username> --host <host> --allow-sudo"
echo "2. Start: sudo systemctl start wxt-agent"
EOF
    elif [ "$OS" = "darwin" ]; then
        cat > "$TAR_DIR/install.sh" << 'EOF'
#!/bin/bash
set -e

echo "Installing wxt-agent..."

# Install binary
sudo cp wxt-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/wxt-agent

# The agent looks for the session helper NEXT TO ITSELF, so both binaries have
# to land in the same directory. Without it remote control cannot start.
if [ -f wxt-agent-session ]; then
    sudo cp wxt-agent-session /usr/local/bin/
    sudo chmod +x /usr/local/bin/wxt-agent-session
    echo "✓ Session helper installed (needed for remote control)"
fi

echo "✓ Installation complete!"
echo ""
echo "To configure: wxt-agent configure --help"
echo ""
echo "For remote control, grant wxt-agent-session both Screen Recording and"
echo "Accessibility under System Settings > Privacy & Security. macOS will not"
echo "grant these silently."
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
create_tarball "linux" "amd64" "wxt-agent"
create_tarball "linux" "arm64" "wxt-agent"

# macOS
create_tarball "darwin" "amd64" "wxt-agent"
create_tarball "darwin" "arm64" "wxt-agent"

echo ""
echo -e "${GREEN}✓ TAR.GZ packages built:${NC}"
ls -la "$BUILD_DIR"/*.tar.gz 2>/dev/null || echo "  (none)"
