#!/bin/bash

# Build DMG packages for macOS

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
BUILD_DIR="$DIST_DIR/dmg"

echo -e "${GREEN}Building DMG packages${NC}"
echo "Package: $PACKAGE_NAME"
echo "Version: $VERSION"
echo ""

mkdir -p "$BUILD_DIR"

# Function to create DMG
create_dmg() {
    local ARCH=$1
    local BIN_SOURCE="$DIST_DIR/bin/darwin-${ARCH}/wxt-agent"
    # The session helper is the half of remote control that runs inside the
    # user's GUI session. Without it the agent still works, but remote control
    # fails at the moment an admin tries to use it.
    local HELPER_SOURCE="$DIST_DIR/bin/darwin-${ARCH}/wxt-agent-session"

    if [ ! -f "$BIN_SOURCE" ]; then
        echo -e "${YELLOW}  Skipping darwin-$ARCH (binary not found)${NC}"
        return
    fi

    local DMG_DIR="$BUILD_DIR/dmg-${ARCH}"
    local DMG_NAME="${PACKAGE_NAME}-${VERSION}-macos-${ARCH}.dmg"
    local VOL_NAME="Vsay Agent ${VERSION}"

    rm -rf "$DMG_DIR"
    mkdir -p "$DMG_DIR/Vsay Agent"

    # Copy binaries
    cp "$BIN_SOURCE" "$DMG_DIR/Vsay Agent/wxt-agent"
    chmod +x "$DMG_DIR/Vsay Agent/wxt-agent"
    if [ -f "$HELPER_SOURCE" ]; then
        cp "$HELPER_SOURCE" "$DMG_DIR/Vsay Agent/wxt-agent-session"
        chmod +x "$DMG_DIR/Vsay Agent/wxt-agent-session"
    else
        echo -e "${YELLOW}  Warning: session helper missing for darwin-$ARCH — remote control will not work${NC}"
    fi

    # Create install script
    cat > "$DMG_DIR/Vsay Agent/Install.command" << 'EOF'
#!/bin/bash
cd "$(dirname "$0")"
echo "Installing wxt-agent..."
sudo cp wxt-agent /usr/local/bin/
sudo chmod +x /usr/local/bin/wxt-agent

# The agent looks for the session helper NEXT TO ITSELF, so both have to land
# in the same directory.
if [ -f wxt-agent-session ]; then
    sudo cp wxt-agent-session /usr/local/bin/
    sudo chmod +x /usr/local/bin/wxt-agent-session
    echo "✓ Session helper installed (needed for remote control)"
fi

echo ""
echo "✓ Installation complete!"
echo ""
echo "To configure: wxt-agent configure --help"
echo ""
echo "For remote control, macOS will ask you to allow wxt-agent-session under"
echo "System Settings > Privacy & Security > Screen Recording, and again under"
echo "Accessibility. Both are required."
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
   Open Terminal and run: sudo cp wxt-agent /usr/local/bin/

Contents:
  wxt-agent           the agent daemon
  wxt-agent-session   the session helper, required for remote control.
                      It MUST sit in the same directory as wxt-agent.

Usage:
  wxt-agent configure --help
  wxt-agent start --config /etc/vsay/agent.yaml

Remote control permissions (macOS only):
  Grant wxt-agent-session both Screen Recording and Accessibility under
  System Settings > Privacy & Security. Remote control cannot work without
  them, and macOS will not grant them silently.

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
