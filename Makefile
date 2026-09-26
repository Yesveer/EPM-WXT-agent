.PHONY: help build build-all install clean test lint proto proto-install
.PHONY: build-linux build-macos build-windows
.PHONY: build-deb build-rpm build-dmg build-exe build-tar
.PHONY: packages all deploy-binaries

# Version info
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "1.0.0")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) -extldflags '-static'"

# Directories
DIST_DIR := dist
BIN_DIR := $(DIST_DIR)/bin
AGENT_BACKEND_DIR ?= ../vsay-agent-backend

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# =============================================================================
# MAIN TARGETS
# =============================================================================

all: packages ## Build everything (all platforms, all packages)

packages: build-all build-deb build-rpm build-tar build-dmg build-exe ## Build all package formats

# =============================================================================
# PROTO GENERATION
# =============================================================================

proto: ## Generate protobuf code
	@echo "Generating protobuf code..."
	@chmod +x scripts/generate-proto.sh
	@./scripts/generate-proto.sh

proto-install: ## Install protobuf tools
	@echo "Installing protobuf tools..."
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "✓ Tools installed. Install protoc from: https://grpc.io/docs/protoc-installation/"

# =============================================================================
# BUILD BINARIES
# =============================================================================

build: proto ## Build for current platform
	@echo "Building vsay-agent for current platform..."
	@go build $(LDFLAGS) -o vsay-agent ./cmd/agent
	@echo "✓ Built: vsay-agent"

build-all: proto build-linux build-macos build-windows ## Build binaries for all platforms
	@echo ""
	@echo "✓ All binaries built successfully!"
	@ls -la $(BIN_DIR)/

build-linux: proto ## Build for Linux (amd64, arm64)
	@echo "Building for Linux..."
	@mkdir -p $(BIN_DIR)/linux-amd64 $(BIN_DIR)/linux-arm64
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/vsay-agent ./cmd/agent
	@echo "  ✓ linux-amd64"
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/vsay-agent ./cmd/agent
	@echo "  ✓ linux-arm64"

build-macos: proto ## Build for macOS (amd64, arm64)
	@echo "Building for macOS..."
	@mkdir -p $(BIN_DIR)/darwin-amd64 $(BIN_DIR)/darwin-arm64
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/darwin-amd64/vsay-agent ./cmd/agent
	@echo "  ✓ darwin-amd64"
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/darwin-arm64/vsay-agent ./cmd/agent
	@echo "  ✓ darwin-arm64"

build-windows: proto ## Build for Windows (amd64, arm64)
	@echo "Building for Windows..."
	@mkdir -p $(BIN_DIR)/windows-amd64 $(BIN_DIR)/windows-arm64
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/windows-amd64/vsay-agent.exe ./cmd/agent
	@echo "  ✓ windows-amd64"
	@CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/windows-arm64/vsay-agent.exe ./cmd/agent
	@echo "  ✓ windows-arm64"

# =============================================================================
# PACKAGE BUILDS
# =============================================================================

build-deb: build-linux ## Build DEB packages (Debian/Ubuntu) for amd64 and arm64
	@echo ""
	@echo "Building DEB packages..."
	@mkdir -p $(DIST_DIR)/deb
	@ARCH=amd64 VERSION=$(VERSION) ./scripts/build-deb.sh
	@ARCH=arm64 VERSION=$(VERSION) ./scripts/build-deb.sh
	@echo "✓ DEB packages built"

build-rpm: build-linux ## Build RPM packages (RHEL/CentOS/Rocky/Fedora) for amd64 and arm64
	@echo ""
	@echo "Building RPM packages..."
	@mkdir -p $(DIST_DIR)/rpm
	@ARCH=amd64 VERSION=$(VERSION) ./scripts/build-rpm.sh
	@ARCH=arm64 VERSION=$(VERSION) ./scripts/build-rpm.sh
	@echo "✓ RPM packages built"

build-tar: build-linux build-macos ## Build TAR.GZ packages (Universal) for all platforms
	@echo ""
	@echo "Building TAR.GZ packages..."
	@mkdir -p $(DIST_DIR)/tar
	@./scripts/build-tar.sh
	@echo "✓ TAR.GZ packages built"

build-dmg: build-macos ## Build DMG packages (macOS) for amd64 and arm64
	@echo ""
	@echo "Building DMG packages..."
	@mkdir -p $(DIST_DIR)/dmg
	@./scripts/build-dmg.sh
	@echo "✓ DMG packages built"

build-exe: build-windows ## Build Windows installers (EXE) for amd64 and arm64
	@echo ""
	@echo "Building Windows packages..."
	@mkdir -p $(DIST_DIR)/windows
	@./scripts/build-windows.sh
	@echo "✓ Windows packages built"

