package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	osExec "os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Yesveer/wxt-agent/internal/config"
	"github.com/Yesveer/wxt-agent/internal/executor"
	"github.com/Yesveer/wxt-agent/internal/filesystem"
	"github.com/Yesveer/wxt-agent/internal/grpc"
	"github.com/Yesveer/wxt-agent/internal/monitor"
	"github.com/Yesveer/wxt-agent/internal/portforward"
	"github.com/Yesveer/wxt-agent/internal/pty"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol"
	"github.com/Yesveer/wxt-agent/internal/tunnel"
	"github.com/Yesveer/wxt-agent/internal/vscode"
	agentv1 "github.com/Yesveer/wxt-agent/proto/agent/v1"
	commonv1 "github.com/Yesveer/wxt-agent/proto/common/v1"
	"go.uber.org/zap"
)

// BuildVersion is the agent's compiled version (set by main from the
// -X main.version ldflag). Reported to the backend in register metadata
// so the portal can show "current vs latest" and trigger self-update.
var BuildVersion = "dev"

// Agent represents the vsay agent
type Agent struct {
	config             *config.Config
	logger             *zap.Logger
	grpcClient         *grpc.Client
	grpcMu             sync.Mutex // protects grpcClient reads/writes across goroutines
	agentID            string
	executor           *executor.Executor
	monitor            *monitor.Monitor
	ptyHandler         *pty.Handler
	vscodeServer       *vscode.Server
	vscodeProcesses    map[string]*osExec.Cmd
	vscodeProcessesMux sync.Mutex
	fsOps              *filesystem.Operations
	portForward        *portforward.Manager
	remoteControl      *remotecontrol.Controller
	// certMu serialises all cert and CA file operations.
	// Prevents concurrent CA rotation paths (gRPC in-band + polling recovery)
	// from racing on the same on-disk files.
	certMu sync.Mutex
}

// New creates a new agent
func New(cfg *config.Config, logger *zap.Logger) (*Agent, error) {
	// Validate configuration
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Create executor (will create user if needed)
	exec, err := executor.New(cfg.LinuxUser.Username, cfg.Permissions.AllowSudo, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create executor: %w", err)
	}

	// Create monitor
	mon := monitor.New(logger)

	// Create PTY handler
	ptyHandler := pty.NewHandler(logger)

	// Create filesystem operations handler
	fsOps := filesystem.NewOperations(logger)

	agent := &Agent{
		config:          cfg,
		logger:          logger,
		executor:        exec,
		monitor:         mon,
		ptyHandler:      ptyHandler,
		vscodeProcesses: make(map[string]*osExec.Cmd),
		fsOps:           fsOps,
	}

	// Create port forwarding manager with send function
	agent.portForward = portforward.NewManager(logger, func(sessionID string, data []byte) error {
		if agent.grpcClient != nil {
			return agent.grpcClient.SendTerminalOutput(sessionID, data)
		}
		return fmt.Errorf("grpc client not connected")
	})

	// Remote control (AnyDesk-style live session takeover). The controller only
	// bridges to the session helper — nothing is launched until the backend
	// actually asks for a session.
	agent.remoteControl = remotecontrol.New("", logger, func(sessionID string, data []byte) error {
		if agent.grpcClient != nil {
			return agent.grpcClient.SendTerminalOutput(sessionID, data)
		}
		return fmt.Errorf("grpc client not connected")
	})

	return agent, nil
}

// Start starts the agent.
func (a *Agent) Start(ctx context.Context) error {
	a.logger.Info("Agent starting",
		zap.String("linux_user", a.config.LinuxUser.Username),
		zap.String("server", a.config.Server.Host),
	)

	// One-time migration: encrypt any leftover plaintext .pem files from older agent versions.
	a.migrateToEncryptedCerts()

	// On Windows, ensure the remote-desktop backend is up (RDP on Pro, VNC on Home) so
	// the portal's Desktop feature can tunnel to it. No-op on Linux. Best-effort.
	a.ensureRemoteDesktop()

	// The session helper must never outlive the agent: an orphaned helper would
	// leave the user's screen capturable with nobody supervising it.
	go func() {
		<-ctx.Done()
		if err := a.remoteControl.Close(); err != nil {
			a.logger.Warn("Failed to stop the session helper", zap.Error(err))
		}
	}()

	// caFingerprintLoop runs for the full agent lifetime. It uses recoveryHTTPClient
	// (InsecureSkipVerify) so it works even when the pinned CA is stale, and checks
	// immediately on startup to catch any CA rotation that happened while offline.
	go a.caFingerprintLoop(ctx)

	// tunnelOnce ensures the tunnel goroutine is started only once,
	// after agentID is known from the first successful registration.
	var tunnelOnce sync.Once

	// Reconnection loop with exponential backoff
	retryDelay := 5 * time.Second
	maxRetryDelay := 60 * time.Second

	// lastCertCheck tracks when we last ran checkAndRenewCert. Zero value ensures
	// a check runs before the very first connection attempt.
	var lastCertCheck time.Time

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("Context canceled, stopping agent")
			if a.grpcClient != nil {
				if err := a.grpcClient.Close(); err != nil {
					a.logger.Warn("Failed to close gRPC client", zap.Error(err))
				}
			}
			return nil
		default:
		}

		// Pre-connect cert check: runs on startup and at most once per hour during
		// the retry loop. Cert operations are intentionally skipped while the agent
		// sits idle offline — they only happen when a connection is imminent.
		if time.Since(lastCertCheck) > time.Hour {
			a.checkAndRenewCert(ctx)
			lastCertCheck = time.Now()
		}

		// Connect to backend
		if err := a.connect(ctx); err != nil {
			a.logger.Error("Failed to connect, retrying...", zap.Error(err), zap.Duration("retry_in", retryDelay))

			// TLS authentication failure usually means the CA has rotated (backend redeployed).
			// Check immediately — don't wait 30 min for the next polling tick.
			if isTLSAuthError(err) {
				a.logger.Warn("TLS handshake failed — checking for CA rotation now")
				a.checkCAFingerprint(ctx)
				// Force a cert re-check on the next connect attempt so we pick up the new CA.
				lastCertCheck = time.Time{}
			}

			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}

		// Register with backend
		if err := a.register(ctx); err != nil {
			a.logger.Error("Failed to register, retrying...", zap.Error(err), zap.Duration("retry_in", retryDelay))
			if a.grpcClient != nil {
				if cerr := a.grpcClient.Close(); cerr != nil {
					a.logger.Warn("Failed to close gRPC client", zap.Error(cerr))
				}
				a.grpcClient = nil
			}
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}

		// Reset retry delay on successful connection
		retryDelay = 5 * time.Second
		a.logger.Info("Agent connected and registered successfully", zap.String("agent_id", a.agentID))

		// Start tunnel client once, after agentID is known from registration
		if a.config.Tunnel.Enabled && a.config.Tunnel.URL != "" {
			tunnelOnce.Do(func() {
				pollInterval := 10 * time.Second
				if a.config.Tunnel.PollInterval != "" {
					if d, err := time.ParseDuration(a.config.Tunnel.PollInterval); err == nil {
						pollInterval = d
					}
				}
				tc := tunnel.New(a.config.Tunnel.URL, a.agentID, pollInterval, a.logger)
				go tc.Start(ctx)
				a.logger.Info("Tunnel client started",
					zap.String("tunnel_url", a.config.Tunnel.URL),
					zap.String("machine_id", a.agentID))
			})
		}

		// heartbeatLoop and certRenewalLoop are scoped to the connection lifecycle.
		// certRenewalLoop must NOT run while offline — cert operations require a working
		// HTTPS channel to the backend. The pre-connect check above covers the offline case.
		heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
		go a.heartbeatLoop(heartbeatCtx)
		go a.certRenewalLoop(heartbeatCtx)
		// Watch for external SSH/RDP logins and report them to the backend.
		go a.startSessionMonitor(heartbeatCtx)
		// Report the agent process's own resource usage for the Agent Monitoring UI.
		go a.agentStatsLoop(heartbeatCtx)

		// Start gRPC stream (blocks until disconnection)
		streamErr := a.streamLoop(ctx)
		heartbeatCancel()

		// Close gRPC client after disconnection
		if a.grpcClient != nil {
			if err := a.grpcClient.Close(); err != nil {
				a.logger.Warn("Failed to close gRPC client", zap.Error(err))
			}
			a.grpcClient = nil
		}

		// On any disconnect (clean or error), force a cert check before the next
		// connect attempt. The cert may have expired or the CA may have rotated
		// while we were connected and missed the in-band gRPC notification.
		lastCertCheck = time.Time{}

		if streamErr != nil {
			a.logger.Warn("Stream disconnected, will reconnect...", zap.Error(streamErr), zap.Duration("retry_in", retryDelay))
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}

		// Stream closed cleanly — still reconnect
		a.logger.Info("Stream closed, reconnecting...", zap.Duration("retry_in", retryDelay))
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, maxRetryDelay)
	}
}

