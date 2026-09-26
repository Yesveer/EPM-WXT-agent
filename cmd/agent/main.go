package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/vsay/vsay-agent/internal/agent"
	"github.com/vsay/vsay-agent/internal/config"
	"gopkg.in/yaml.v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "vsay-agent",
		Short: "Vsay Agent - Remote terminal access for Linux machines",
		Long:  `Lightweight agent that enables secure remote terminal access to Linux machines`,
	}

	// Version command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("vsay-agent %s (commit: %s, built: %s)\n", version, commit, date)
		},
	})

	// Configure command
	configureCmd := &cobra.Command{
		Use:   "configure",
		Short: "Configure the agent",
		RunE:  runConfigure,
	}
	configureCmd.Flags().String("token", "", "Agent token")
	configureCmd.Flags().String("tenant", "", "Tenant ID/name")
	configureCmd.Flags().String("org", "", "Organization ID/name")
	configureCmd.Flags().String("project", "", "Project ID/name")
	configureCmd.Flags().String("user", "", "Portal user email")
	configureCmd.Flags().String("linux-user", "", "Linux username")
	configureCmd.Flags().String("host", "", "Backend server host")
	configureCmd.Flags().String("api-host", "", "gRPC server address (IP:PORT, e.g., 192.168.1.6:8081)")
	configureCmd.Flags().String("name", "", "Machine name (display name for this machine)")
	configureCmd.Flags().Bool("allow-sudo", false, "Allow sudo command execution (requires user in sudo group)")
	configureCmd.Flags().String("windows-user", "", "Windows username to create/use for RDP (Windows only)")
	configureCmd.Flags().String("windows-password", "", "Password for the Windows RDP user (Windows only)")
	configureCmd.Flags().Bool("interactive", false, "Interactive configuration")
	configureCmd.Flags().String("tunnel-url", "", "vsay-tunnel server URL (e.g. http://192.168.1.20:8083); enables tunneling when set")
	configureCmd.Flags().StringArray("host-entry", []string{}, "Custom /etc/hosts entry in IP:DOMAIN format (e.g. 192.168.1.5:db.internal); may be repeated up to 5 times")
	rootCmd.AddCommand(configureCmd)

	// Start command
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the agent",
		RunE:  runStart,
	}
	startCmd.Flags().String("config", "/etc/vsay/agent.yaml", "Config file path")
	rootCmd.AddCommand(startCmd)

	// Validate command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate configuration",
		RunE:  runValidate,
	})

	// Show-config command — decrypt and print agent.yaml in readable YAML
	showConfigCmd := &cobra.Command{
		Use:   "show-config",
		Short: "Decrypt and print the agent configuration (requires root)",
		RunE:  runShowConfig,
	}
	showConfigCmd.Flags().String("config", "/etc/vsay/agent.yaml", "Config file path")
	rootCmd.AddCommand(showConfigCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func runConfigure(cmd *cobra.Command, args []string) error {
	// Get flags
	token, _ := cmd.Flags().GetString("token")
	tenant, _ := cmd.Flags().GetString("tenant")
	org, _ := cmd.Flags().GetString("org")
	project, _ := cmd.Flags().GetString("project")
	user, _ := cmd.Flags().GetString("user")
	linuxUser, _ := cmd.Flags().GetString("linux-user")
	host, _ := cmd.Flags().GetString("host")
	apiHost, _ := cmd.Flags().GetString("api-host")
	machineName, _ := cmd.Flags().GetString("name")
	allowSudo, _ := cmd.Flags().GetBool("allow-sudo")
	interactive, _ := cmd.Flags().GetBool("interactive")
	tunnelURL, _ := cmd.Flags().GetString("tunnel-url")
	hostEntryRaw, _ := cmd.Flags().GetStringArray("host-entry")

	// Parse --host-entry values (format: "IP:DOMAIN")
	var hostEntries []config.HostEntry
	for _, raw := range hostEntryRaw {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid --host-entry %q: expected format IP:DOMAIN (e.g. 192.168.1.5:db.internal)", raw)
		}
		hostEntries = append(hostEntries, config.HostEntry{IP: parts[0], Domain: parts[1]})
	}
	if len(hostEntries) > 5 {
		return fmt.Errorf("too many --host-entry values: maximum is 5, got %d", len(hostEntries))
	}

	if interactive {
		// TODO: Implement interactive configuration
		fmt.Println("Interactive mode not yet implemented")
		return nil
	}

	// Windows uses a completely separate configure flow (RDP user, Windows paths,
	// scheduled-task service). It lives in configure_windows.go and does NOT touch any
	// of the Linux logic below. On non-Windows this branch is never taken.
	if runtime.GOOS == "windows" {
		winUser, _ := cmd.Flags().GetString("windows-user")
		winPass, _ := cmd.Flags().GetString("windows-password")
		return runConfigureWindows(configureParams{
			Token: token, Tenant: tenant, Org: org, Project: project,
			User: user, Host: host, APIHost: apiHost, MachineName: machineName,
			WindowsUser: winUser, WindowsPassword: winPass, TunnelURL: tunnelURL,
		})
	}

	// Validate required flags
	if token == "" || tenant == "" || org == "" || project == "" || user == "" || linuxUser == "" || host == "" {
		return fmt.Errorf("missing required flags: token, tenant, org, project, user, linux-user, and host are required")
	}

	// Create or validate Linux user
	homeDir, err := ensureLinuxUser(linuxUser, allowSudo)
	if err != nil {
		return fmt.Errorf("Linux user setup failed: %w", err)
	}
	fmt.Printf("✓ Linux user '%s' ready (home: %s)\n", linuxUser, homeDir)

	// Determine gRPC URL
	var grpcURL string
	if apiHost != "" {
		// Use provided api-host directly as gRPC address
		grpcURL = apiHost
		fmt.Printf("✓ Using custom gRPC address: %s\n", grpcURL)
	} else {
		// Extract from host (backward compatibility)
		grpcURL = extractGRPCURL(host)
		fmt.Printf("✓ Using default gRPC address: %s (extracted from --host)\n", grpcURL)
	}

	// Create directories. /etc/vsay and /var/log/vsay are left broadly readable
	// (0755) since other local tooling — log shippers, ops scripts — may
	// reasonably expect to read config/logs there. certs/ holds key material
	// and only this (root-run) agent ever needs it, so it's tightened.
	_ = os.MkdirAll("/etc/vsay", 0755) // #nosec G301
	_ = os.MkdirAll("/etc/vsay/certs", 0750)
	_ = os.MkdirAll("/var/log/vsay", 0755) // #nosec G301

	// Download CA cert from backend and pin it locally
	caCertFile, caFingerprint, err := fetchAndSaveCA(host)
	if err != nil {
		fmt.Printf("Warning: could not pin CA cert: %v\n", err)
		fmt.Println("  Agent will use system CA pool for server verification")
	} else if caFingerprint != "" {
		fmt.Println("✓ CA cert pinned — fingerprint (verify out-of-band):")
		fmt.Printf("  SHA256: %s\n", caFingerprint)
		// A fingerprint hash is not secret — it's meant to be compared/shared
		// for out-of-band verification, so world-readable is fine here.
		if err := os.WriteFile("/etc/vsay/certs/ca-fingerprint.txt", []byte(caFingerprint), 0644); err != nil { // #nosec G306
			fmt.Printf("Warning: could not persist CA fingerprint: %v\n", err)
		}
	}

	if err := fetchSignedClientCert(host, token); err != nil {
		return fmt.Errorf("failed to get signed client cert from backend: %w", err)
	}
	fmt.Println("✓ Client cert signed by backend CA (ECDSA P-256, 1-day validity)")

	// Write custom host entries to /etc/hosts before the agent connects
	if len(hostEntries) > 0 {
		if err := appendHostEntries(hostEntries); err != nil {
			return fmt.Errorf("failed to write host entries: %w", err)
		}
		fmt.Printf("✓ %d host entries written to /etc/hosts\n", len(hostEntries))
	}

	// Build tunnel config — enabled automatically when a URL is provided
	tunnelCfg := config.TunnelConfig{
		URL:          tunnelURL,
		Enabled:      tunnelURL != "",
		PollInterval: "10s",
	}

	// Create configuration
	cfg := &config.Config{
		Agent: config.AgentConfig{
			ID:   "",          // Will be set after registration
			Name: machineName, // User-provided machine name
		},
		Server: config.ServerConfig{
			Host:       host,
			GRPCURL:    grpcURL,
			APIHost:    apiHost,
			TLS:        true,
			TokenHash:  token,
			CACertFile: caCertFile,
		},
		Tunnel: tunnelCfg,
		Tenant: config.TenantConfig{
			ID: tenant,
		},
		Org: config.OrgConfig{
			ID: org,
		},
		Project: config.ProjectConfig{
			ID: project,
		},
		PortalUser: config.PortalUserConfig{
			Email: user,
		},
		LinuxUser: config.LinuxUserConfig{
			Username:    linuxUser,
			HomeDir:     homeDir,
			Shell:       "/bin/bash",
			SudoEnabled: allowSudo,
		},
		Permissions: config.PermissionsConfig{
			AllowSudo: allowSudo,
		},
		Logging: config.LoggingConfig{
			Level:      "info",
			File:       "/var/log/vsay/agent.log",
			MaxSizeMB:  100,
			MaxBackups: 5,
		},
		HostEntries: hostEntries,
		Status:      "pending_approval",
	}

	// Save configuration — AES-256-GCM encrypted, key derived from /etc/machine-id.
	// The file is only decryptable on this machine.
	configPath := "/etc/vsay/agent.yaml"
	if err := config.SaveEncrypted(cfg, configPath); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Println("✓ Configuration saved (encrypted) to", configPath)

	// Ensure systemd service is installed and enabled
	if err := setupSystemdService(); err != nil {
		return fmt.Errorf("failed to setup systemd service: %w", err)
	}

	// Start and enable the service
	if err := startAndEnableService(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	fmt.Println("✓ Agent daemon started and enabled")
	fmt.Println("✓ Agent will auto-start on system boot")
	fmt.Println("")
	fmt.Println("Check status: sudo systemctl status vsay-agent")
	fmt.Println("View logs: sudo journalctl -u vsay-agent -f")

	return nil
}

// generateClientCerts auto-generates a self-signed ECDSA P-256 mTLS client cert + key.
// Skips generation if certs already exist.
func generateClientCerts() error {
	certPath := "/etc/vsay/certs/client-cert.pem"
	keyPath := "/etc/vsay/certs/client-key.pem"

	// Already exists — skip
	if _, err := os.Stat(certPath); err == nil {
		fmt.Println("✓ mTLS client certs already present")
		return nil
	}

	hostname, _ := os.Hostname()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ECDSA key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"Vsay Agent"},
			CommonName:   hostname,
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	keyDER, _ := x509.MarshalECPrivateKey(key)

	// A cert file is public information (no private key material), so 0644 is
	// intentional here — the client's actual private key below stays 0600.
	certOut, err := os.OpenFile(certPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644) // #nosec G302
	if err != nil {
		return fmt.Errorf("create cert file: %w", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		_ = certOut.Close()
		return fmt.Errorf("encode cert: %w", err)
	}
	if err := certOut.Close(); err != nil {
		return fmt.Errorf("close cert file: %w", err)
	}

	keyOut, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		_ = keyOut.Close()
		return fmt.Errorf("encode key: %w", err)
	}
	if err := keyOut.Close(); err != nil {
		return fmt.Errorf("close key file: %w", err)
	}

	fmt.Println("✓ mTLS client cert + key generated at /etc/vsay/certs/")
	return nil
}

