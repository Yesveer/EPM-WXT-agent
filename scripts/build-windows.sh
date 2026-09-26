#!/bin/bash

# Build Windows packages (EXE installer or ZIP fallback)

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

DIST_DIR="$PROJECT_ROOT/dist"
BUILD_DIR="$DIST_DIR/windows"

echo -e "${GREEN}Building Windows packages${NC}"
echo "Package: $PACKAGE_NAME"
echo "Version: $VERSION"
echo ""

mkdir -p "$BUILD_DIR"

# Function to create Windows package
create_windows_package() {
    local ARCH=$1
    local BIN_SOURCE="$DIST_DIR/bin/windows-${ARCH}/wxt-agent.exe"
    # The session helper runs inside the logged-in user's desktop; a Windows
    # service cannot capture the screen or show a dialog from session 0.
    # Remote control is dead without it.
    local HELPER_SOURCE="$DIST_DIR/bin/windows-${ARCH}/wxt-agent-session.exe"

    if [ ! -f "$BIN_SOURCE" ]; then
        echo -e "${YELLOW}  Building binary for windows-$ARCH...${NC}"
        mkdir -p "$DIST_DIR/bin/windows-${ARCH}"
        cd "$PROJECT_ROOT"
        CGO_ENABLED=0 GOOS=windows GOARCH=$ARCH go build \
            -ldflags "-s -w -X main.version=${VERSION}" \
            -o "$BIN_SOURCE" \
            ./cmd/agent
    fi

    if [ ! -f "$HELPER_SOURCE" ]; then
        echo -e "${YELLOW}  Building session helper for windows-$ARCH...${NC}"
        cd "$PROJECT_ROOT"
        CGO_ENABLED=0 GOOS=windows GOARCH=$ARCH go build \
            -ldflags "-s -w -X main.version=${VERSION}" \
            -o "$HELPER_SOURCE" \
            ./cmd/session-helper
    fi

    # Try NSIS first (if available)
    if command -v makensis >/dev/null 2>&1; then
        echo -e "${GREEN}  Building NSIS installer for windows-$ARCH...${NC}"
        cd "$PROJECT_ROOT/scripts/nsis"
        if makensis -DVERSION="${VERSION}" -DARCH="${ARCH}" installer.nsi 2>/dev/null; then
            mv "wxt-agent-${VERSION}-windows-${ARCH}-setup.exe" "$BUILD_DIR/" 2>/dev/null || true
            echo -e "  ${GREEN}✓${NC} windows-${ARCH}: wxt-agent-${VERSION}-windows-${ARCH}-setup.exe"
            cd - > /dev/null
            return 0
        fi
        cd - > /dev/null
    fi

    # Fallback: Create ZIP with installer scripts
    echo -e "${YELLOW}  NSIS not available, creating ZIP package...${NC}"

    local PKG_DIR="$BUILD_DIR/pkg-${ARCH}"
    local EXE_NAME="${PACKAGE_NAME}_${VERSION}_windows_${ARCH}.exe"

    rm -rf "$PKG_DIR"
    mkdir -p "$PKG_DIR/wxt-agent"

    # Copy binaries. Both must end up in the SAME directory: the agent looks
    # for the helper next to its own executable.
    cp "$BIN_SOURCE" "$PKG_DIR/wxt-agent/wxt-agent.exe"
    cp "$HELPER_SOURCE" "$PKG_DIR/wxt-agent/wxt-agent-session.exe"

    # Create install script (PowerShell)
    cat > "$PKG_DIR/wxt-agent/install.ps1" << 'PSEOF'
# VSay Agent Installer for Windows
$ErrorActionPreference = "Stop"

Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  VSay Agent Installer" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "This installer requires Administrator privileges." -ForegroundColor Red
    Read-Host "Press Enter to exit"
    exit 1
}

Write-Host "Installing VSay Agent..." -ForegroundColor Green

$installDir = "C:\Program Files\VSay"
if (-not (Test-Path $installDir)) {
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
}

Copy-Item -Path ".\wxt-agent.exe" -Destination "$installDir\wxt-agent.exe" -Force

# The session helper must live beside the agent — that is where the agent
# looks for it when an admin starts a remote-control session.
if (Test-Path ".\wxt-agent-session.exe") {
    Copy-Item -Path ".\wxt-agent-session.exe" -Destination "$installDir\wxt-agent-session.exe" -Force
    Write-Host "Session helper installed (required for remote control)" -ForegroundColor Green
}

$currentPath = [Environment]::GetEnvironmentVariable("Path", "Machine")
if ($currentPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$installDir", "Machine")
}

$configDir = "C:\ProgramData\VSay"
if (-not (Test-Path $configDir)) {
    New-Item -ItemType Directory -Path $configDir -Force | Out-Null
}

Write-Host ""
Write-Host "Installation complete!" -ForegroundColor Green
Write-Host "Open a NEW terminal and run: wxt-agent configure --help"
Read-Host "Press Enter to exit"
PSEOF

    # Create batch file wrapper
    cat > "$PKG_DIR/wxt-agent/install.bat" << 'BATEOF'
@echo off
echo Running installer as Administrator...
powershell -ExecutionPolicy Bypass -File "%~dp0install.ps1"
BATEOF

    # Create README
    cat > "$PKG_DIR/wxt-agent/README.txt" << READMEEOF
VSay Agent ${VERSION} for Windows

CONTENTS:
  wxt-agent.exe          the agent daemon
  wxt-agent-session.exe  the session helper, required for remote control.
                         It MUST sit in the same folder as wxt-agent.exe.

INSTALLATION:
Right-click "install.bat" and select "Run as administrator"

USAGE:
  wxt-agent configure --help
  wxt-agent start --config C:\ProgramData\VSay\agent.yaml

Visit: https://vsay.in
READMEEOF

    # Copy both binaries out as standalone downloads. Someone installing with
    # curl fetches the pair into one directory; that is all the agent needs.
    cp "$BIN_SOURCE" "$BUILD_DIR/${EXE_NAME}"
    cp "$HELPER_SOURCE" "$BUILD_DIR/${PACKAGE_NAME}-session_${VERSION}_windows_${ARCH}.exe"

    # Create ZIP
    cd "$PKG_DIR"
    zip -qr "../${PACKAGE_NAME}_${VERSION}_windows_${ARCH}.zip" "wxt-agent"
    cd - > /dev/null

    # Cleanup
    rm -rf "$PKG_DIR"

    echo -e "  ${GREEN}✓${NC} windows-${ARCH}: ${EXE_NAME} (standalone agent)"
    echo -e "  ${GREEN}✓${NC} windows-${ARCH}: ${PACKAGE_NAME}-session_${VERSION}_windows_${ARCH}.exe (session helper)"
    echo -e "  ${GREEN}✓${NC} windows-${ARCH}: ${PACKAGE_NAME}_${VERSION}_windows_${ARCH}.zip (with installer)"
}

# Build for both architectures
echo "Creating Windows packages..."
echo ""

create_windows_package "amd64"
create_windows_package "arm64"

echo ""
echo -e "${GREEN}✓ Windows packages built${NC}"
ls -la "$BUILD_DIR"/*.exe "$BUILD_DIR"/*.zip 2>/dev/null || true
