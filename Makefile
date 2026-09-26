.PHONY: help build build-all install clean test lint proto proto-install
.PHONY: build-linux build-macos build-windows
.PHONY: build-deb build-rpm build-dmg build-exe build-tar
.PHONY: packages all deploy-binaries

# Version info
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "1.0.0")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) -extldflags '-static'"

# The session helper cannot be linked statically on macOS: it needs cgo for
# ScreenCaptureKit, CGEvent and NSPasteboard, and Apple does not ship the static
# crt0.o that -static requires. These flags are the same minus -static.
HELPER_LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

# The agent embeds the session helper so that each platform ships as ONE file.
# //go:embed reads from this fixed path, so the helper for a given target is
# built into it immediately before the agent for that same target — the two
# builds cannot be reordered or parallelised.
EMBED_SLOT := internal/remotecontrol/helperbin/helper.bin

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

packages: build-all build-deb build-rpm build-tar build-dmg build-exe ## Build every package format, Linux included

packages-desktop: build-dmg build-exe ## Build only the platforms EPM supports (macOS, Windows)

# The embed slot (see EMBED_SLOT) is shared mutable state: each target writes
# the helper for its own platform into the same file before building the agent.
# A parallel make would interleave those writes and embed the wrong helper, so
# this Makefile must run serially.
.NOTPARALLEL:

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
	@echo "Building wxt-agent for current platform..."
	@go build $(LDFLAGS) -o wxt-agent ./cmd/agent
	@echo "✓ Built: wxt-agent"
	@go build $(HELPER_LDFLAGS) -o wxt-agent-session ./cmd/session-helper
	@echo "✓ Built: wxt-agent-session"

build-all: proto build-linux build-macos build-windows ## Build binaries for all platforms
	@echo ""
	@echo "✓ All binaries built successfully!"
	@ls -la $(BIN_DIR)/

build-linux: proto ## Build for Linux (amd64, arm64)
	@echo "Building for Linux..."
	@mkdir -p $(BIN_DIR)/linux-amd64 $(BIN_DIR)/linux-arm64
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/wxt-agent ./cmd/agent
	@echo "  ✓ linux-amd64"
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/wxt-agent ./cmd/agent
	@echo "  ✓ linux-arm64"

build-macos: proto ## Build for macOS (amd64, arm64)
	@echo "Building for macOS..."
	@mkdir -p $(BIN_DIR)/darwin-amd64 $(BIN_DIR)/darwin-arm64
	@# The session helper MUST be built with cgo: screen capture is
	@# ScreenCaptureKit, input is CGEvent and the clipboard is NSPasteboard,
	@# none of which exist without it. Cross-arch therefore needs an explicit
	@# -arch for the C compiler, and both builds need macOS + Xcode CLT.
	@# Helper first, then embedded into the agent for the same arch.
	@CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 CGO_CFLAGS="-arch x86_64" CGO_LDFLAGS="-arch x86_64" \
		go build $(HELPER_LDFLAGS) -o $(BIN_DIR)/darwin-amd64/wxt-agent-session ./cmd/session-helper
	@echo "  ✓ darwin-amd64 session helper"
	@cp $(BIN_DIR)/darwin-amd64/wxt-agent-session $(EMBED_SLOT)
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/darwin-amd64/wxt-agent ./cmd/agent
	@echo "  ✓ darwin-amd64 (helper embedded)"
	@CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 CGO_CFLAGS="-arch arm64" CGO_LDFLAGS="-arch arm64" \
		go build $(HELPER_LDFLAGS) -o $(BIN_DIR)/darwin-arm64/wxt-agent-session ./cmd/session-helper
	@echo "  ✓ darwin-arm64 session helper"
	@cp $(BIN_DIR)/darwin-arm64/wxt-agent-session $(EMBED_SLOT)
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/darwin-arm64/wxt-agent ./cmd/agent
	@echo "  ✓ darwin-arm64 (helper embedded)"
	@$(MAKE) --no-print-directory reset-embed-slot

build-windows: proto ## Build for Windows (amd64, arm64)
	@echo "Building for Windows..."
	@mkdir -p $(BIN_DIR)/windows-amd64 $(BIN_DIR)/windows-arm64
	@# The Windows helper is plain syscalls against user32/gdi32, so unlike the
	@# macOS one it needs no cgo and cross-compiles from anywhere.
	@# Helper first, then embedded into the agent for the same arch.
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/windows-amd64/wxt-agent-session.exe ./cmd/session-helper
	@echo "  ✓ windows-amd64 session helper"
	@cp $(BIN_DIR)/windows-amd64/wxt-agent-session.exe $(EMBED_SLOT)
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/windows-amd64/wxt-agent.exe ./cmd/agent
	@echo "  ✓ windows-amd64 (helper embedded)"
	@CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/windows-arm64/wxt-agent-session.exe ./cmd/session-helper
	@echo "  ✓ windows-arm64 session helper"
	@cp $(BIN_DIR)/windows-arm64/wxt-agent-session.exe $(EMBED_SLOT)
	@CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/windows-arm64/wxt-agent.exe ./cmd/agent
	@echo "  ✓ windows-arm64 (helper embedded)"
	@$(MAKE) --no-print-directory reset-embed-slot

# =============================================================================
# PACKAGE BUILDS
# =============================================================================