// fetchAndSaveCA downloads the private CA cert from the backend and saves it for TLS pinning.
// Returns (certPath, SHA256-fingerprint-hex, error).
func fetchAndSaveCA(host string) (string, string, error) {
	host = strings.TrimSuffix(host, "/")
	resp, err := http.Get(host + "/ca-cert")
	if err != nil {
		return "", "", fmt.Errorf("fetch CA cert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("server returned %d", resp.StatusCode)
	}

	caPEM, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return "", "", fmt.Errorf("read CA cert: %w", err)
	}

	block, _ := pem.Decode(caPEM)
	if block == nil {
		return "", "", fmt.Errorf("invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("parse CA cert: %w", err)
	}

	// Compute SHA-256 fingerprint for out-of-band verification
	sum := sha256.Sum256(cert.Raw)
	fingerprint := fmt.Sprintf("%x", sum)

	// A CA cert is public trust material, not a secret — 0644 is intentional.
	caPath := "/etc/vsay/certs/server-ca.pem"
	if err := os.WriteFile(caPath, caPEM, 0644); err != nil { // #nosec G306
		return "", "", fmt.Errorf("save CA cert: %w", err)
	}
	fmt.Println("✓ CA cert saved at", caPath)
	return caPath, fingerprint, nil
}

// fetchSignedClientCert generates an ECDSA P-256 CSR, sends it to backend, and saves the signed cert.
func fetchSignedClientCert(host, token string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	hostname, _ := os.Hostname()
	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			Organization: []string{"Vsay Agent"},
			CommonName:   hostname,
		},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, key)
	if err != nil {
		return fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// POST CSR to backend
	host = strings.TrimSuffix(host, "/")
	req, err := http.NewRequest("POST", host+"/agent/sign-cert", bytes.NewReader(csrPEM))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-pem-file")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sign-cert request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend returned %d for sign-cert", resp.StatusCode)
	}

	signedCertPEM, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return fmt.Errorf("read signed cert: %w", err)
	}

	// Encrypt key and cert before writing — plaintext private key never persists on disk.
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	encKey, err := agent.EncryptPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("encrypt key: %w", err)
	}
	encCert, err := agent.EncryptPEM(signedCertPEM)
	if err != nil {
		return fmt.Errorf("encrypt cert: %w", err)
	}

	if err := os.WriteFile("/etc/vsay/certs/client-key.enc", encKey, 0600); err != nil {
		return fmt.Errorf("save key: %w", err)
	}
	if err := os.WriteFile("/etc/vsay/certs/client-cert.enc", encCert, 0600); err != nil {
		return fmt.Errorf("save cert: %w", err)
	}
	return nil
}