// isTLSAuthError returns true when the error is an mTLS/x509 authentication failure
// (wrong CA, expired cert, cert signed by unknown authority, handshake failure).
// This distinguishes cert problems from transient network errors.
func isTLSAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// "tls" alone is intentionally excluded — it matches "mTLS" in our own error
	// prefix ("mTLS connect failed: ...") causing false-positive CA rotation checks.
	return strings.Contains(msg, "x509") ||
		strings.Contains(msg, "certificate") ||
		strings.Contains(msg, "handshake") ||
		strings.Contains(msg, "tls handshake")
}

// extractHostname returns the hostname portion of a "host:port" string.
func extractHostname(hostPort string) string {
	hostPort = strings.TrimPrefix(hostPort, "https://")
	hostPort = strings.TrimPrefix(hostPort, "http://")
	if idx := strings.LastIndex(hostPort, ":"); idx != -1 {
		return hostPort[:idx]
	}
	return hostPort
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// mtlsCertFile, mtlsKeyFile, serverCACert and the legacy plaintext paths are defined
// per-OS in certpaths_unix.go / certpaths_windows.go so the agent stores certs under
// the platform-appropriate directory (/etc/vsay/certs on Unix, C:\ProgramData\vsay\certs
// on Windows). Values on Unix are unchanged from before this split.

// closeGRPC safely closes the gRPC client under grpcMu. Safe to call from any goroutine.
func (a *Agent) closeGRPC() {
	a.grpcMu.Lock()
	defer a.grpcMu.Unlock()
	if a.grpcClient != nil {
		if err := a.grpcClient.Close(); err != nil {
			a.logger.Warn("Failed to close gRPC client", zap.Error(err))
		}
	}
}

// connect establishes a gRPC connection using mTLS with full server certificate verification.
//
// ServerName is set to the hostname in GRPCURL so the TLS layer rejects any cert
// that does not have SERVER_DOMAIN in its Subject Alternative Names.
func (a *Agent) connect(ctx context.Context) error {
	a.logger.Info("Connecting to backend", zap.String("url", a.config.Server.GRPCURL))

	token := a.config.Server.TokenHash
	caCertFile := a.config.Server.CACertFile

	// Prefer explicit CA cert → fall back to well-known path → empty (system CAs)
	if caCertFile == "" {
		if _, err := os.Stat(serverCACert); err == nil {
			caCertFile = serverCACert
		}
	}

	// ServerName must match the SAN in the server cert (SERVER_DOMAIN).
	// This prevents connecting to a server with a valid-but-wrong cert.
	serverName := extractHostname(a.config.Server.GRPCURL)

	// Decrypt cert+key in memory — plaintext key never sits on disk.
	tlsCert, err := LoadEncryptedKeyPair(mtlsCertFile, mtlsKeyFile)
	if err != nil {
		return fmt.Errorf("mTLS connect failed — load encrypted cert: %w", err)
	}

	client, err := grpc.NewClientMTLSCert(
		a.config.Server.GRPCURL, token,
		tlsCert, caCertFile, serverName,
		a.logger,
	)
	if err != nil {
		return fmt.Errorf("mTLS connect failed: %w", err)
	}

	a.grpcClient = client
	a.logger.Info("Connected to backend via mTLS", zap.String("server_name", serverName))
	return nil
}

// register registers agent with backend
func (a *Agent) register(ctx context.Context) error {
	a.logger.Info("Registering agent")

	// Get Linux user info
	linuxUser, err := a.getLinuxUserInfo()
	if err != nil {
		return fmt.Errorf("failed to get Linux user info: %w", err)
	}

	// Get hostname
	hostname, _ := os.Hostname()

	// Get IP address
	ipAddress, err := monitor.GetIPAddress()
	if err != nil {
		a.logger.Warn("Failed to get IP address", zap.Error(err))
		ipAddress = "127.0.0.1"
	}

	// Determine machine name (use configured name or fallback to hostname)
	machineName := a.config.Agent.Name
	if machineName == "" {
		machineName = hostname
	}

	// Create registration request
	req := &agentv1.RegisterRequest{
		Token:     a.config.Server.TokenHash, // TODO: Use actual token
		Hostname:  hostname,
		OsInfo:    a.getOSInfo(),
		IpAddress: ipAddress,
		LinuxUser: linuxUser,
		Metadata: map[string]string{
			"tenant_id":    a.config.Tenant.ID,
			"org_id":       a.config.Org.ID,
			"project_id":   a.config.Project.ID,
			"user_email":   a.config.PortalUser.Email,
			"machine_name": machineName,
			"version":      BuildVersion,
		},
	}

	// Hardware totals so the portal can show absolute usage (cores / GB) from the
	// reported percentages.
	cores, memMB, diskGB := a.monitor.GetMachineTotals()
	req.Metadata["cpu_cores"] = fmt.Sprintf("%d", cores)
	req.Metadata["mem_total_mb"] = fmt.Sprintf("%d", memMB)
	req.Metadata["disk_total_gb"] = fmt.Sprintf("%d", diskGB)

	// Advertise the browser remote-desktop protocol (Windows only): "rdp" on
	// Pro/Enterprise, "vnc" on Home. Empty on Linux (terminal, not desktop). For RDP we
	// also report the auto-detected Windows account so the portal pre-fills the username
	// and only asks the operator for their password at connect time.
	if mode := remoteDesktopMode(); mode != "" {
		req.Metadata["remote_desktop"] = mode
		if mode == "rdp" {
			req.Metadata["remote_desktop_user"] = a.config.LinuxUser.Username
		}
	}

	resp, err := a.grpcClient.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	a.agentID = resp.AgentId
	a.logger.Info("Agent registered",
		zap.String("agent_id", resp.AgentId),
		zap.Bool("approved", resp.Approved),
	)

	if !resp.Approved {
		a.logger.Warn("Agent registration pending approval")
	}

	return nil
}

// agentStatsLoop periodically reports the AGENT process's own resource usage (CPU,
// memory, goroutines, uptime, version) to the backend so the portal's Agent Monitoring
// section can show it. Sent as a StatusUpdate("__agent_stats__", json) to avoid a proto
// change. Runs for the connection lifetime.
func (a *Agent) agentStatsLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	send := func() {
		st := a.monitor.GetAgentStats()
		openTunnels := 0
		if a.portForward != nil {
			openTunnels = a.portForward.ActiveCount()
		}
		openSessions := 0
		if a.ptyHandler != nil {
			openSessions = a.ptyHandler.Count()
		}
		msg, _ := json.Marshal(map[string]interface{}{
			"cpu_percent":   st.CPUPercent,
			"memory_mb":     st.MemoryMB,
			"goroutines":    st.Goroutines,
			"uptime_sec":    st.UptimeSec,
			"version":       BuildVersion,
			"open_tunnels":  openTunnels,
			"open_sessions": openSessions,
		})
		if err := a.grpcClient.SendStatusUpdate("__agent_stats__", string(msg)); err != nil {
			a.logger.Debug("agent stats: send failed", zap.Error(err))
		}
	}

	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// heartbeatLoop sends periodic heartbeats
func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendHeartbeat(ctx); err != nil {
				a.logger.Error("Failed to send heartbeat", zap.Error(err))
			}
		}
	}
}

// certRenewalLoop proactively renews the client cert while the agent is connected.
// It runs only for the lifetime of a connection (scoped to heartbeatCtx in Start).
// Pre-connection cert checks are handled separately in the Start reconnect loop.
func (a *Agent) certRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkAndRenewCert(ctx)
		}
	}
}

// certRenewThreshold is how far before expiry we renew the client cert.
// Set to 6h (25% of the 1-day validity): gives 6 hourly retry attempts before
// the cert actually expires, without renewing so early that keys barely rotate.
const certRenewThreshold = 6 * time.Hour

// checkAndRenewCert renews the client cert when it is within certRenewThreshold
// of expiry, or when the cert file is missing/corrupt (treated as expired).
// If renewal fails with a TLS/x509 error it triggers an immediate CA fingerprint
// check, since that failure most likely means the CA rotated while offline.
func (a *Agent) checkAndRenewCert(ctx context.Context) {
	a.certMu.Lock()
	defer a.certMu.Unlock()

	needsRenewal, hoursLeft := a.certNeedsRenewal()
	if !needsRenewal {
		return
	}

	if hoursLeft <= 0 {
		a.logger.Warn("Client cert expired or missing/corrupt — renewing now", zap.Float64("hours_left", hoursLeft))
	} else {
		a.logger.Info("Client cert expires soon, renewing", zap.Float64("hours_left", hoursLeft))
	}

	if err := a.renewClientCertLocked(); err != nil {
		a.logger.Error("Failed to renew client cert", zap.Error(err))
		if isTLSAuthError(err) {
			a.logger.Warn("Cert renewal failed due to TLS error — CA may have rotated, checking fingerprint")
			// Release lock before calling checkCAFingerprint (which acquires it internally).
			a.certMu.Unlock()
			a.checkCAFingerprint(ctx)
			a.certMu.Lock()
		}
		return
	}
	a.logger.Info("Client cert renewed successfully")
	a.closeGRPC()
}

