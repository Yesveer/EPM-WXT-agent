#!/bin/bash

# Build DMG packages for macOS

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
BUILD_DIR="$DIST_DIR/dmg"

echo -e "${GREEN}Building DMG packages${NC}"
echo "Package: $PACKAGE_NAME"
echo "Version: $VERSION"
echo ""

mkdir -p "$BUILD_DIR"

# Function to create DMG
create_dmg() {
    local ARCH=$1
    local BIN_SOURCE="$DIST_DIR/bin/darwin-${ARCH}/vsay-agent"

    if [ ! -f "$BIN_SOURCE" ]; then
        echo -e "${YELLOW}  Skipping darwin-$ARCH (binary not found)${NC}"
        return
    fi

    local DMG_DIR="$BUILD_DIR/dmg-${ARCH}"
    local DMG_NAME="${PACKAGE_NAME}-${VERSION}-macos-${ARCH}.dmg"
    local VOL_NAME="Vsay Agent ${VERSION}"

    rm -rf "$DMG_DIR"
    mkdir -p "$DMG_DIR/Vsay Agent"

    # Copy binary
    cp "$BIN_SOURCE" "$DMG_DIR/Vsay Agent/vsay-agent"
    chmod +x "$DMG_DIR/Vsay Agent/vsay-agent"

    # Create install script
    cat > "$DMG_DIR/Vsay Agent/Install.command" << 'EOF'
#!/bin/bash
cd "$(dirname "$0")"
echo "Installing vsay-agent..."
sudo cp vsay-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/vsay-agent
echo ""
echo "✓ Installation complete!"
echo ""
echo "To configure: vsay-agent configure --help"
echo ""
read -p "Press Enter to close..."
EOF
    chmod +x "$DMG_DIR/Vsay Agent/Install.command"

    # Create README
    cat > "$DMG_DIR/Vsay Agent/README.txt" << EOF
Vsay Agent ${VERSION}

Installation:
1. Double-click "Install.command" to install
   OR
   Open Terminal and run: sudo cp vsay-agent /usr/local/bin/

Usage:
  vsay-agent configure --help
  vsay-agent start --config /etc/vsay/agent.yaml

For more information, visit: https://vsay.io
EOF

    # Check if we can create DMG (macOS only)
    if command -v hdiutil >/dev/null 2>&1; then
        echo "  Creating DMG for $ARCH..."
        hdiutil create -volname "$VOL_NAME" \
            -srcfolder "$DMG_DIR/Vsay Agent" \
            -ov -format UDZO \
            "$BUILD_DIR/$DMG_NAME"

        # Create checksum
        cd "$BUILD_DIR"
        shasum -a 256 "$DMG_NAME" > "${DMG_NAME}.sha256"
        cd - > /dev/null

        echo -e "  ${GREEN}✓${NC} darwin-${ARCH}: $DMG_NAME"
    else
        # Fallback: Create ZIP instead
        echo -e "${YELLOW}  hdiutil not available, creating ZIP instead...${NC}"
        local ZIP_NAME="${PACKAGE_NAME}-${VERSION}-macos-${ARCH}.zip"

        cd "$DMG_DIR"
        zip -r "../$ZIP_NAME" "Vsay Agent"
        cd - > /dev/null

        mv "$DMG_DIR/../$ZIP_NAME" "$BUILD_DIR/"

        # Create checksum
        cd "$BUILD_DIR"
        sha256sum "$ZIP_NAME" > "${ZIP_NAME}.sha256" 2>/dev/null || shasum -a 256 "$ZIP_NAME" > "${ZIP_NAME}.sha256"
        cd - > /dev/null

        echo -e "  ${GREEN}✓${NC} darwin-${ARCH}: $ZIP_NAME"
    fi

    # Cleanup
    rm -rf "$DMG_DIR"
}

# Build for both architectures
echo "Creating macOS packages..."

create_dmg "amd64"
create_dmg "arm64"

echo ""
echo -e "${GREEN}✓ macOS packages built:${NC}"
ls -la "$BUILD_DIR"/*.dmg "$BUILD_DIR"/*.zip 2>/dev/null || echo "  (none)"