build-deb: build-linux ## Build DEB packages (Debian/Ubuntu) for amd64 and arm64
	@echo ""
	@echo "Building DEB packages..."
	@mkdir -p $(DIST_DIR)/deb
	@# Skip rather than fail when no packaging tool is installed. EPM targets
	@# Windows and macOS, and a dev machine without dpkg/nfpm must still be
	@# able to run the full deploy for those.
	@if command -v nfpm >/dev/null 2>&1 || command -v dpkg-deb >/dev/null 2>&1 || command -v docker >/dev/null 2>&1; then \
		ARCH=amd64 VERSION=$(VERSION) ./scripts/build-deb.sh && \
		ARCH=arm64 VERSION=$(VERSION) ./scripts/build-deb.sh && \
		echo "✓ DEB packages built"; \
	else \
		echo "  ⊘ skipped — no nfpm, dpkg-deb or docker on this machine"; \
	fi

build-rpm: build-linux ## Build RPM packages (RHEL/CentOS/Rocky/Fedora) for amd64 and arm64
	@echo ""
	@echo "Building RPM packages..."
	@mkdir -p $(DIST_DIR)/rpm
	@if command -v nfpm >/dev/null 2>&1 || command -v rpmbuild >/dev/null 2>&1 || command -v docker >/dev/null 2>&1; then \
		ARCH=amd64 VERSION=$(VERSION) ./scripts/build-rpm.sh && \
		ARCH=arm64 VERSION=$(VERSION) ./scripts/build-rpm.sh && \
		echo "✓ RPM packages built"; \
	else \
		echo "  ⊘ skipped — no nfpm, rpmbuild or docker on this machine"; \
	fi

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

reset-embed-slot: ## Restore the embed placeholder (run automatically after builds)
	@printf '%s\n' \
		'WXT-HELPER-PLACEHOLDER' \
		'This file is a build-time placeholder, not a binary.' \
		'' \
		'//go:embed cannot compile against a file that does not exist, so one has to be' \
		'committed. The Makefile overwrites it with the real session helper for the' \
		'target platform immediately before building the agent, which is what makes the' \
		'agent a single self-contained download.' \
		'' \
		'An agent built without that step embeds THIS text. helperbin.Available()' \
		'detects it by the marker on the first line and reports that nothing usable is' \
		'embedded, so the agent falls back to looking for the helper on disk rather than' \
		'extracting garbage and failing in a way nobody could diagnose.' \
		> $(EMBED_SLOT)

# =============================================================================
# UTILITIES
# =============================================================================

install: build ## Build and install agent locally
	@echo "Installing wxt-agent..."
	@sudo cp wxt-agent /usr/local/bin/
	@sudo chmod +x /usr/local/bin/wxt-agent
	@echo "✓ Installed to /usr/local/bin/wxt-agent"

clean: ## Clean build artifacts
	@echo "Cleaning..."
	@rm -f wxt-agent wxt-agent.exe
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
	@./wxt-agent start --config config.yaml

dev: ## Run in development mode
	@go run cmd/agent/main.go start --config config.yaml

# =============================================================================
# RELEASE
# =============================================================================

deploy-binaries: packages-desktop ## Build macOS + Windows packages and copy to vsay-agent-backend/agent-binaries/
	@echo ""
	@echo "Deploying packages to $(AGENT_BACKEND_DIR)/agent-binaries/..."
	@mkdir -p $(AGENT_BACKEND_DIR)/agent-binaries
	@# Only macOS and Windows: those are the platforms remote control runs on.
	@# Linux machines are managed through the terminal and have no desktop to
	@# take over, so publishing a Linux agent here would advertise a feature it
	@# cannot provide.
	@f=$$(ls -t $(DIST_DIR)/dmg/wxt-agent-*-macos-amd64.dmg 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/wxt-agent-amd64.dmg && echo "  ✓ wxt-agent-amd64.dmg" || echo "  ✗ wxt-agent-amd64.dmg not found (macOS only)"
	@f=$$(ls -t $(DIST_DIR)/dmg/wxt-agent-*-macos-arm64.dmg 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/wxt-agent-arm64.dmg && echo "  ✓ wxt-agent-arm64.dmg" || echo "  ✗ wxt-agent-arm64.dmg not found (macOS only)"
	@f=$$(ls -t $(DIST_DIR)/windows/wxt-agent_*_windows_amd64.exe 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/wxt-agent-amd64.exe && echo "  ✓ wxt-agent-amd64.exe" || echo "  ✗ wxt-agent-amd64.exe not found"
	@f=$$(ls -t $(DIST_DIR)/windows/wxt-agent_*_windows_arm64.exe 2>/dev/null | head -1) && [ -n "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/wxt-agent-arm64.exe && echo "  ✓ wxt-agent-arm64.exe" || echo "  ✗ wxt-agent-arm64.exe not found"
	@echo ""
	@echo "(session helper is bundled inside each agent binary — no separate download)"
	@echo "macOS standalone agent binaries (for curl installs)..."
	@f=$(BIN_DIR)/darwin-amd64/wxt-agent; [ -f "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/wxt-agent-macos-amd64 && echo "  ✓ wxt-agent-macos-amd64" || echo "  ✗ wxt-agent-macos-amd64 not found"
	@f=$(BIN_DIR)/darwin-arm64/wxt-agent; [ -f "$$f" ] && cp "$$f" $(AGENT_BACKEND_DIR)/agent-binaries/wxt-agent-macos-arm64 && echo "  ✓ wxt-agent-macos-arm64" || echo "  ✗ wxt-agent-macos-arm64 not found"
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