// certNeedsRenewal reports whether the on-disk cert needs renewal and how many
// hours are left. Returns (true, 0) when the file is missing, corrupt, or expired.
func (a *Agent) certNeedsRenewal() (bool, float64) {
	encBytes, err := os.ReadFile(mtlsCertFile)
	if err != nil {
		// Missing cert file — must renew.
		a.logger.Warn("Client cert file missing — will attempt renewal", zap.Error(err))
		return true, 0
	}
	certPEM, err := DecryptPEM(encBytes)
	if err != nil {
		// Corrupt or wrong-machine encrypted file — must renew.
		a.logger.Warn("Client cert file corrupt/undecryptable — will attempt renewal", zap.Error(err))
		return true, 0
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		a.logger.Warn("Client cert file has no valid PEM block — will attempt renewal")
		return true, 0
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		a.logger.Warn("Client cert file unparseable — will attempt renewal", zap.Error(err))
		return true, 0
	}

	hoursLeft := time.Until(cert.NotAfter).Hours()
	a.logger.Info("Client cert expiry check", zap.Float64("hours_left", hoursLeft))

	return hoursLeft <= certRenewThreshold.Hours(), hoursLeft
}

// renewClientCertLocked requests a fresh cert from the backend and atomically
// replaces the on-disk key+cert pair. certMu must be held by the caller.
// Uses pinnedCAHTTPClient — verifies the backend's TLS cert against the pinned CA.
func (a *Agent) renewClientCertLocked() error {
	return a.doRenewClientCert(a.pinnedCAHTTPClient())
}

// doRenewClientCert generates a new ECDSA P-256 CSR, sends it to /agent/sign-cert,
// validates the returned cert, and atomically replaces the on-disk cert + key.
// certMu must be held by the caller.
func (a *Agent) doRenewClientCert(httpClient *http.Client) error {
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

	host := strings.TrimSuffix(a.config.Server.Host, "/")
	req, err := http.NewRequest("POST", host+"/agent/sign-cert", bytes.NewReader(csrPEM))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.config.Server.TokenHash)
	req.Header.Set("Content-Type", "application/x-pem-file")

	// Send current cert fingerprint so backend can verify token-cert binding.
	// If this is the first signing the file won't exist — omit the header.
	if currentEnc, err := os.ReadFile(mtlsCertFile); err == nil {
		if currentPEM, err := DecryptPEM(currentEnc); err == nil {
			if blk, _ := pem.Decode(currentPEM); blk != nil {
				if current, err := x509.ParseCertificate(blk.Bytes); err == nil {
					sum := sha256.Sum256(current.Raw)
					req.Header.Set("X-Cert-Fingerprint", hex.EncodeToString(sum[:]))
				}
			}
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sign-cert request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	signedCertPEM, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return fmt.Errorf("read signed cert: %w", err)
	}

	// Validate: signed cert must be parseable and its public key must match our private key.
	// This ensures the backend returned a cert for the CSR we actually sent, not garbage.
	blk, _ := pem.Decode(signedCertPEM)
	if blk == nil {
		return fmt.Errorf("backend returned invalid PEM cert")
	}
	signedCert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return fmt.Errorf("parse signed cert: %w", err)
	}
	certPub, ok := signedCert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certPub.Equal(key.Public()) {
		return fmt.Errorf("signed cert public key does not match our private key — backend returned wrong cert")
	}

	// Encrypt key and cert in memory — plaintext key never persists on disk.
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	encKey, err := EncryptPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("encrypt key: %w", err)
	}
	encCert, err := EncryptPEM(signedCertPEM)
	if err != nil {
		return fmt.Errorf("encrypt cert: %w", err)
	}

	// Atomic write. Cert is renamed first so that if we crash between the two
	// renames, the agent loads (new cert + old key) which fails cleanly rather
	// than silently using a mismatched pair.
	// mtlsCertFile/mtlsKeyFile are fixed package-level constants, not request input.
	if err := os.WriteFile(mtlsCertFile+".new", encCert, 0600); err != nil { // #nosec G703
		return fmt.Errorf("write new cert: %w", err)
	}
	if err := os.WriteFile(mtlsKeyFile+".new", encKey, 0600); err != nil { // #nosec G703
		a.removeQuiet(mtlsCertFile + ".new")
		return fmt.Errorf("write new key: %w", err)
	}
	if err := os.Rename(mtlsCertFile+".new", mtlsCertFile); err != nil {
		a.removeQuiet(mtlsCertFile + ".new")
		a.removeQuiet(mtlsKeyFile + ".new")
		return fmt.Errorf("install new cert: %w", err)
	}
	if err := os.Rename(mtlsKeyFile+".new", mtlsKeyFile); err != nil {
		a.removeQuiet(mtlsKeyFile + ".new")
		return fmt.Errorf("install new key: %w", err)
	}

	return nil
}

// migrateToEncryptedCerts is a one-time, idempotent migration that runs at agent startup.
// If the old plaintext .pem files exist and the new .enc files do not, it encrypts and
// moves them — so agents that auto-update don't need manual intervention.
// removeQuiet best-effort deletes a rollback/temp file, logging (not failing)
// on error — callers use this purely for cleanup after an earlier failure.
func (a *Agent) removeQuiet(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		a.logger.Warn("Failed to remove file during rollback", zap.String("path", path), zap.Error(err))
	}
}

func (a *Agent) migrateToEncryptedCerts() {
	// Already migrated (or first install with no certs yet) — nothing to do.
	if _, err := os.Stat(mtlsCertFile); err == nil {
		return
	}
	if _, err := os.Stat(mtlsCertFileLegacy); err != nil {
		return // No plaintext files either — first install
	}

	a.logger.Info("Migrating plaintext cert/key to encrypted storage")

	certPEM, err := os.ReadFile(mtlsCertFileLegacy)
	if err != nil {
		a.logger.Error("Migration: failed to read legacy cert", zap.Error(err))
		return
	}
	keyPEM, err := os.ReadFile(mtlsKeyFileLegacy)
	if err != nil {
		a.logger.Error("Migration: failed to read legacy key", zap.Error(err))
		return
	}

	encCert, err := EncryptPEM(certPEM)
	if err != nil {
		a.logger.Error("Migration: failed to encrypt cert", zap.Error(err))
		return
	}
	encKey, err := EncryptPEM(keyPEM)
	if err != nil {
		a.logger.Error("Migration: failed to encrypt key", zap.Error(err))
		return
	}

	// mtlsCertFile/mtlsKeyFile are fixed package-level constants, not request
	// input.
	if err := os.WriteFile(mtlsCertFile, encCert, 0600); err != nil { // #nosec G703
		a.logger.Error("Migration: failed to write encrypted cert", zap.Error(err))
		return
	}
	if err := os.WriteFile(mtlsKeyFile, encKey, 0600); err != nil { // #nosec G703
		a.logger.Error("Migration: failed to write encrypted key", zap.Error(err))
		if rerr := os.Remove(mtlsCertFile); rerr != nil {
			a.logger.Warn("Migration: failed to roll back encrypted cert", zap.Error(rerr))
		}
		return
	}

	// Remove plaintext files only after both encrypted files are safely written.
	if err := os.Remove(mtlsCertFileLegacy); err != nil {
		a.logger.Warn("Migration: failed to remove legacy cert", zap.Error(err))
	}
	if err := os.Remove(mtlsKeyFileLegacy); err != nil {
		a.logger.Warn("Migration: failed to remove legacy key", zap.Error(err))
	}
	a.logger.Info("Migration complete: cert and key now encrypted on disk")
}

// caFingerprintFile is defined per-OS in certpaths_unix.go / certpaths_windows.go so
// the CA-rotation fingerprint persists under the platform-appropriate directory.
// Without a writable path the fingerprint never saves and rotation re-triggers every
// poll — causing the agent to re-sign its cert in a loop (the Windows bug).

// caFingerprintLoop polls the backend every 30 min and triggers CA rotation when the fingerprint changes.
func (a *Agent) caFingerprintLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	// Check immediately on start — catches CA rotation that happened while agent was offline.
	a.checkCAFingerprint(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkCAFingerprint(ctx)
		}
	}
}

