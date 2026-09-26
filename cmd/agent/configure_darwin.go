//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Yesveer/wxt-agent/internal/agent"
	"github.com/Yesveer/wxt-agent/internal/config"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol"
)

// macOS install paths. The agent keeps its config and certs where the Linux
// build does — /etc/vsay — rather than moving to /Library/Application Support,
// so that one set of paths works across both Unix platforms and the existing
// cert-handling code needs no per-OS branching.
const (
	// The label is shared with the self-update restart path — see
	// internal/agent/serviceids.go. Duplicating it is how the two drifted
	// apart before, leaving updates that installed but never took effect.
	macLaunchDaemonLabel = agent.LaunchDaemonLabel
	macLaunchDaemonPlist = "/Library/LaunchDaemons/" + agent.LaunchDaemonLabel + ".plist"
	macConfigPath        = "/etc/vsay/agent.yaml"
	macLogPath           = "/var/log/vsay/agent.log"
	macInstallDir        = "/usr/local/bin"
)

// runConfigureDarwin configures the agent on macOS.
//
// It exists because the Linux flow cannot run here at all: that path calls
// useradd and getent to provision a login account, and neither command exists
// on macOS. More importantly nothing needs provisioning — the daemon runs as
// root under launchd, and the part that touches the user's desktop is the
// session helper, which launchd starts in their own GUI session on demand. So
// this flow creates no accounts and asks for no --linux-user.
func runConfigureDarwin(p configureParams) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("configure must be run with sudo")
	}
	if p.Token == "" || p.Tenant == "" || p.Org == "" || p.Project == "" || p.User == "" || p.Host == "" {
		return fmt.Errorf("missing required flags: token, tenant, org, project, user and host are required")
	}

	fmt.Println("Configuring wxt-agent for macOS...")

	// Install the session helper alongside the agent, under the canonical name
	// whatever the download was called. Leaving it in ~/Downloads would look
	// like a successful install right up until an admin tries to connect — to
	// somebody's live desktop, with a user waiting — so it is worth doing here.
	if err := installSessionHelper(); err != nil {
		fmt.Printf("Warning: %v\n", err)
		fmt.Println("  Remote control will not work until the helper sits next to the agent.")
		fmt.Println("  Download it from the portal's Packages page and re-run configure.")
	}

	grpcURL := p.APIHost
	if grpcURL == "" {
		grpcURL = extractGRPCURL(p.Host)
		fmt.Printf("✓ Using default gRPC address: %s (extracted from --host)\n", grpcURL)
	} else {
		fmt.Printf("✓ Using custom gRPC address: %s\n", grpcURL)
	}

	_ = os.MkdirAll("/etc/vsay", 0755) // #nosec G301 -- readable by local tooling, same as the Linux layout
	_ = os.MkdirAll("/etc/vsay/certs", 0750)
	_ = os.MkdirAll("/var/log/vsay", 0755) // #nosec G301

	caCertFile, caFingerprint, err := fetchAndSaveCA(p.Host)
	if err != nil {
		fmt.Printf("Warning: could not pin CA cert: %v\n", err)
		fmt.Println("  Agent will use the system CA pool for server verification")
	} else if caFingerprint != "" {
		fmt.Println("✓ CA cert pinned — fingerprint (verify out-of-band):")
		fmt.Printf("  SHA256: %s\n", caFingerprint)
		if err := os.WriteFile("/etc/vsay/certs/ca-fingerprint.txt", []byte(caFingerprint), 0644); err != nil { // #nosec G306 -- a fingerprint is meant to be compared and shared
			fmt.Printf("Warning: could not persist CA fingerprint: %v\n", err)
		}
	}

	if err := fetchSignedClientCert(p.Host, p.Token); err != nil {
		return fmt.Errorf("failed to get a signed client cert from the backend: %w", err)
	}
	fmt.Println("✓ Client cert signed by the backend CA (ECDSA P-256)")

	cfg := &config.Config{
		Agent: config.AgentConfig{
			Name: p.MachineName,
			Metadata: map[string]string{
				// Tells the portal this machine offers live remote control
				// rather than a terminal.
				"platform": "macos",
			},
		},
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
		// The console user is recorded even though macOS needs no account
		// provisioning: the web terminal opens a PTY as this user, and without
		// it the agent would try to spawn a shell with no username, no home
		// and no shell path.
		LinuxUser:  consoleUserConfig(),
		Tenant:     config.TenantConfig{ID: p.Tenant},
		Org:        config.OrgConfig{ID: p.Org},
		Project:    config.ProjectConfig{ID: p.Project},
		PortalUser: config.PortalUserConfig{Email: p.User},
		Logging: config.LoggingConfig{
			Level:      "info",
			File:       macLogPath,
			MaxSizeMB:  100,
			MaxBackups: 5,
		},
		Status: "pending_approval",
	}

	// Encrypted with a key derived from IOPlatformUUID, so the file is only
	// decryptable on this Mac.
	if err := config.SaveEncrypted(cfg, macConfigPath); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	fmt.Println("✓ Configuration saved (encrypted) to", macConfigPath)

	if err := setupLaunchDaemon(); err != nil {
		return fmt.Errorf("failed to install the launchd service: %w", err)
	}
	fmt.Println("✓ Agent daemon installed and started")
	fmt.Println("✓ Agent will start automatically on boot")

	fmt.Println("")
	fmt.Println("One more step — macOS will not grant these silently:")
	fmt.Println("  System Settings › Privacy & Security › Screen Recording  → allow wxt-agent-session")
	fmt.Println("  System Settings › Privacy & Security › Accessibility     → allow wxt-agent-session")
	fmt.Println("Remote control cannot capture the screen or move the mouse without both.")
	fmt.Println("")
	fmt.Printf("Check status: sudo launchctl print system/%s\n", macLaunchDaemonLabel)
	fmt.Printf("View logs:    tail -f %s\n", macLogPath)

	return nil
}