// appendHostEntries writes custom host entries to /etc/hosts under a vsay-managed block.
// Re-running configure will replace the existing block cleanly.
func appendHostEntries(entries []config.HostEntry) error {
	const hostsFile = "/etc/hosts"
	const blockStart = "# vsay-agent managed hosts — do not edit manually"
	const blockEnd = "# end vsay-agent managed hosts"

	// Read current /etc/hosts
	existing, err := os.ReadFile(hostsFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", hostsFile, err)
	}

	// Strip any existing vsay block
	content := string(existing)
	if s := strings.Index(content, blockStart); s != -1 {
		e := strings.Index(content, blockEnd)
		if e != -1 {
			content = content[:s] + content[e+len(blockEnd):]
		}
	}
	content = strings.TrimRight(content, "\n") + "\n"

	// Build new block
	var block strings.Builder
	block.WriteString(blockStart + "\n")
	for _, entry := range entries {
		fmt.Fprintf(&block, "%-20s %s\n", entry.IP, entry.Domain)
	}
	block.WriteString(blockEnd + "\n")

	// hostsFile is the fixed literal "/etc/hosts" above, not request/user input,
	// and must stay world-readable (0644) — every process on the box resolves
	// hostnames through it.
	return os.WriteFile(hostsFile, []byte(content+block.String()), 0644) // #nosec G302,G306,G703
}