// pinnedCAHTTPClient returns an HTTP client for backend HTTP calls (cert signing, CA
// fetch, etc.). It trusts BOTH the system CA pool AND the pinned private CA, so it
// works whether the HTTP API endpoint (e.g. api.webxterm.me) presents a PUBLIC cert
// (Let's Encrypt via a load balancer — common when the backend runs with TLS_ENABLED
// =false behind an LB) or the backend's own PRIVATE-CA cert (self-hosted TLS).
//
// This matters because the HTTP API host and the gRPC host can be terminated
// differently: gRPC (mTLS) always uses the private CA, but the public HTTP endpoint is
// frequently fronted by an LB with a real cert. Pinning ONLY the private CA there
// caused "x509: certificate signed by unknown authority" on cert renewal while gRPC
// kept working.
func (a *Agent) pinnedCAHTTPClient() *http.Client {
	// Start from the system trust store (public CAs like Let's Encrypt).
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	// Also trust the pinned private CA, if configured, so self-hosted (self-signed)
	// deployments keep working too.
	caCertFile := a.config.Server.CACertFile
	if caCertFile == "" {
		if _, err := os.Stat(serverCACert); err == nil {
			caCertFile = serverCACert
		}
	}
	if caCertFile != "" {
		// caCertFile is agent config (CLI flag / config file) or the fixed
		// serverCACert constant, not request input.
		if caPEM, err := os.ReadFile(caCertFile); err == nil { // #nosec G304
			pool.AppendCertsFromPEM(caPEM)
		}
	}

	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// recoveryHTTPClient returns an HTTP client that skips TLS verification.
// Used ONLY as a last-resort fallback when the agent has been offline so long
// that the CA grace period expired and it can no longer verify the server cert.
// Security note: these endpoints auth via Bearer token. A MITM can only cause
// DoS (wrong CA → gRPC fails) — they cannot gain access because operations go
// through mTLS gRPC which is fully verified once the agent re-signs.
func recoveryHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- CA bootstrap fallback, see doc comment above
		},
	}
}

// handleGRPCCARotation processes a CA rotation notification received over the
// existing secure mTLS gRPC channel. Because the message comes from a verified
// backend over a verified channel, NO InsecureSkipVerify is needed here.
//
// Flow:
//  1. Validate and save the new CA cert (received in the gRPC message body)
//  2. Renew client cert via pinnedCAHTTPClient with NEW CA (server cert already updated)
//  3. Save new fingerprint
//  4. Close gRPC connection → reconnect loop picks it up with fresh certs
func (a *Agent) handleGRPCCARotation(newCAPEM string) {
	a.logger.Info("Received CA rotation notification via gRPC (secure in-band delivery)")

	// Validate the received PEM before touching any files.
	block, _ := pem.Decode([]byte(newCAPEM))
	if block == nil {
		a.logger.Error("CA rotation: invalid PEM received")
		return
	}
	newCA, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		a.logger.Error("CA rotation: failed to parse cert", zap.Error(err))
		return
	}
	sum := sha256.Sum256(newCA.Raw)
	fingerprint := hex.EncodeToString(sum[:])
	a.logger.Info("New CA fingerprint", zap.String("sha256", fingerprint),
		zap.String("valid_until", newCA.NotAfter.Format("2006-01-02")))

	a.certMu.Lock()
	defer a.certMu.Unlock()

	caCertPath := serverCACert
	if a.config.Server.CACertFile != "" {
		caCertPath = a.config.Server.CACertFile
	}
	// caCertPath is the fixed serverCACert constant or agent config, not
	// request input; a CA cert is public trust material, so 0644 is intentional.
	if err := os.WriteFile(caCertPath+".new", []byte(newCAPEM), 0644); err != nil { // #nosec G306,G304
		a.logger.Error("CA rotation: failed to write new CA cert", zap.Error(err))
		return
	}
	if err := os.Rename(caCertPath+".new", caCertPath); err != nil {
		a.removeQuiet(caCertPath + ".new")
		a.logger.Error("CA rotation: failed to install new CA cert", zap.Error(err))
		return
	}
	a.logger.Info("New CA cert saved", zap.String("path", caCertPath))

	// pinnedCAHTTPClient now reads the new CA from disk, so this is fully verified.
	if err := a.renewClientCertLocked(); err != nil {
		a.logger.Error("CA rotation: failed to renew client cert", zap.Error(err))
		return
	}

	a.persistCAFingerprint(fingerprint)

	a.logger.Info("CA rotation via gRPC complete — closing connection to reconnect with new certs")
	a.closeGRPC()
}

// persistCAFingerprint saves the CA fingerprint to disk so CA-rotation detection is
// stable across reconnects. It creates the parent directory first — without this, a
// missing certs dir made the write fail on Windows, so the fingerprint never persisted
// and every reconnect re-triggered rotation (re-signing the cert in a loop).
func (a *Agent) persistCAFingerprint(fingerprint string) {
	// caFingerprintFile lives under /etc/vsay/certs — owner-only, same as the
	// rest of that directory.
	if err := os.MkdirAll(filepath.Dir(caFingerprintFile), 0o750); err != nil {
		a.logger.Warn("Could not create cert dir for CA fingerprint", zap.Error(err))
	}
	// A fingerprint hash is not secret — it's meant for out-of-band comparison.
	if err := os.WriteFile(caFingerprintFile, []byte(fingerprint), 0o644); err != nil { // #nosec G306
		a.logger.Error("Failed to persist CA fingerprint — rotation may re-trigger on next poll", zap.Error(err))
		return
	}
	a.logger.Info("CA fingerprint persisted", zap.String("path", caFingerprintFile))
}

// checkCAFingerprint fetches the current CA fingerprint from the backend.
// If it differs from the locally saved value, it triggers CA rotation.
//
// Uses recoveryHTTPClient so it succeeds even when the CA has already rotated
// and the agent's pinned CA no longer trusts the server's new TLS cert.
func (a *Agent) checkCAFingerprint(ctx context.Context) {
	host := strings.TrimSuffix(a.config.Server.Host, "/")
	resp, err := recoveryHTTPClient().Get(host + "/ca-fingerprint")
	if err != nil {
		a.logger.Warn("CA fingerprint check failed", zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Fingerprint == "" {
		return
	}

	saved, _ := os.ReadFile(caFingerprintFile)
	if strings.TrimSpace(string(saved)) == body.Fingerprint {
		return // No change
	}

	a.logger.Warn("CA fingerprint changed — rotating local CA cert", zap.String("new_fp", body.Fingerprint))
	if err := a.handleCARotation(ctx, body.Fingerprint); err != nil {
		a.logger.Error("CA rotation handling failed", zap.Error(err))
	}
}

// handleCARotation is the OFFLINE RECOVERY path — only reached when the agent
// missed the in-band gRPC notification (was offline past the grace period).
//
// Security model:
//   - CA cert download uses InsecureSkipVerify (old CA is stale so can't verify)
//   - Downloaded cert is cross-checked: its SHA-256 fingerprint must match the
//     fingerprint returned by /ca-fingerprint (fetched in checkCAFingerprint).
//     A MITM can serve a fake cert body but cannot forge the hash that the
//     server reported over a separate request.
//   - After saving the new CA, client cert renewal uses pinnedCAHTTPClient
//     (verified TLS) — NOT InsecureSkipVerify — because the new CA is now on disk.
func (a *Agent) handleCARotation(ctx context.Context, newFingerprint string) error {
	host := strings.TrimSuffix(a.config.Server.Host, "/")

	// Download the new CA cert (InsecureSkipVerify — old CA is stale).
	newCAPEM, err := func() ([]byte, error) {
		resp, err := recoveryHTTPClient().Get(host + "/ca-cert")
		if err != nil {
			return nil, fmt.Errorf("fetch new CA cert: %w", err)
		}
		defer resp.Body.Close()
		return io.ReadAll(io.LimitReader(resp.Body, 8192))
	}()
	if err != nil {
		return err
	}

	// Validate: fingerprint of downloaded cert must match server-reported fingerprint.
	// This catches a MITM that intercepts the body but can't forge the separate hash.
	block, _ := pem.Decode(newCAPEM)
	if block == nil {
		return fmt.Errorf("invalid CA PEM received")
	}
	parsedCA, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse downloaded CA cert: %w", err)
	}
	s := sha256.Sum256(parsedCA.Raw)
	downloadedFP := hex.EncodeToString(s[:])
	if downloadedFP != newFingerprint {
		return fmt.Errorf("CA cert fingerprint mismatch (MITM suspected): got %s, expected %s",
			downloadedFP, newFingerprint)
	}

	a.certMu.Lock()
	defer a.certMu.Unlock()

	caCertPath := serverCACert
	if a.config.Server.CACertFile != "" {
		caCertPath = a.config.Server.CACertFile
	}
	// caCertPath is the fixed serverCACert constant or agent config, not
	// request input; a CA cert is public trust material, so 0644 is intentional.
	if err := os.WriteFile(caCertPath+".new", newCAPEM, 0644); err != nil { // #nosec G306,G304
		return fmt.Errorf("write new CA cert: %w", err)
	}
	if err := os.Rename(caCertPath+".new", caCertPath); err != nil {
		a.removeQuiet(caCertPath + ".new")
		return fmt.Errorf("install new CA cert: %w", err)
	}
	a.logger.Info("New CA cert saved (recovery path)", zap.String("path", caCertPath))

	// pinnedCAHTTPClient now reads the freshly saved CA — fully verified, no InsecureSkipVerify.
	if err := a.renewClientCertLocked(); err != nil {
		return fmt.Errorf("re-sign client cert: %w", err)
	}

	a.persistCAFingerprint(newFingerprint)

	a.logger.Info("CA rotation (recovery) complete — closing connection to reconnect with new certs")
	a.closeGRPC()
	return nil
}

// sendHeartbeat sends heartbeat to backend
func (a *Agent) sendHeartbeat(ctx context.Context) error {
	a.grpcMu.Lock()
	client := a.grpcClient
	a.grpcMu.Unlock()
	if client == nil {
		return nil
	}

	stats, err := a.getResourceStats()
	if err != nil {
		a.logger.Warn("Failed to get resource stats", zap.Error(err))
		stats = &commonv1.ResourceStats{}
	}

	now := time.Now()
	timestamp := &commonv1.Timestamp{
		Seconds: now.Unix(),
		Nanos:   int32(now.Nanosecond()), // #nosec G115 -- time.Nanosecond() is always 0-999999999, well within int32
	}
	if err := client.SendHeartbeatMessage(timestamp, stats); err != nil {
		return client.SendHeartbeat(ctx, stats)
	}
	return nil
}

// streamLoop maintains bidirectional stream with backend
func (a *Agent) streamLoop(ctx context.Context) error {
	a.logger.Info("Starting gRPC stream")

	// Start bidirectional stream
	if err := a.grpcClient.StartStream(ctx, a.handleServerMessage); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}

	// Keep stream alive
	<-ctx.Done()
	return nil
}

