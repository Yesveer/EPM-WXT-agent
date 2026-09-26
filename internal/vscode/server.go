package vscode

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.uber.org/zap"
)

const (
	// maxExtractFileSize bounds a single extracted file — generous headroom
	// over any real code-server binary, just guards against a corrupt or
	// malicious archive claiming an absurd size.
	maxExtractFileSize = 1 << 30 // 1 GiB
	// maxExtractTotalSize bounds the whole archive's extracted size.
	maxExtractTotalSize = 2 << 30 // 2 GiB
)

// Server manages code-server installation and lifecycle
type Server struct {
	logger     *zap.Logger
	installDir string
	binPath    string
	port       int
}

// NewServer creates a new VS Code server instance
func NewServer(logger *zap.Logger) *Server {
	homeDir, _ := os.UserHomeDir()
	installDir := filepath.Join(homeDir, ".vsay-code-server")

	return &Server{
		logger:     logger,
		installDir: installDir,
		port:       8080,
	}
}

// Install downloads and installs code-server if not already installed
func (s *Server) Install(ctx context.Context) error {
	// Check if already installed
	binPath := filepath.Join(s.installDir, "bin", "code-server")
	if _, err := os.Stat(binPath); err == nil {
		s.logger.Info("code-server already installed", zap.String("path", binPath))
		s.binPath = binPath
		return nil
	}

	s.logger.Info("Installing code-server", zap.String("install_dir", s.installDir))

	// Create install directory — owner-only; nothing else on the box needs to
	// read this per-user code-server install.
	if err := os.MkdirAll(s.installDir, 0750); err != nil {
		return fmt.Errorf("failed to create install directory: %w", err)
	}

	// Download code-server
	downloadURL := s.getDownloadURL()
	if downloadURL == "" {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	s.logger.Info("Downloading code-server", zap.String("url", downloadURL))

	resp, err := http.Get(downloadURL) // #nosec G107 -- downloadURL is a hardcoded literal picked by runtime.GOOS/GOARCH, not request/user input
	if err != nil {
		return fmt.Errorf("failed to download code-server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download: status %d", resp.StatusCode)
	}

	// Extract tar.gz
	if err := s.extractTarGz(resp.Body); err != nil {
		return fmt.Errorf("failed to extract: %w", err)
	}

	// Verify binary exists
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("binary not found after installation: %w", err)
	}

	// Make binary executable — must keep the execute bit or code-server can
	// never run; this is a binary, not a secret, so 0755 is intentional here.
	if err := os.Chmod(binPath, 0755); err != nil { // #nosec G302
		return fmt.Errorf("failed to make binary executable: %w", err)
	}

	s.binPath = binPath
	s.logger.Info("code-server installed successfully", zap.String("path", binPath))

	return nil
}

// Start starts code-server with specified folder
func (s *Server) Start(ctx context.Context, sessionID, folder string) (*exec.Cmd, error) {
	if s.binPath == "" {
		return nil, fmt.Errorf("code-server not installed")
	}

	s.logger.Info("Starting code-server",
		zap.String("path", s.binPath),
		zap.String("session_id", sessionID),
		zap.String("folder", folder))

	// Build command arguments
	args := []string{
		"--bind-addr", fmt.Sprintf("0.0.0.0:%d", s.port),
		"--auth", "none",
		"--disable-telemetry",
		"--disable-update-check",
	}

	// Add folder if specified
	if folder != "" {
		args = append(args, folder)
	}

	// s.binPath is the code-server binary this Install() step just verified
	// exists on disk, and args are the fixed flags built above — neither is
	// request input.
	cmd := exec.CommandContext(ctx, s.binPath, args...) // #nosec G204
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("VSCODE_PROXY_URI=http://0.0.0.0:%d", s.port),
	)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start code-server: %w", err)
	}

	s.logger.Info("code-server started",
		zap.Int("pid", cmd.Process.Pid),
		zap.Int("port", s.port),
		zap.String("folder", folder))

	return cmd, nil
}

// GetPort returns the port code-server is running on
func (s *Server) GetPort() int {
	return s.port
}

// getDownloadURL returns the download URL for code-server based on platform
func (s *Server) getDownloadURL() string {
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "arm64" {
			return "https://github.com/coder/code-server/releases/download/v4.96.2/code-server-4.96.2-linux-arm64.tar.gz"
		}
		return "https://github.com/coder/code-server/releases/download/v4.96.2/code-server-4.96.2-linux-amd64.tar.gz"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "https://github.com/coder/code-server/releases/download/v4.96.2/code-server-4.96.2-darwin-arm64.tar.gz"
		}
		return "https://github.com/coder/code-server/releases/download/v4.96.2/code-server-4.96.2-darwin-amd64.tar.gz"
	}
	return ""
}

// extractTarGz extracts a tar.gz archive
func (s *Server) extractTarGz(r io.Reader) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	// installRoot is the resolved boundary every extracted file must stay
	// within — guards against a "Zip Slip" archive entry (e.g. "../../etc/...")
	// that would otherwise let a malicious/compromised download source write
	// anywhere on disk this (often root-privileged) agent can reach.
	installRoot := filepath.Clean(s.installDir)

	var totalWritten int64
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Skip directories
		if header.Typeflag == tar.TypeDir {
			continue
		}

		// Remove version prefix from path (e.g., "code-server-4.96.2-linux-amd64/")
		path := header.Name
		if idx := strings.Index(path, "/"); idx != -1 {
			path = path[idx+1:]
		}

		target := filepath.Clean(filepath.Join(s.installDir, path)) // #nosec G305 -- validated against installRoot on the next line
		if target != installRoot && !strings.HasPrefix(target, installRoot+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry escapes install directory: %q", header.Name)
		}

		// Bound both the declared and actually-copied size — a malicious or
		// corrupt archive can't exhaust disk via a huge claimed size or a
		// gzip decompression bomb.
		if header.Size > maxExtractFileSize {
			return fmt.Errorf("archive entry too large: %q (%d bytes)", header.Name, header.Size)
		}
		totalWritten += header.Size
		if totalWritten > maxExtractTotalSize {
			return fmt.Errorf("archive exceeds max total extracted size (%d bytes)", maxExtractTotalSize)
		}

		// Create parent directory — owner-only, matches installRoot above.
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}

		// Extract file — target has already been confirmed to stay inside installRoot above.
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode)) // #nosec G304,G115 -- header.Mode is a tar permission bitmask (0-07777), never near uint32's range
		if err != nil {
			return err
		}

		written, err := io.CopyN(f, tr, maxExtractFileSize+1)
		if err != nil && err != io.EOF {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		if written > maxExtractFileSize {
			return fmt.Errorf("archive entry %q exceeded max size during extraction", header.Name)
		}
	}

	return nil
}