// extractGRPCURL extracts gRPC URL from HTTP host
func extractGRPCURL(host string) string {
	// Remove http:// or https://
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")

	// Extract hostname and port
	parts := strings.Split(host, ":")
	if len(parts) >= 2 {
		// Use same hostname, default gRPC port 8081
		return parts[0] + ":8081"
	}
	// If no port specified, use hostname with default gRPC port
	if host != "" {
		return host + ":8081"
	}
	// Default
	return "localhost:8081"
}

// ensureLinuxUser creates the Linux user if it doesn't exist, sets up home directory,
// and optionally adds to sudo group. Returns the home directory path.
//
// username/allowSudo come from the `configure` command's CLI flags, set by
// whoever is running this installer locally — already a root-equivalent
// operation, not remote/attacker-reachable input. Every exec.Command below
// passes them as separate argv elements (no shell), so they can't be used
// for command injection either way.
func ensureLinuxUser(username string, allowSudo bool) (string, error) {
	homeDir := "/home/" + username

	// Check if user already exists
	cmd := exec.Command("id", username) // #nosec G204
	if err := cmd.Run(); err != nil {
		// User doesn't exist, create it
		fmt.Printf("Creating Linux user '%s'...\n", username)

		// Create user with home directory and bash shell
		// useradd -m -d /home/<user> -s /bin/bash <username>
		createCmd := exec.Command("useradd", "-m", "-d", homeDir, "-s", "/bin/bash", username) // #nosec G204
		output, err := createCmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to create user '%s': %w\nOutput: %s", username, err, string(output))
		}
		fmt.Printf("✓ User '%s' created with home directory: %s\n", username, homeDir)
	} else {
		// User exists, get their home directory. Parsed in Go rather than piped
		// through `bash -c "... | cut ..."` so username never touches a shell —
		// a username containing shell metacharacters could otherwise inject
		// arbitrary commands into that string.
		homeCmd := exec.Command("getent", "passwd", username) // #nosec G204
		output, err := homeCmd.Output()
		if err == nil && len(output) > 0 {
			fields := strings.Split(strings.TrimSpace(string(output)), ":")
			if len(fields) >= 6 {
				homeDir = fields[5]
			}
		}
		fmt.Printf("✓ User '%s' already exists (home: %s)\n", username, homeDir)
	}

	// Handle sudo access
	if allowSudo {
		// Add user to sudo group
		fmt.Printf("Adding '%s' to sudo group...\n", username)

		// Try 'sudo' group first (Debian/Ubuntu), then 'wheel' (RHEL/CentOS)
		sudoGroups := []string{"sudo", "wheel"}
		added := false

		for _, group := range sudoGroups {
			// Check if group exists
			checkCmd := exec.Command("getent", "group", group) // #nosec G204
			if checkCmd.Run() != nil {
				continue
			}

			// Add user to group
			addCmd := exec.Command("usermod", "-aG", group, username) // #nosec G204
			if err := addCmd.Run(); err == nil {
				fmt.Printf("✓ User '%s' added to '%s' group\n", username, group)
				added = true
				break
			}
		}

		if !added {
			return homeDir, fmt.Errorf("failed to add user to sudo group. Neither 'sudo' nor 'wheel' group found")
		}

		// Configure passwordless sudo for this user
		sudoersFile := fmt.Sprintf("/etc/sudoers.d/vsay-%s", username)
		sudoersContent := fmt.Sprintf("%s ALL=(ALL) NOPASSWD: ALL\n", username)

		// 0440 is REQUIRED here, not a choice — visudo itself rejects any
		// sudoers.d file with different permissions, so this must never be
		// "tightened" to 0600 or it breaks sudo for this user entirely.
		if err := os.WriteFile(sudoersFile, []byte(sudoersContent), 0440); err != nil { // #nosec G306
			return homeDir, fmt.Errorf("failed to create sudoers file: %w", err)
		}

		// Validate sudoers file
		validateCmd := exec.Command("visudo", "-c", "-f", sudoersFile) // #nosec G204
		if output, err := validateCmd.CombinedOutput(); err != nil {
			// Invalid sudoers file, remove it
			if rerr := os.Remove(sudoersFile); rerr != nil {
				fmt.Printf("Warning: failed to remove invalid sudoers file: %v\n", rerr)
			}
			return homeDir, fmt.Errorf("invalid sudoers file: %s", string(output))
		}

		fmt.Printf("✓ Passwordless sudo configured for '%s'\n", username)
	} else {
		// Remove passwordless sudo if it was previously configured
		sudoersFile := fmt.Sprintf("/etc/sudoers.d/vsay-%s", username)
		if err := os.Remove(sudoersFile); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Warning: failed to remove sudoers file: %v\n", err)
		}

		fmt.Printf("✓ User '%s' configured as normal user (no sudo access)\n", username)
	}

	// Ensure home directory has correct ownership
	chownCmd := exec.Command("chown", "-R", username+":"+username, homeDir) // #nosec G204
	if err := chownCmd.Run(); err != nil {
		fmt.Printf("Warning: chown %s failed (may already be correct): %v\n", homeDir, err)
	}

	return homeDir, nil
}