// handleServerMessage handles messages from backend
func (a *Agent) handleServerMessage(msg *agentv1.ServerMessage) error {
	switch payload := msg.Payload.(type) {
	case *agentv1.ServerMessage_Command:
		return a.handleCommand(payload.Command)
	case *agentv1.ServerMessage_TerminalInput:
		return a.handleTerminalInput(payload.TerminalInput)
	case *agentv1.ServerMessage_Config:
		return a.handleConfigUpdate(payload.Config)
	case *agentv1.ServerMessage_Approval:
		return a.handleApproval(payload.Approval)
	default:
		a.logger.Warn("Unknown message type", zap.Any("payload", payload))
		return nil
	}
}

// handleCommand executes a command
func (a *Agent) handleCommand(cmd *agentv1.CommandRequest) error {
	// "__vsay_init__" is a special cloud-init style script sent by the backend
	// on the machine's very first registration. Run it, write output to ~/script.log,
	// then notify the backend so it can flip status to "online".
	if cmd.CommandId == "__vsay_init__" {
		go a.runInitScript(cmd.Command)
		return nil
	}

	// "__vsay_ca_rotation__" delivers the new CA cert PEM over the existing secure
	// mTLS gRPC channel. Handling it here avoids InsecureSkipVerify entirely for
	// agents that are online during the rotation. cmd.Command contains the CA PEM.
	if cmd.CommandId == "__vsay_ca_rotation__" {
		go a.handleGRPCCARotation(cmd.Command)
		return nil
	}

	// "__vsay_update__" is a self-update trigger from the portal. cmd.Command is
	// the download URL of the new agent package. The agent downloads it, hot-swaps
	// its own binary, and restarts the service. Runs in its own goroutine with a
	// panic guard so a failed update can never crash the running agent.
	if cmd.CommandId == "__vsay_update__" {
		go a.handleSelfUpdate(cmd.Command)
		return nil
	}

	a.logger.Info("Received command",
		zap.String("command_id", cmd.CommandId),
		zap.String("command", cmd.Command),
	)

	// Execute command as configured Linux user
	go func() {
		result, err := a.executor.Execute(cmd.Command, cmd.Env)
		if err != nil {
			a.logger.Error("Command execution failed",
				zap.String("command_id", cmd.CommandId),
				zap.Error(err),
			)
			// Send error as stderr
			stderr := []byte(fmt.Sprintf("Execution error: %v", err))
			if serr := a.grpcClient.SendCommandOutput(cmd.CommandId, nil, stderr, 1, true); serr != nil {
				a.logger.Warn("Failed to send command error output", zap.String("command_id", cmd.CommandId), zap.Error(serr))
			}
			return
		}

		// Send command output
		if err := a.grpcClient.SendCommandOutput(
			cmd.CommandId,
			result.Stdout,
			result.Stderr,
			result.ExitCode,
			true,
		); err != nil {
			a.logger.Error("Failed to send command output",
				zap.String("command_id", cmd.CommandId),
				zap.Error(err),
			)
		}
	}()

	return nil
}

// runInitScript runs the cloud-init style script on first registration.
// Output (stdout + stderr) is written to ~/script.log under the configured
// Linux user's home directory. The backend is notified via StatusUpdate so
// it can flip the machine status from "executing_script" → "online".
func (a *Agent) runInitScript(script string) {
	a.logger.Info("Running init script")

	homeDir := a.config.LinuxUser.HomeDir
	if homeDir == "" {
		homeDir = "/home/" + a.config.LinuxUser.Username
	}
	logPath := homeDir + "/script.log"

	sendStatus := func(status, message string) {
		if err := a.grpcClient.SendStatusUpdate(status, message); err != nil {
			a.logger.Warn("Failed to send status update", zap.String("status", status), zap.Error(err))
		}
	}

	// Write the script to a temp file so it can be executed as a proper shell script
	tmpFile, err := os.CreateTemp("", "vsay-init-*.sh")
	if err != nil {
		a.logger.Error("Failed to create temp script file", zap.Error(err))
		sendStatus("script_error", "failed to create temp file: "+err.Error())
		return
	}
	defer func() {
		if err := os.Remove(tmpFile.Name()); err != nil {
			a.logger.Warn("Failed to remove temp init script", zap.Error(err))
		}
	}()

	if _, err := tmpFile.WriteString("#!/bin/bash\nset -euo pipefail\n" + script + "\n"); err != nil {
		_ = tmpFile.Close()
		a.logger.Error("Failed to write init script", zap.Error(err))
		sendStatus("script_error", "failed to write script: "+err.Error())
		return
	}
	if err := tmpFile.Close(); err != nil {
		a.logger.Error("Failed to close temp init script", zap.Error(err))
		sendStatus("script_error", "failed to close temp script: "+err.Error())
		return
	}
	// The script is invoked as an explicit /bin/bash argument below (not
	// exec'd directly), so the execute bit isn't strictly required, but 0700
	// (owner-only, no world/group access) is the appropriate permission for
	// a script that may embed secrets from `script`.
	if err := os.Chmod(tmpFile.Name(), 0700); err != nil { // #nosec G302
		a.logger.Warn("Failed to chmod temp init script", zap.Error(err))
	}

	// Open / create ~/script.log. logPath is derived from this agent's own
	// configured Linux user, not request input. Kept owner-only (0600) since
	// the script's stdout/stderr may contain secrets it echoed.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) // #nosec G304
	if err != nil {
		a.logger.Warn("Cannot open script.log, falling back to /tmp/vsay-script.log", zap.Error(err))
		logFile, err = os.OpenFile("/tmp/vsay-script.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) // #nosec G304
		if err != nil {
			sendStatus("script_error", "cannot open log file: "+err.Error())
			return
		}
		logPath = "/tmp/vsay-script.log"
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			a.logger.Warn("Failed to close script.log", zap.Error(err))
		}
	}()

	fmt.Fprintf(logFile, "=== vsay init script started at %s ===\n\n", time.Now().Format(time.RFC3339))

	// tmpFile.Name() is our own just-created temp file, not request input.
	cmd := osExec.Command("/bin/bash", tmpFile.Name()) // #nosec G204
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()

	if runErr != nil {
		fmt.Fprintf(logFile, "\n=== script exited with error: %v ===\n", runErr)
		a.logger.Error("Init script failed", zap.Error(runErr))
		sendStatus("script_error", runErr.Error())
	} else {
		fmt.Fprintf(logFile, "\n=== script completed successfully at %s ===\n", time.Now().Format(time.RFC3339))
		a.logger.Info("Init script completed successfully", zap.String("log", logPath))
		sendStatus("script_complete", "init script finished, log at "+logPath)
	}
}

// ensureRemoteDesktop is implemented per-OS: remotedesktop_windows.go brings up RDP
// (Pro) or VNC (Home); remotedesktop_unix.go is a no-op.

// grpcSafe returns the current gRPC client under grpcMu. May be nil while the
// agent is reconnecting or shutting down — callers MUST nil-check.
func (a *Agent) grpcSafe() *grpc.Client {
	a.grpcMu.Lock()
	defer a.grpcMu.Unlock()
	return a.grpcClient
}