// consoleUserConfig describes whoever is logged in at the screen.
//
// macOS creates no account for the agent — unlike Linux, where configure
// provisions one — but the terminal feature still needs somebody to run a
// shell as. /dev/console is owned by the user at the display, which makes
// stat'ing it the simplest reliable way to identify them.
func consoleUserConfig() config.LinuxUserConfig {
	var cfg config.LinuxUserConfig

	fi, err := os.Stat("/dev/console")
	if err != nil {
		fmt.Printf("Warning: could not identify the console user: %v\n", err)
		return cfg
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return cfg
	}

	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		fmt.Printf("Warning: could not look up uid %d: %v\n", st.Uid, err)
		return cfg
	}

	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	cfg = config.LinuxUserConfig{
		Username: u.Username,
		UID:      uid,
		GID:      gid,
		HomeDir:  u.HomeDir,
		Shell:    loginShell(u.Username),
	}
	fmt.Printf("✓ Terminal sessions will run as %s (uid %d, shell %s)\n",
		cfg.Username, cfg.UID, cfg.Shell)
	return cfg
}

// loginShell reads the user's shell from the directory service, falling back to
// zsh — the macOS default since Catalina.
func loginShell(username string) string {
	const fallback = "/bin/zsh"

	out, err := exec.Command("/usr/bin/dscl", ".", "-read", "/Users/"+username, "UserShell").Output()
	if err != nil {
		return fallback
	}
	// Output is "UserShell: /bin/zsh".
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return fallback
	}
	shell := fields[len(fields)-1]
	if !strings.HasPrefix(shell, "/") {
		return fallback
	}
	return shell
}

// installSessionHelper copies the helper next to the agent under the canonical
// name. It accepts any of the names the portal publishes, so an operator who
// curl-ed "wxt-agent-session-macos-arm64" into the same folder does not have to
// know it needed renaming.
func installSessionHelper() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine the agent path: %w", err)
	}

	dst := filepath.Join(macInstallDir, remotecontrol.CanonicalHelperName())

	// The agent carries the helper inside itself, so a normal install needs no
	// second download. A copy sitting next to the binary still wins, which is
	// what lets a developer test a helper they just built.
	if src, err := remotecontrol.FindHelper(filepath.Dir(exe)); err == nil && src != dst {
		if err := copyFile(src, dst, 0755); err != nil {
			return fmt.Errorf("could not install the session helper to %s: %w", dst, err)
		}
		fmt.Printf("✓ Session helper installed to %s\n", dst)
		return nil
	}

	if _, err := remotecontrol.EnsureHelper(macInstallDir); err != nil {
		return err
	}
	fmt.Printf("✓ Session helper installed to %s\n", dst)
	return nil
}

// setupLaunchDaemon writes the LaunchDaemon plist and (re)loads it.
func setupLaunchDaemon() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine the agent path: %w", err)
	}

	// Run from a fixed location rather than wherever configure happened to be
	// invoked from: launchd re-executes this path on every boot, and a binary
	// still sitting in ~/Downloads would break the moment it is tidied away.
	installedPath := filepath.Join(macInstallDir, "wxt-agent")
	if exePath != installedPath {
		if err := copyFile(exePath, installedPath, 0755); err != nil {
			return fmt.Errorf("could not install the agent to %s: %w", installedPath, err)
		}
		fmt.Printf("✓ Agent installed to %s\n", installedPath)
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>start</string>
		<string>--config</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, macLaunchDaemonLabel, installedPath, macConfigPath, macLogPath, macLogPath)

	// LaunchDaemon plists must be owned by root and not group/world writable,
	// or launchd refuses to load them.
	if err := os.WriteFile(macLaunchDaemonPlist, []byte(plist), 0644); err != nil { // #nosec G306 -- launchd requires a world-readable plist
		return fmt.Errorf("could not write %s: %w", macLaunchDaemonPlist, err)
	}
	if err := os.Chown(macLaunchDaemonPlist, 0, 0); err != nil {
		return fmt.Errorf("could not set ownership on %s: %w", macLaunchDaemonPlist, err)
	}

	// Unload any previous instance first; bootstrap fails outright if the
	// label is already loaded, which is the normal case when reconfiguring.
	_ = exec.Command("/bin/launchctl", "bootout", "system/"+macLaunchDaemonLabel).Run()

	if out, err := exec.Command("/bin/launchctl", "bootstrap", "system", macLaunchDaemonPlist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w\n%s", err, out)
	}
	if out, err := exec.Command("/bin/launchctl", "enable", "system/"+macLaunchDaemonLabel).CombinedOutput(); err != nil {
		// Non-fatal: bootstrap already started it, enable only affects whether
		// a disabled label is allowed to run.
		fmt.Printf("Warning: launchctl enable: %v\n%s", err, out)
	}
	return nil
}

// copyFile copies src to dst, replacing dst if it exists.
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src) // #nosec G304 -- src is this process's own executable path
	if err != nil {
		return err
	}
	// Remove first: writing over a running binary gives "text file busy",
	// whereas unlinking leaves the running image alone and creates a new file.
	_ = os.Remove(dst)
	return os.WriteFile(dst, data, mode)
}