// setupSystemdService ensures systemd service is installed (always overwrites to apply updates)
func setupSystemdService() error {
	serviceFile := "/etc/systemd/system/vsay-agent.service"

	// Stop existing service if running (ignore errors if not running)
	if err := exec.Command("systemctl", "stop", "vsay-agent").Run(); err != nil { // #nosec G204
		fmt.Printf("(service was not running: %v)\n", err)
	}

	// Always create/overwrite service file to ensure latest configuration
	serviceContent := `[Unit]
Description=Vsay Agent - Remote Terminal Access
After=network.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/vsay-agent start --config /etc/vsay/agent.yaml
Restart=on-failure
RestartSec=10s
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

	// systemd unit files are conventionally 0644 (world-readable) — that's the
	// standard every other .service file on the box has, and tools like
	// `systemctl cat`/`status` expect to be able to read them as any user.
	if err := os.WriteFile(serviceFile, []byte(serviceContent), 0644); err != nil { // #nosec G306
		return fmt.Errorf("failed to create service file: %w", err)
	}

	// Reload systemd
	cmd := exec.Command("systemctl", "daemon-reload")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	return nil
}

// startAndEnableService starts and enables the systemd service
func startAndEnableService() error {
	// Enable service
	cmd := exec.Command("systemctl", "enable", "vsay-agent")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable service: %w", err)
	}

	// Start service
	cmd = exec.Command("systemctl", "start", "vsay-agent")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	return nil
}

func runStart(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	// Guaranteed early marker to the default log file — proves THIS binary ran (with a
	// fresh timestamp) even before config load. If this line never appears in the log,
	// an old binary is running.
	writeBootLog(fmt.Sprintf("=== vsay-agent %s starting (config=%s) ===", version, configPath))

	// Load configuration — handles both encrypted (new) and plaintext (old) formats.
	cfg, err := config.LoadEncrypted(configPath)
	if err != nil {
		// Config load failed before the logger is up. Write the reason to the default
		// log file so a background service (no console) is still diagnosable.
		writeBootError(fmt.Sprintf("failed to load config %s: %v", configPath, err))
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Setup logger with backend-style formatting
	loggerConfig := zap.Config{
		Level:       zap.NewAtomicLevelAt(zap.InfoLevel),
		Development: false,
		Encoding:    "console",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:       "ts",
			LevelKey:      "level",
			NameKey:       "logger",
			CallerKey:     "",
			FunctionKey:   zapcore.OmitKey,
			MessageKey:    "msg",
			StacktraceKey: "stacktrace",
			LineEnding:    zapcore.DefaultLineEnding,
			EncodeLevel:   zapcore.CapitalLevelEncoder,
			EncodeTime: func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
				enc.AppendString(t.Format("2006-01-02 15:04:05"))
			},
			EncodeDuration:   zapcore.StringDurationEncoder,
			EncodeCaller:     zapcore.ShortCallerEncoder,
			ConsoleSeparator: " | ",
		},
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	// Write logs to BOTH stdout and a log file. Without the file sink, a background
	// service (Windows scheduled task as SYSTEM, or systemd) has no visible log —
	// this is what makes Windows startup issues diagnosable. The file is opened
	// directly (not via zap's OutputPaths) to avoid zap's Windows drive-letter path
	// parsing issue (C:\ read as a URL scheme).
	//
	// Fall back to the platform default path if the config has no log file set (e.g.
	// an older config), so a log ALWAYS appears.
	logFilePath := cfg.Logging.File
	if logFilePath == "" {
		logFilePath = defaultLogFile()
	}
	logSinks := []zapcore.WriteSyncer{zapcore.AddSync(os.Stdout)}
	if logFilePath != "" {
		// logFilePath is this agent's own config value (cfg.Logging.File) or the
		// platform default, never request/user input. Left world-readable
		// (0755/0644) since ops/monitoring tooling on the box may reasonably
		// expect to read agent logs.
		if err := os.MkdirAll(filepath.Dir(logFilePath), 0o755); err != nil { // #nosec G301,G304
			fmt.Fprintf(os.Stderr, "warning: cannot create log dir %s: %v\n", filepath.Dir(logFilePath), err)
		}
		if lf, ferr := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil { // #nosec G302,G304
			logSinks = append(logSinks, zapcore.AddSync(lf))
		} else {
			fmt.Fprintf(os.Stderr, "warning: cannot open log file %s: %v\n", logFilePath, ferr)
		}
	}
	logger := zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(loggerConfig.EncoderConfig),
		zapcore.NewMultiWriteSyncer(logSinks...),
		loggerConfig.Level,
	))
	defer logger.Sync()

	logger.Info("Starting vsay-agent",
		zap.String("version", version),
		zap.String("agent_id", cfg.Agent.ID),
		zap.String("linux_user", cfg.LinuxUser.Username),
	)

	// Propagate the compiled version into the agent package so it gets
	// reported to the backend in register metadata (for self-update).
	agent.BuildVersion = version

	// Create agent
	agt, err := agent.New(cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to create agent: %w", err)
	}

	// Start agent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		if err := agt.Start(ctx); err != nil {
			errChan <- err
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errChan:
		return fmt.Errorf("agent error: %w", err)
	case sig := <-sigChan:
		logger.Info("Received signal, shutting down", zap.String("signal", sig.String()))
		cancel()
		time.Sleep(2 * time.Second)
	}

	return nil
}

func runValidate(cmd *cobra.Command, args []string) error {
	// TODO: Implement configuration validation
	fmt.Println("Validation not yet implemented")
	return nil
}

func runShowConfig(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	cfg, err := config.LoadEncrypted(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	// Marshal back to YAML for human-readable output.
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	fmt.Printf("# Decrypted config from %s\n", configPath)
	fmt.Print(string(out))
	return nil
}