// sendUpdateStatus reports self-update progress to the backend. Nil-safe: during
// a self-update the service may be mid-restart and the client may already be gone.
func (a *Agent) sendUpdateStatus(status, message string) {
	if c := a.grpcSafe(); c != nil {
		_ = c.SendStatusUpdate(status, message)
	}
}

// handleSelfUpdate downloads a new agent package, hot-swaps the running binary,
// and restarts the service.
//
// It deliberately NEVER runs the OS package manager (dpkg -i / rpm -U): those run
// prerm/postinstall hooks that stop the service mid-update and historically caused
// the agent to crash. Instead it extracts JUST the binary from whatever package
// format was downloaded and atomically replaces the current executable, then asks
// the service manager to restart. The whole thing is wrapped in a panic guard so a
// failed update can never take down the running agent.
func (a *Agent) handleSelfUpdate(downloadURL string) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("Self-update recovered from panic", zap.Any("panic", r))
			a.sendUpdateStatus("update_error", fmt.Sprintf("panic during update: %v", r))
		}
	}()

	a.logger.Info("Self-update started", zap.String("url", downloadURL))
	a.sendUpdateStatus("update_started", "downloading new agent package")

	// 1. Resolve the path of the currently-running binary — that's what we replace.
	currentBin, err := os.Executable()
	if err != nil {
		a.logger.Error("Self-update: cannot resolve own path", zap.Error(err))
		a.sendUpdateStatus("update_error", "cannot resolve executable path: "+err.Error())
		return
	}
	if resolved, rerr := filepath.EvalSymlinks(currentBin); rerr == nil {
		currentBin = resolved
	}
	binDir := filepath.Dir(currentBin)

	// 2. Download the package to a temp file.
	tmpPkg, err := os.CreateTemp("", "wxt-agent-update-*")
	if err != nil {
		a.sendUpdateStatus("update_error", "create temp file: "+err.Error())
		return
	}
	tmpPkgPath := tmpPkg.Name()
	defer a.removeQuiet(tmpPkgPath)

	if err := a.downloadUpdate(downloadURL, tmpPkg); err != nil {
		_ = tmpPkg.Close()
		a.logger.Error("Self-update download failed", zap.Error(err))
		a.sendUpdateStatus("update_error", "download failed: "+err.Error())
		return
	}
	if err := tmpPkg.Close(); err != nil {
		a.logger.Warn("Self-update: failed to close downloaded package", zap.Error(err))
	}

	// 3. Extract the new binary out of the package (dispatched by file extension).
	a.sendUpdateStatus("update_installing", "extracting new binary")
	newBin, err := a.extractAgentBinary(downloadURL, tmpPkgPath, binDir)
	if err != nil {
		a.logger.Error("Self-update extract failed", zap.Error(err))
		a.sendUpdateStatus("update_error", "extract failed: "+err.Error())
		return
	}
	defer a.removeQuiet(newBin)

	// 4. Atomically swap the binary. On Linux/macOS you may rename over a running
	// executable — the live process keeps its open inode while the path is repointed
	// at the new file, which the next service start will exec. newBin must stay
	// executable, hence 0755 (not a secret — it's the agent binary itself).
	if err := os.Chmod(newBin, 0o755); err != nil { // #nosec G302
		a.logger.Warn("Self-update: failed to chmod new binary", zap.Error(err))
	}
	if err := os.Rename(newBin, currentBin); err != nil {
		// Cross-device rename or permission denied → fall back to a sudo copy.
		// newBin/currentBin are agent-internal paths (temp extract dir / our own
		// resolved executable path), not request input; argv-slice form, no shell.
		if out, cerr := osExec.Command("sudo", "cp", newBin, currentBin).CombinedOutput(); cerr != nil { // #nosec G204
			a.logger.Error("Self-update binary swap failed", zap.Error(cerr), zap.ByteString("out", out))
			a.sendUpdateStatus("update_error", "binary swap failed: "+cerr.Error())
			return
		}
		if err := osExec.Command("sudo", "chmod", "755", currentBin).Run(); err != nil { // #nosec G204
			a.logger.Warn("Self-update: failed to chmod swapped binary via sudo", zap.Error(err))
		}
	}

	a.logger.Info("Self-update binary swapped, restarting service", zap.String("bin", currentBin))
	a.sendUpdateStatus("update_complete", "agent updated — restarting service")

	// 5. Restart through the service manager (detached). systemd/launchctl owns the
	// lifecycle, so even though this process is about to be killed, the restart
	// completes and the NEW binary comes up and re-registers with its new version.
	time.Sleep(500 * time.Millisecond) // let the status message flush over the stream
	a.restartService()
}

// downloadUpdate streams the package at url into dst. It trusts the system CA pool
// plus the agent's pinned private CA (this deployment fronts downloads with a
// private CA), and uses a generous timeout to allow for ~20MB binaries.
func (a *Agent) downloadUpdate(url string, dst *os.File) error {
	pool, _ := x509.SystemCertPool()
	if pool == nil {
		pool = x509.NewCertPool()
	}
	caCertFile := a.config.Server.CACertFile
	if caCertFile == "" {
		if _, err := os.Stat(serverCACert); err == nil {
			caCertFile = serverCACert
		}
	}
	if caCertFile != "" {
		// caCertFile is agent config (CLI flag / config file) or the fixed
		// serverCACert constant, not request input.
		if caPEM, err := os.ReadFile(caCertFile); err == nil { // #nosec G304
			pool.AppendCertsFromPEM(caPEM)
		}
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	n, err := io.Copy(dst, resp.Body)
	if err != nil {
		return fmt.Errorf("write package: %w", err)
	}
	a.logger.Info("Self-update package downloaded", zap.Int64("bytes", n))
	return nil
}

// extractAgentBinary pulls the wxt-agent executable out of the downloaded package.
// Returns the path to the extracted binary (a temp file the caller must remove).
func (a *Agent) extractAgentBinary(downloadURL, pkgPath, workDir string) (string, error) {
	lower := strings.ToLower(downloadURL)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractBinaryFromTarGz(pkgPath, workDir)
	case strings.HasSuffix(lower, ".deb"):
		return extractBinaryFromDeb(pkgPath, workDir)
	case strings.HasSuffix(lower, ".dmg"):
		return extractBinaryFromDmg(pkgPath, workDir)
	case strings.HasSuffix(lower, ".exe"):
		// A standalone .exe IS the binary — just copy it out.
		dst := filepath.Join(workDir, "wxt-agent-new.exe")
		if err := copyFile(pkgPath, dst, 0o755); err != nil {
			return "", err
		}
		return dst, nil
	default:
		return "", fmt.Errorf("unsupported package format: %s", downloadURL)
	}
}

// extractBinaryFromTarGz finds the wxt-agent binary inside a .tar.gz and writes it
// to a temp file in workDir.
func extractBinaryFromTarGz(pkgPath, workDir string) (string, error) {
	// pkgPath is our own just-downloaded temp file, not request input.
	f, err := os.Open(pkgPath) // #nosec G304
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base == "wxt-agent" || base == "wxt-agent.exe" {
			return writeTempBinary(workDir, tr)
		}
	}
	return "", fmt.Errorf("wxt-agent binary not found in tar.gz")
}

