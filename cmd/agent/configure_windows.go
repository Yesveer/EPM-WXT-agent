//go:build windows

package main

import (
	"bytes"
	"crypto/des" //nolint:staticcheck // VNC password obfuscation requires single-DES
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/vsay/vsay-agent/internal/agent"
	"github.com/vsay/vsay-agent/internal/config"
)

// Windows install layout — mirrors the Unix /etc/vsay layout under ProgramData.
const (
	winConfigDir  = `C:\ProgramData\vsay`
	winCertDir    = `C:\ProgramData\vsay\certs`
	winLogDir     = `C:\ProgramData\vsay\logs`
	winConfigFile = `C:\ProgramData\vsay\agent.yaml`
	winTaskName   = "VsayAgent"
)

// runConfigureWindows is the Windows equivalent of the Linux configure flow. It sets
// up an RDP user, enables Remote Desktop, pins the backend CA, obtains a signed mTLS
// client cert, writes the encrypted config, and registers a startup scheduled task.
//
// This file is compiled ONLY on Windows (//go:build windows) — it never affects Linux.
func runConfigureWindows(p configureParams) error {
	if p.Token == "" || p.Tenant == "" || p.Org == "" || p.Project == "" || p.User == "" || p.Host == "" {
		return fmt.Errorf("missing required flags: token, tenant, org, project, user, and host are required")
	}

	edition := detectEdition()
	fmt.Printf("• Detected Windows edition: %s\n", edition)

	// Auto-detect the interactive user. For RDP this is the account to log in as; for
	// VNC it's informational (VNC auths by password). Must be non-empty so config
	// validation passes on start.
	winUser := p.WindowsUser
	if winUser == "" {
		winUser = detectWindowsUser()
	}
	if winUser == "" {
		winUser = "vsay"
	}

	// Remote-desktop backend per edition (the industry-standard split):
	//   Home  → VNC  (Home cannot host RDP at all)
	//   Pro/Enterprise/Education/Server → RDP (native, now reliable — the relay
	//                                    drop-and-teardown bug that made it look broken
	//                                    is fixed on the backend).
	if edition == "home" {
		if p.WindowsPassword == "" {
			return fmt.Errorf("--windows-password is required on Windows Home (used as the VNC password)")
		}
		fmt.Println("• Setting up VNC server (Windows Home)")
		if err := installVNCServer(p.WindowsPassword); err != nil {
			return fmt.Errorf("VNC setup failed: %w", err)
		}
		fmt.Println("✓ VNC server installed and running on port 5900")
	} else {
		fmt.Printf("• Enabling Remote Desktop (RDP) for user %q\n", winUser)
		enableWindowsRDPConfigure()
		// --windows-password is OPTIONAL for RDP: if given, we set it on the account so
		// there are known credentials; if omitted, the operator logs in with their
		// existing Windows password at connect time. Either way the account is added to
		// the Remote Desktop Users group.
		if err := ensureWindowsUser(winUser, p.WindowsPassword); err != nil {
			fmt.Printf("Warning: could not fully configure RDP user: %v\n", err)
		}
		fmt.Println("✓ Remote Desktop enabled on port 3389")
	}

	// 3. Create install directories.
	for _, d := range []string{winConfigDir, winCertDir, winLogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	// 4. Determine gRPC URL.
	grpcURL := p.APIHost
	if grpcURL == "" {
		grpcURL = extractGRPCURL(p.Host)
	}
	fmt.Printf("✓ gRPC address: %s\n", grpcURL)

	// 5. Pin the backend CA cert.
	caCertFile, caFingerprint, err := fetchAndSaveCAWindows(p.Host)
	if err != nil {
		fmt.Printf("Warning: could not pin CA cert: %v\n", err)
		fmt.Println("  Agent will use system CA pool for server verification")
	} else if caFingerprint != "" {
		fmt.Println("✓ CA cert pinned — SHA256:", caFingerprint)
	}

	// 6. Obtain a signed mTLS client cert.
	if err := fetchSignedClientCertWindows(p.Host, p.Token); err != nil {
		return fmt.Errorf("failed to get signed client cert: %w", err)
	}
	fmt.Println("✓ Client cert signed by backend CA")

	// 7. Build + save encrypted config.
	homeDir := ""
	if winUser != "" {
		homeDir = `C:\Users\` + winUser
	}
	cfg := &config.Config{
		Agent: config.AgentConfig{ID: "", Name: p.MachineName},
		Server: config.ServerConfig{
			Host:       p.Host,
			GRPCURL:    grpcURL,
			APIHost:    p.APIHost,
			TLS:        true,
			TokenHash:  p.Token,
			CACertFile: caCertFile,
		},
		Tunnel: config.TunnelConfig{
			URL:          p.TunnelURL,
			Enabled:      p.TunnelURL != "",
			PollInterval: "10s",
		},
		Tenant:     config.TenantConfig{ID: p.Tenant},
		Org:        config.OrgConfig{ID: p.Org},
		Project:    config.ProjectConfig{ID: p.Project},
		PortalUser: config.PortalUserConfig{Email: p.User},
		LinuxUser: config.LinuxUserConfig{ // reused for the Windows account
			Username:    winUser,
			HomeDir:     homeDir,
			Shell:       "",
			SudoEnabled: false,
		},
		Permissions: config.PermissionsConfig{AllowSudo: false},
		Logging: config.LoggingConfig{
			Level:      "info",
			File:       filepath.Join(winLogDir, "agent.log"),
			MaxSizeMB:  100,
			MaxBackups: 5,
		},
		Status: "pending_approval",
	}

	if err := config.SaveEncrypted(cfg, winConfigFile); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	fmt.Println("✓ Configuration saved (encrypted) to", winConfigFile)

	// 8. Register + start a startup scheduled task running the agent as SYSTEM.
	if err := setupWindowsService(); err != nil {
		return fmt.Errorf("failed to register startup task: %w", err)
	}
	fmt.Println("✓ Agent registered to run at system startup and started")
	fmt.Println("")
	fmt.Println("Check task:  schtasks /query /tn", winTaskName)
	fmt.Println("Stop agent:  schtasks /end /tn", winTaskName)
	return nil
}

// vncDownloadURL is the TightVNC installer used on Windows Home (overridable via env).
const vncDownloadURLDefault = "https://www.tightvnc.com/download/2.8.84/tightvnc-2.8.84-gpl-setup-64bit.msi"

// detectEdition returns "home" or "pro" from the registry EditionID. Home editions
// ("Core*") cannot host RDP and use VNC instead.
func detectEdition() string {
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "EditionID").Output()
	if err != nil {
		return "pro"
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "pro"
	}
	if strings.Contains(strings.ToLower(fields[len(fields)-1]), "core") {
		return "home"
	}
	return "pro"
}

// installVNCServer downloads and silently installs TightVNC as a service listening on
// port 5900, with the given VNC password. Used on Windows Home where RDP is unavailable.
// VNC auth truncates the password to 8 characters.
func installVNCServer(password string) error {
	alreadyInstalled := false
	if _, err := os.Stat(`C:\Program Files\TightVNC\tvnserver.exe`); err == nil {
		alreadyInstalled = true
	}

	url := os.Getenv("VNC_DOWNLOAD_URL")
	if url == "" {
		url = vncDownloadURLDefault
	}
	msiPath := filepath.Join(os.TempDir(), "tightvnc-setup.msi")
	fmt.Println("• Downloading VNC server:", url)
	if err := downloadFile(url, msiPath); err != nil {
		return fmt.Errorf("download VNC installer: %w", err)
	}
	defer os.Remove(msiPath)

	// If TightVNC is already installed, UNINSTALL it first. The MSI's SET_PASSWORD only
	// applies reliably on a FRESH install — on a reinstall the old password sticks,
	// which is what caused guacd's "authentication failed". A clean install guarantees
	// the password below is the active one.
	if alreadyInstalled {
		fmt.Println("• Reinstalling TightVNC to reset the VNC password")
		_ = exec.Command("net", "stop", "tvnserver").Run()
		_ = exec.Command("msiexec", "/x", msiPath, "/quiet", "/norestart").Run()
	}

	// Fresh silent install: server as a service, firewall exception, VNC auth ON with
	// the configured password.
	if out, err := exec.Command("msiexec",
		"/i", msiPath, "/quiet", "/norestart",
		"ADDLOCAL=Server",
		"SERVER_REGISTER_AS_SERVICE=1",
		"SERVER_ADD_FIREWALL_EXCEPTION=1",
		"SET_USEVNCAUTHENTICATION=1", "VALUE_OF_USEVNCAUTHENTICATION=1",
		"SET_PASSWORD=1", "VALUE_OF_PASSWORD="+password,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("msiexec install failed: %v (%s)", err, string(out))
	}

	// Belt-and-suspenders: also write the password to the registry directly (standard
	// VNC DES obfuscation), in case the MSI property didn't take.
	if err := setTightVNCPassword(password); err != nil {
		fmt.Println("• Warning: registry password set failed:", err)
	}

	// Restart the service so the password takes effect immediately.
	_ = exec.Command("sc", "config", "tvnserver", "start=", "auto").Run()
	_ = exec.Command("net", "stop", "tvnserver").Run()
	_ = exec.Command("net", "start", "tvnserver").Run()
	return nil
}

// setTightVNCPassword writes the VNC password to the TightVNC registry. VNC stores the
// password DES-encrypted with a well-known fixed key (with each key byte bit-reversed —
// the classic VNC quirk). This is the same scheme vncpasswd uses.
func setTightVNCPassword(password string) error {
	enc := vncObfuscate(password)
	hexStr := hex.EncodeToString(enc)

	// Ensure VNC authentication is enabled, then write the obfuscated password.
	_ = exec.Command("reg", "add", `HKLM\SOFTWARE\TightVNC\Server`,
		"/v", "UseVncAuthentication", "/t", "REG_DWORD", "/d", "1", "/f").Run()
	if out, err := exec.Command("reg", "add", `HKLM\SOFTWARE\TightVNC\Server`,
		"/v", "Password", "/t", "REG_BINARY", "/d", hexStr, "/f").CombinedOutput(); err != nil {
		return fmt.Errorf("reg add Password: %v (%s)", err, string(out))
	}
	return nil
}

// vncObfuscate DES-encrypts the (up to 8-char) password with VNC's fixed key to produce
// the 8-byte value stored in the registry.
func vncObfuscate(password string) []byte {
	// Classic VNC fixed key, each byte bit-reversed before use as the DES key.
	fixed := []byte{23, 82, 107, 6, 35, 78, 88, 7}
	key := make([]byte, 8)
	for i, b := range fixed {
		key[i] = reverseBits(b)
	}
	block, err := des.NewCipher(key)
	if err != nil {
		return make([]byte, 8)
	}
	pt := make([]byte, 8) // password padded with NULs / truncated to 8 bytes
	copy(pt, []byte(password))
	ct := make([]byte, 8)
	block.Encrypt(ct, pt)
	return ct
}

func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = (r << 1) | (b & 1)
		b >>= 1
	}
	return r
}

// downloadFile fetches url into dst.
func downloadFile(url, dst string) error {
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// detectWindowsUser returns the interactive user running configure — the account the
// operator will RDP in as. Prefers the bare USERNAME env var; falls back to
// os/user.Current() (stripping any DOMAIN\ prefix).
func detectWindowsUser() string {
	if u := strings.TrimSpace(os.Getenv("USERNAME")); u != "" {
		return u
	}
	if cur, err := user.Current(); err == nil {
		name := cur.Username
		if i := strings.LastIndex(name, `\`); i >= 0 {
			name = name[i+1:] // strip DOMAIN\
		}
		return strings.TrimSpace(name)
	}
	return ""
}

// ensureWindowsUser makes sure the account exists and can log in over RDP.
//   - If a password is given: create the user with it, or set it on an existing
//     account, so RDP works with known credentials.
//   - If no password: assume the account already exists (the operator will enter
//     their own Windows password at connect time) and only add it to the RDP group.
// In all cases the user is added to "Remote Desktop Users".
func ensureWindowsUser(username, password string) error {
	if password != "" {
		// Create with password; if it already exists, set the password instead.
		if out, err := exec.Command("net", "user", username, password, "/add").CombinedOutput(); err != nil {
			if out2, err2 := exec.Command("net", "user", username, password).CombinedOutput(); err2 != nil {
				return fmt.Errorf("net user add/update failed: %v / %v (%s)", err, err2, string(out)+string(out2))
			}
		}
	}
	// Add to Remote Desktop Users (localized group name may differ; ignore failure).
	_ = exec.Command("net", "localgroup", "Remote Desktop Users", username, "/add").Run()
	return nil
}

// enableWindowsRDPConfigure enables Remote Desktop at configure time (best-effort).
func enableWindowsRDPConfigure() {
	_ = exec.Command("reg", "add",
		`HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server`,
		"/v", "fDenyTSConnections", "/t", "REG_DWORD", "/d", "0", "/f").Run()
	_ = exec.Command("netsh", "advfirewall", "firewall",
		"set", "rule", `group=remote desktop`, "new", "enable=Yes").Run()

	// Disable the NLA REQUIREMENT and let the security layer be negotiated. FreeRDP 3.x
	// (guacd 1.6.0) fails NLA/CredSSP against Windows 11 ("Server refused connection —
	// wrong security type"), so the backend connects over plain TLS; that only works if
	// Windows doesn't insist on NLA. (The tunnel itself is already mTLS-secured.)
	rdpTcp := `HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server\WinStations\RDP-Tcp`
	_ = exec.Command("reg", "add", rdpTcp,
		"/v", "UserAuthentication", "/t", "REG_DWORD", "/d", "0", "/f").Run()
	_ = exec.Command("reg", "add", rdpTcp,
		"/v", "SecurityLayer", "/t", "REG_DWORD", "/d", "1", "/f").Run()

	// NOTE: we deliberately DO NOT disable the WDDM graphics driver. On Windows 11 WDDM
	// is the only RDP graphics driver — disabling it leaves no renderer and yields a
	// black screen. The native FreeRDP 3.x GFX pipeline (32bpp) renders correctly with
	// WDDM on. If a previous build disabled it, remove that override so WDDM is active:
	_ = exec.Command("reg", "delete",
		`HKLM\SOFTWARE\Policies\Microsoft\Windows NT\Terminal Services`,
		"/v", "fEnableWddmDriver", "/f").Run()

	// Restart the RDP service so the security-layer settings apply.
	_ = exec.Command("net", "stop", "TermService", "/y").Run()
	_ = exec.Command("net", "start", "TermService").Run()
}

// winInstalledExe is the stable location the agent binary is copied to. Running the
// scheduled task from here (instead of the user's Downloads folder) avoids failures
// when SYSTEM tries to launch an exe out of a per-user profile directory.
const winInstalledExe = `C:\ProgramData\vsay\vsay-agent.exe`

// setupWindowsService copies the agent binary to a stable system location, then
// registers it as a startup scheduled task (runs as SYSTEM at boot) and starts it now.
// Scheduled tasks run console apps persistently without needing Windows Service
// Control Manager integration.
func setupWindowsService() error {
	// Copy the running binary to the stable install path.
	srcExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := copyExecutable(srcExe, winInstalledExe); err != nil {
		return fmt.Errorf("install binary to %s: %w", winInstalledExe, err)
	}
	fmt.Println("✓ Agent binary installed to", winInstalledExe)

	tr := fmt.Sprintf(`"%s" start --config "%s"`, winInstalledExe, winConfigFile)
	if out, err := exec.Command("schtasks", "/create",
		"/tn", winTaskName, "/tr", tr,
		"/sc", "onstart", "/ru", "SYSTEM", "/rl", "HIGHEST", "/f").CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks create: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("schtasks", "/run", "/tn", winTaskName).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks run: %v (%s)", err, string(out))
	}
	return nil
}

// copyExecutable copies src to dst (overwriting). Skips the copy if src and dst are
// already the same file.
func copyExecutable(src, dst string) error {
	if strings.EqualFold(src, dst) {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// fetchAndSaveCAWindows pins the backend CA to the Windows cert dir.
func fetchAndSaveCAWindows(host string) (string, string, error) {
	host = trimTrailingSlash(host)
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
	sum := sha256.Sum256(cert.Raw)
	fingerprint := fmt.Sprintf("%x", sum)
	caPath := filepath.Join(winCertDir, "server-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		return "", "", fmt.Errorf("save CA cert: %w", err)
	}
	// Persist the fingerprint now so the agent's first CA-rotation check matches and
	// does NOT trigger a rotation + cert re-sign loop on startup.
	_ = os.WriteFile(filepath.Join(winCertDir, "ca-fingerprint.txt"), []byte(fingerprint), 0o644)
	return caPath, fingerprint, nil
}

// fetchSignedClientCertWindows generates a CSR, has the backend sign it, and stores
// the encrypted key + cert in the Windows cert dir.
func fetchSignedClientCertWindows(host, token string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	hostname, _ := os.Hostname()
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{Organization: []string{"Vsay Agent"}, CommonName: hostname},
	}, key)
	if err != nil {
		return fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	req, err := http.NewRequest("POST", trimTrailingSlash(host)+"/agent/sign-cert", bytes.NewReader(csrPEM))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-pem-file")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
	if err := os.WriteFile(filepath.Join(winCertDir, "client-key.enc"), encKey, 0o600); err != nil {
		return fmt.Errorf("save key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(winCertDir, "client-cert.enc"), encCert, 0o600); err != nil {
		return fmt.Errorf("save cert: %w", err)
	}
	return nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