# =============================================================================
# UTILITIES
# =============================================================================

install: build ## Build and install agent locally
	@echo "Installing vsay-agent..."
	@sudo cp vsay-agent /usr/local/bin/
	@sudo chmod +x /usr/local/bin/vsay-agent
	@echo "✓ Installed to /usr/local/bin/vsay-agent"

clean: ## Clean build artifacts
	@echo "Cleaning..."
	@rm -f vsay-agent vsay-agent.exe
	@rm -rf $(DIST_DIR)/
	@echo "✓ Cleaned"

test: ## Run tests
	@echo "Running tests..."
	@go test ./...

lint: ## Run linter
	@echo "Running linter..."
	@if command -v golangci-lint &> /dev/null; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed. Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi

run: build ## Build and run agent
	@./vsay-agent start --config config.yaml

dev: ## Run in development mode
	@go run cmd/agent/main.go start --config config.yaml

# =============================================================================
# RELEASE
# =============================================================================

deploy-binaries: packages ## Build all packages and copy to vsay-agent-backend/agent-binaries/
	@echo ""
	@echo "Deploying packages to $(AGENT_BACKEND_DIR)/agent-binaries/..."
	@mkdir -p $(AGENT_BACKEND_DIR)/agent-binaries
	@f=$$(ls -t $(DIST_DIR)/deb/vsay-agent_*_amd64.deb 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-amd64.deb && echo "  ✓ vsay-agent-amd64.deb" || echo "  ✗ vsay-agent-amd64.deb not found"
	@f=$$(ls -t $(DIST_DIR)/deb/vsay-agent_*_arm64.deb 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-arm64.deb && echo "  ✓ vsay-agent-arm64.deb" || echo "  ✗ vsay-agent-arm64.deb not found"
	@f=$$(ls -t $(DIST_DIR)/tar/vsay-agent-*-linux-amd64.tar.gz 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-x86_64.tar.gz && echo "  ✓ vsay-agent-x86_64.tar.gz" || echo "  ✗ vsay-agent-x86_64.tar.gz not found"
	@f=$$(ls -t $(DIST_DIR)/tar/vsay-agent-*-linux-arm64.tar.gz 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-aarch64.tar.gz && echo "  ✓ vsay-agent-aarch64.tar.gz" || echo "  ✗ vsay-agent-aarch64.tar.gz not found"
	@f=$$(ls -t $(DIST_DIR)/dmg/vsay-agent-*-macos-amd64.dmg 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-amd64.dmg && echo "  ✓ vsay-agent-amd64.dmg" || echo "  ✗ vsay-agent-amd64.dmg not found (macOS only)"
	@f=$$(ls -t $(DIST_DIR)/dmg/vsay-agent-*-macos-arm64.dmg 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-arm64.dmg && echo "  ✓ vsay-agent-arm64.dmg" || echo "  ✗ vsay-agent-arm64.dmg not found (macOS only)"
	@f=$$(ls -t $(DIST_DIR)/windows/vsay-agent_*_windows_amd64.exe 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-amd64.exe && echo "  ✓ vsay-agent-amd64.exe" || echo "  ✗ vsay-agent-amd64.exe not found"
	@f=$$(ls -t $(DIST_DIR)/windows/vsay-agent_*_windows_arm64.exe 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/vsay-agent-arm64.exe && echo "  ✓ vsay-agent-arm64.exe" || echo "  ✗ vsay-agent-arm64.exe not found"
	@echo ""
	@echo "✓ Deploy complete!"
	@ls -lh $(AGENT_BACKEND_DIR)/agent-binaries/ | grep -v gitkeep

release: clean packages ## Build release packages
	@echo ""
	@echo "=============================================="
	@echo "Release packages built successfully!"
	@echo "=============================================="
	@echo ""
	@echo "DEB packages (Debian/Ubuntu):"
	@ls -la $(DIST_DIR)/deb/*.deb 2>/dev/null || echo "  (none)"
	@echo ""
	@echo "RPM packages (RHEL/CentOS/Rocky/Fedora):"
	@ls -la $(DIST_DIR)/rpm/*.rpm 2>/dev/null || echo "  (none)"
	@echo ""
	@echo "TAR.GZ packages (Universal):"
	@ls -la $(DIST_DIR)/tar/*.tar.gz 2>/dev/null || echo "  (none)"
	@echo ""
	@echo "DMG packages (macOS):"
	@ls -la $(DIST_DIR)/dmg/*.dmg 2>/dev/null || echo "  (none)"
	@echo ""
	@echo "Windows packages:"
	@ls -la $(DIST_DIR)/windows/*.zip 2>/dev/null || echo "  (none)"