// extractBinaryFromDeb extracts the binary from a .deb using `dpkg-deb -x` (which
// only unpacks files — it does NOT run maintainer scripts or touch the service).
func extractBinaryFromDeb(pkgPath, workDir string) (string, error) {
	outDir, err := os.MkdirTemp(workDir, "deb-extract-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(outDir) }() // best-effort temp dir cleanup

	// pkgPath/outDir are our own temp-file paths, not request input.
	if out, err := osExec.Command("dpkg-deb", "-x", pkgPath, outDir).CombinedOutput(); err != nil { // #nosec G204
		return "", fmt.Errorf("dpkg-deb -x: %v (%s)", err, string(out))
	}

	// The .deb installs the binary at usr/local/bin/wxt-agent.
	var found string
	_ = filepath.Walk(outDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == "wxt-agent" {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("wxt-agent binary not found in .deb")
	}

	dst := filepath.Join(workDir, "wxt-agent-new")
	if err := copyFile(found, dst, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}

// extractBinaryFromDmg mounts a .dmg, copies the wxt-agent binary out, and detaches.
func extractBinaryFromDmg(pkgPath, workDir string) (string, error) {
	mnt, err := os.MkdirTemp(workDir, "dmg-mount-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(mnt) }() // best-effort temp dir cleanup

	// mnt/pkgPath are our own temp-file paths, not request input.
	if out, err := osExec.Command("hdiutil", "attach", "-nobrowse", "-mountpoint", mnt, pkgPath).CombinedOutput(); err != nil { // #nosec G204
		return "", fmt.Errorf("hdiutil attach: %v (%s)", err, string(out))
	}
	defer func() { _ = osExec.Command("hdiutil", "detach", mnt, "-force").Run() }() // #nosec G204

	var found string
	_ = filepath.Walk(mnt, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == "wxt-agent" {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("wxt-agent binary not found in .dmg")
	}

	dst := filepath.Join(workDir, "wxt-agent-new")
	if err := copyFile(found, dst, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}

// writeTempBinary streams r into a uniquely-named temp file in workDir.
func writeTempBinary(workDir string, r io.Reader) (string, error) {
	out, err := os.CreateTemp(workDir, "wxt-agent-new-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, r); err != nil {
		if rerr := os.Remove(out.Name()); rerr != nil {
			return "", fmt.Errorf("copy failed (%w) and cleanup failed: %v", err, rerr)
		}
		return "", err
	}
	return out.Name(), nil
}

// copyFile copies src to dst with the given file mode. Both are always
// agent-internal paths (self-update temp/extract dirs), never request input.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) // #nosec G304
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) // #nosec G304
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(mode)
}

// restartService asks the OS service manager to restart the agent. It runs detached
// so the restart survives this process being killed. systemd/launchctl then brings
// the freshly-swapped binary back up, which re-registers with the new version.
func (a *Agent) restartService() {
	// The identifiers come from serviceids.go rather than being written out
	// here: when they were duplicated they drifted from what configure
	// actually registers, and a restart that targets the wrong name fails
	// silently — leaving the machine running the old binary after a
	// "successful" update.
	var cmd *osExec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = osExec.Command("sudo", "systemctl", "restart", SystemdUnit)
	case "darwin":
		// kickstart -k forces launchd to stop and restart the job.
		cmd = osExec.Command("launchctl", "kickstart", "-k", "system/"+LaunchDaemonLabel)
	case "windows":
		// A scheduled task, not a service — so schtasks, not `net`.
		cmd = osExec.Command("schtasks", "/end", "/tn", WindowsTaskName)
	default:
		a.logger.Warn("Self-update: no restart strategy for OS", zap.String("os", runtime.GOOS))
		return
	}

	a.logger.Info("Self-update: restarting the agent service",
		zap.String("os", runtime.GOOS), zap.String("cmd", cmd.String()))

	if out, err := cmd.CombinedOutput(); err != nil {
		// Worth surfacing: a failed restart is the difference between an
		// update that took effect and one that only appeared to.
		a.logger.Error("Self-update: restart command failed — the agent may still be running the old binary",
			zap.Error(err), zap.ByteString("out", out))
		return
	}

	if runtime.GOOS == "windows" {
		// schtasks /end only stops it; the task's own trigger does not re-run
		// on demand, so it has to be started explicitly.
		if out, err := osExec.Command("schtasks", "/run", "/tn", WindowsTaskName).CombinedOutput(); err != nil {
			a.logger.Error("Self-update: could not restart the scheduled task",
				zap.Error(err), zap.ByteString("out", out))
		}
	}
}

// handleTerminalInput handles terminal input
func (a *Agent) handleTerminalInput(input *agentv1.TerminalInput) error {
	a.logger.Debug("Received terminal input",
		zap.String("session_id", input.SessionId),
		zap.Int("data_len", len(input.Data)),
	)

	// Check for VS Code messages (JSON format)
	var msg map[string]interface{}
	if err := json.Unmarshal(input.Data, &msg); err == nil {
		if msgType, ok := msg["type"].(string); ok {
			if msgType == "vscode_init" {
				a.logger.Info("Received VS Code initialization request",
					zap.String("session_id", input.SessionId))
				go a.initializeVSCodeServer(input.SessionId, "")
				return nil
			} else if msgType == "vscode_open_folder" {
				folder, _ := msg["folder"].(string)
				a.logger.Info("Received VS Code open folder request",
					zap.String("session_id", input.SessionId),
					zap.String("folder", folder))
				go a.openVSCodeFolder(input.SessionId, folder)
				return nil
			} else if msgType == "vscode_cleanup" {
				a.logger.Info("Received VS Code cleanup request",
					zap.String("session_id", input.SessionId))
				go a.cleanupVSCodeServer(input.SessionId)
				return nil
			} else if msgType == "fs_operation" {
				a.logger.Debug("Received filesystem operation request",
					zap.String("session_id", input.SessionId))
				go a.handleFilesystemOperation(input.SessionId, msg)
				return nil
			} else if remotecontrol.Handles(msgType) {
				a.logger.Info("Received remote-control message",
					zap.String("session_id", input.SessionId),
					zap.String("type", msgType))
				a.remoteControl.HandleMessage(input.SessionId, msg)
				return nil
			} else if msgType == "port_connect" || msgType == "port_data" || msgType == "port_close" {
				a.logger.Debug("Received port forwarding message",
					zap.String("session_id", input.SessionId),
					zap.String("type", msgType))
				// Handle SYNCHRONOUSLY (no goroutine) so messages are processed in the
				// exact order they arrived on the stream. Spawning a goroutine per message
				// raced port_connect against the first port_data: the data goroutine could
				// run before the connection was dialed+registered, so guacd's opening RDP
				// negotiation bytes were dropped ("Connection not found for data") →
				// corrupted handshake → intermittent "wrong security type". handleConnect
				// returns quickly (it starts its own reader goroutine) and handleData is a
				// fast localhost write, so the receive loop is not meaningfully blocked.
				a.portForward.HandleMessage(input.SessionId, msg)
				return nil
			}
		}
	}

	// Regular terminal logic - Get or create PTY session
	session, err := a.ptyHandler.GetSession(input.SessionId)
	if err != nil {
		// Session doesn't exist, create new one
		userInfo := a.executor.GetUserInfo()
		session, err = a.ptyHandler.CreateSession(
			input.SessionId,
			userInfo.Username,
			userInfo.UID,
			userInfo.GID,
			userInfo.HomeDir,
			userInfo.Shell,
		)
		if err != nil {
			a.logger.Error("Failed to create PTY session",
				zap.String("session_id", input.SessionId),
				zap.Error(err),
			)
			return err
		}

		// Start forwarding output to backend
		go a.forwardTerminalOutput(session)
	}

	// Write input to PTY
	if err := session.Write(input.Data); err != nil {
		a.logger.Error("Failed to write to PTY",
			zap.String("session_id", input.SessionId),
			zap.Error(err),
		)
		return err
	}

	return nil
}

// forwardTerminalOutput forwards PTY output to backend
func (a *Agent) forwardTerminalOutput(session *pty.Session) {
	for data := range session.OutputChan {
		if err := a.grpcClient.SendTerminalOutput(session.ID, data); err != nil {
			a.logger.Error("Failed to send terminal output",
				zap.String("session_id", session.ID),
				zap.Error(err),
			)
		}
	}

	a.logger.Info("Terminal output forwarding stopped", zap.String("session_id", session.ID))
}

// handleConfigUpdate handles configuration updates
func (a *Agent) handleConfigUpdate(config *agentv1.ConfigUpdate) error {
	a.logger.Info("Received config update")
	// TODO: Update local config
	return nil
}

// handleApproval handles command approval
func (a *Agent) handleApproval(approval *agentv1.ApprovalNotification) error {
	a.logger.Info("Received approval",
		zap.String("command_id", approval.CommandId),
		zap.Bool("approved", approval.Approved),
	)
	// TODO: Process approval
	return nil
}

// sendTerminalOutput writes to the session's terminal, logging (not failing)
// if the underlying gRPC stream write fails — a lost status line shouldn't
// abort whatever operation it was reporting on.
func (a *Agent) sendTerminalOutput(sessionID string, data []byte) {
	if err := a.grpcClient.SendTerminalOutput(sessionID, data); err != nil {
		a.logger.Warn("Failed to send terminal output", zap.String("session_id", sessionID), zap.Error(err))
	}
}

// initializeVSCodeServer installs and starts VS Code server
func (a *Agent) initializeVSCodeServer(sessionID, folder string) {
	ctx := context.Background()

	// Send status
	a.sendTerminalOutput(sessionID, []byte("Installing VS Code server...\r\n"))

	// Initialize VS Code server if not already done
	if a.vscodeServer == nil {
		a.vscodeServer = vscode.NewServer(a.logger)
	}

	// Install
	if err := a.vscodeServer.Install(ctx); err != nil {
		a.logger.Error("Failed to install VS Code server", zap.Error(err))
		errorMsg := fmt.Sprintf("Failed to install VS Code server: %v\r\n", err)
		a.sendTerminalOutput(sessionID, []byte(errorMsg))
		return
	}

	a.sendTerminalOutput(sessionID, []byte("VS Code server installed successfully\r\n"))

	// Start
	a.sendTerminalOutput(sessionID, []byte("Starting VS Code server...\r\n"))

	cmd, err := a.vscodeServer.Start(ctx, sessionID, folder)
	if err != nil {
		a.logger.Error("Failed to start VS Code server", zap.Error(err))
		errorMsg := fmt.Sprintf("Failed to start VS Code server: %v\r\n", err)
		a.sendTerminalOutput(sessionID, []byte(errorMsg))
		return
	}

	// Store process for cleanup
	a.vscodeProcessesMux.Lock()
	a.vscodeProcesses[sessionID] = cmd
	a.vscodeProcessesMux.Unlock()

	// Success
	successMsg := fmt.Sprintf("VS Code server started successfully (PID: %d)\r\n", cmd.Process.Pid)
	a.sendTerminalOutput(sessionID, []byte(successMsg))

	a.logger.Info("VS Code server initialized",
		zap.String("session_id", sessionID),
		zap.Int("pid", cmd.Process.Pid))
}

// openVSCodeFolder restarts code-server with a specific folder
func (a *Agent) openVSCodeFolder(sessionID, folder string) {
	ctx := context.Background()

	a.logger.Info("Opening folder in VS Code server",
		zap.String("session_id", sessionID),
		zap.String("folder", folder))

	// Stop existing server if running
	a.vscodeProcessesMux.Lock()
	if cmd, exists := a.vscodeProcesses[sessionID]; exists {
		if cmd.Process != nil {
			a.logger.Info("Stopping existing code-server",
				zap.String("session_id", sessionID),
				zap.Int("pid", cmd.Process.Pid))
			if err := cmd.Process.Kill(); err != nil {
				a.logger.Warn("Failed to kill existing code-server process", zap.Error(err))
			}
		}
		delete(a.vscodeProcesses, sessionID)
	}
	a.vscodeProcessesMux.Unlock()

	// Start with new folder
	a.sendTerminalOutput(sessionID, []byte(fmt.Sprintf("Opening folder: %s\r\n", folder)))

	cmd, err := a.vscodeServer.Start(ctx, sessionID, folder)
	if err != nil {
		a.logger.Error("Failed to start VS Code server with folder", zap.Error(err))
		errorMsg := fmt.Sprintf("Failed to open folder: %v\r\n", err)
		a.sendTerminalOutput(sessionID, []byte(errorMsg))
		return
	}

	// Store process for cleanup
	a.vscodeProcessesMux.Lock()
	a.vscodeProcesses[sessionID] = cmd
	a.vscodeProcessesMux.Unlock()

	// Success
	successMsg := "Folder opened successfully! code-server is running on port 8080\r\n"
	a.sendTerminalOutput(sessionID, []byte(successMsg))

	a.logger.Info("VS Code server opened folder",
		zap.String("session_id", sessionID),
		zap.String("folder", folder),
		zap.Int("pid", cmd.Process.Pid))
}

// cleanupVSCodeServer stops VS Code server for a session
func (a *Agent) cleanupVSCodeServer(sessionID string) {
	a.vscodeProcessesMux.Lock()
	cmd, exists := a.vscodeProcesses[sessionID]
	if exists {
		delete(a.vscodeProcesses, sessionID)
	}
	a.vscodeProcessesMux.Unlock()

	if !exists {
		a.logger.Warn("No VS Code server process found for session",
			zap.String("session_id", sessionID))
		return
	}

	// Kill the process
	if cmd.Process != nil {
		a.logger.Info("Stopping VS Code server",
			zap.String("session_id", sessionID),
			zap.Int("pid", cmd.Process.Pid))

		if err := cmd.Process.Kill(); err != nil {
			a.logger.Error("Failed to kill VS Code server process",
				zap.String("session_id", sessionID),
				zap.Error(err))
		} else {
			a.logger.Info("VS Code server stopped successfully",
				zap.String("session_id", sessionID))
			a.sendTerminalOutput(sessionID, []byte("VS Code server stopped\r\n"))
		}
	}
}

// handleFilesystemOperation handles filesystem operations from VS Code extension
func (a *Agent) handleFilesystemOperation(sessionID string, msg map[string]interface{}) {
	requestID, _ := msg["request_id"].(float64)
	operation, _ := msg["operation"].(string)
	path, _ := msg["path"].(string)
	data, _ := msg["data"].(map[string]interface{})

	a.logger.Debug("Processing filesystem operation",
		zap.String("session_id", sessionID),
		zap.String("operation", operation),
		zap.String("path", path))

	var result interface{}
	var errMsg string

	switch operation {
	case "stat":
		res, err := a.fsOps.Stat(path)
		if err != nil {
			errMsg = err.Error()
		} else {
			result = res
		}

	case "readdir":
		res, err := a.fsOps.ReadDir(path)
		if err != nil {
			errMsg = err.Error()
		} else {
			result = res
		}

	case "read":
		content, err := a.fsOps.ReadFile(path)
		if err != nil {
			errMsg = err.Error()
		} else {
			result = map[string]interface{}{"content": content}
		}

	case "write":
		if data != nil {
			content, _ := data["content"].(string)
			create, _ := data["create"].(bool)
			overwrite, _ := data["overwrite"].(bool)
			err := a.fsOps.WriteFile(path, content, create, overwrite)
			if err != nil {
				errMsg = err.Error()
			} else {
				result = map[string]interface{}{"success": true}
			}
		} else {
			errMsg = "missing data for write operation"
		}

	case "mkdir":
		err := a.fsOps.MkDir(path)
		if err != nil {
			errMsg = err.Error()
		} else {
			result = map[string]interface{}{"success": true}
		}

	case "delete":
		if data != nil {
			recursive, _ := data["recursive"].(bool)
			err := a.fsOps.Delete(path, recursive)
			if err != nil {
				errMsg = err.Error()
			} else {
				result = map[string]interface{}{"success": true}
			}
		} else {
			errMsg = "missing data for delete operation"
		}

	case "rename":
		if data != nil {
			newPath, _ := data["new_path"].(string)
			overwrite, _ := data["overwrite"].(bool)
			err := a.fsOps.Rename(path, newPath, overwrite)
			if err != nil {
				errMsg = err.Error()
			} else {
				result = map[string]interface{}{"success": true}
			}
		} else {
			errMsg = "missing data for rename operation"
		}

	case "copy":
		if data != nil {
			destPath, _ := data["dest_path"].(string)
			overwrite, _ := data["overwrite"].(bool)
			err := a.fsOps.Copy(path, destPath, overwrite)
			if err != nil {
				errMsg = err.Error()
			} else {
				result = map[string]interface{}{"success": true}
			}
		} else {
			errMsg = "missing data for copy operation"
		}

	default:
		errMsg = fmt.Sprintf("unknown operation: %s", operation)
	}

	// Send response back through WebSocket
	response := map[string]interface{}{
		"type":       "fs_response",
		"request_id": requestID,
	}

	if errMsg != "" {
		response["error"] = errMsg
	} else {
		response["data"] = result
	}

	responseBytes, err := json.Marshal(response)
	if err != nil {
		a.logger.Error("Failed to marshal filesystem response",
			zap.String("session_id", sessionID),
			zap.Error(err))
		return
	}

	// Send through terminal output channel
	if err := a.grpcClient.SendTerminalOutput(sessionID, responseBytes); err != nil {
		a.logger.Error("Failed to send filesystem response",
			zap.String("session_id", sessionID),
			zap.Error(err))
	}
}

// Helper functions

func (a *Agent) getLinuxUserInfo() (*agentv1.LinuxUserInfo, error) {
	u, err := user.Lookup(a.config.LinuxUser.Username)
	if err != nil {
		return nil, err
	}

	// Get groups
	groups := []string{"users"} // Default
	if gids, err := u.GroupIds(); err == nil {
		for _, gid := range gids {
			if g, err := user.LookupGroupId(gid); err == nil {
				groups = append(groups, g.Name)
			}
		}
	}

	// Check sudo access
	hasSudo := false
	for _, g := range groups {
		if g == "sudo" {
			hasSudo = true
			break
		}
	}

	return &agentv1.LinuxUserInfo{
		Username: u.Username,
		Uid:      int32(parseInt(u.Uid)), // #nosec G115 -- OS UID/GID from os/user, never negative in practice
		Gid:      int32(parseInt(u.Gid)), // #nosec G115
		Groups:   groups,
		HasSudo:  hasSudo,
		HomeDir:  u.HomeDir,
		Shell:    "/bin/bash", // Default
	}, nil
}

func (a *Agent) getOSInfo() string {
	return monitor.GetOSInfo()
}

func (a *Agent) getResourceStats() (*commonv1.ResourceStats, error) {
	return a.monitor.GetStats()
}

func parseInt(s string) int {
	var i int
	_, _ = fmt.Sscanf(s, "%d", &i) // best-effort: i stays 0 on a malformed input
	return i
}

// validateConfig validates configuration
func validateConfig(cfg *config.Config) error {
	if cfg.Server.GRPCURL == "" {
		return fmt.Errorf("server URL is required")
	}
	if cfg.LinuxUser.Username == "" {
		return fmt.Errorf("linux user is required")
	}
	return nil
}

// validateLinuxUser validates Linux user (optional check, user will be created if needed)
func validateLinuxUser(username string) error {
	// User existence is validated/created in executor.New()
	// Just validate username format here
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}
	return nil
}
