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

PACKAGE_NAME="vsay-agent"
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
    local BIN_SOURCE="$DIST_DIR/bin/windows-${ARCH}/vsay-agent.exe"

    if [ ! -f "$BIN_SOURCE" ]; then
        echo -e "${YELLOW}  Building binary for windows-$ARCH...${NC}"
        mkdir -p "$DIST_DIR/bin/windows-${ARCH}"
        cd "$PROJECT_ROOT"
        CGO_ENABLED=0 GOOS=windows GOARCH=$ARCH go build \
            -ldflags "-s -w -X main.version=${VERSION}" \
            -o "$BIN_SOURCE" \
            ./cmd/agent
    fi

    # Try NSIS first (if available)
    if command -v makensis >/dev/null 2>&1; then
        echo -e "${GREEN}  Building NSIS installer for windows-$ARCH...${NC}"
        cd "$PROJECT_ROOT/scripts/nsis"
        if makensis -DVERSION="${VERSION}" -DARCH="${ARCH}" installer.nsi 2>/dev/null; then
            mv "vsay-agent-${VERSION}-windows-${ARCH}-setup.exe" "$BUILD_DIR/" 2>/dev/null || true
            echo -e "  ${GREEN}✓${NC} windows-${ARCH}: vsay-agent-${VERSION}-windows-${ARCH}-setup.exe"
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
    mkdir -p "$PKG_DIR/vsay-agent"

    # Copy binary
    cp "$BIN_SOURCE" "$PKG_DIR/vsay-agent/vsay-agent.exe"

    # Create install script (PowerShell)
    cat > "$PKG_DIR/vsay-agent/install.ps1" << 'PSEOF'
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

Copy-Item -Path ".\vsay-agent.exe" -Destination "$installDir\vsay-agent.exe" -Force

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
Write-Host "Open a NEW terminal and run: vsay-agent configure --help"
Read-Host "Press Enter to exit"
PSEOF

    # Create batch file wrapper
    cat > "$PKG_DIR/vsay-agent/install.bat" << 'BATEOF'
@echo off
echo Running installer as Administrator...
powershell -ExecutionPolicy Bypass -File "%~dp0install.ps1"
BATEOF

    # Create README
    cat > "$PKG_DIR/vsay-agent/README.txt" << READMEEOF
VSay Agent ${VERSION} for Windows

INSTALLATION:
Right-click "install.bat" and select "Run as administrator"

USAGE:
  vsay-agent configure --help
  vsay-agent start --config C:\ProgramData\VSay\agent.yaml

Visit: https://vsay.in
READMEEOF

    # Copy binary as standalone EXE
    cp "$BIN_SOURCE" "$BUILD_DIR/${EXE_NAME}"

    # Create ZIP
    cd "$PKG_DIR"
    zip -qr "../${PACKAGE_NAME}_${VERSION}_windows_${ARCH}.zip" "vsay-agent"
    cd - > /dev/null

    # Cleanup
    rm -rf "$PKG_DIR"

    echo -e "  ${GREEN}✓${NC} windows-${ARCH}: ${EXE_NAME} (standalone)"
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
