//go:build windows

package agent

import (
	"os/exec"
	"strings"
)

// detectWindowsEdition returns "home" or "pro". Windows Home editions cannot host RDP
// (they can only be an RDP client), so those machines fall back to VNC.
//
// Detection uses the registry EditionID: Home editions are "Core", "CoreN",
// "CoreSingleLanguage", "CoreCountrySpecific"; Pro/Enterprise/Education are not.
func detectWindowsEdition() string {
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "EditionID").Output()
	if err != nil {
		return "pro" // assume RDP-capable if we can't tell
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "pro"
	}
	edition := strings.ToLower(fields[len(fields)-1])
	if strings.Contains(edition, "core") {
		return "home"
	}
	return "pro"
}

// remoteDesktopMode reports the browser remote-desktop protocol this machine offers:
// RDP on editions that can host it (Pro/Enterprise/Education/Server), VNC on Home
// (which cannot host RDP at all). Earlier RDP looked unreliable, but the real cause was
// the backend relay dropping RDP's heavy bitmap bursts (shared 100-slot channel + a
// 100ms drop-and-teardown). With a dedicated backpressure lane that's fixed, so we use
// the native protocol per edition — the industry-standard split.
func remoteDesktopMode() string {
	if detectWindowsEdition() == "home" {
		return "vnc"
	}
	return "rdp"
}

// ensureRemoteDesktop is disabled for EPM.
//
// It used to enable the RDP listener (or install a VNC service on Home) on
// every agent start. EPM does not use either: remote control joins the session
// the user is already logged into, through the agent's own RFB server on
// loopback. Leaving this running would open port 3389 on every managed machine
// for a feature nothing connects to — a standing inbound entry point that
// exists only because the code used to need it.
//
// The original body is preserved below; restore it only if RDP comes back as a
// separate feature.
func (a *Agent) ensureRemoteDesktop() {
	a.logger.Debug("Remote-desktop (RDP/VNC) provisioning is disabled — EPM uses live remote control")
}

/*
func (a *Agent) ensureRemoteDesktopOriginal() {
	mode := remoteDesktopMode()
	a.logger.Info("Ensuring remote desktop backend is up", zap.String("mode", mode))

	run := func(name string, args ...string) {
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			a.logger.Warn("Remote-desktop setup step failed",
				zap.String("cmd", name), zap.Error(err), zap.ByteString("out", out))
		}
	}

	if mode == "vnc" {
		run("sc", "config", "tvnserver", "start=", "auto")
		run("net", "start", "tvnserver")
		return
	}

	run("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server`,
		"/v", "fDenyTSConnections", "/t", "REG_DWORD", "/d", "0", "/f")
	run("sc", "config", "TermService", "start=", "auto")
	run("net", "start", "TermService")
	run("sc", "config", "UmRdpService", "start=", "demand")
	run("netsh", "advfirewall", "firewall", "add", "rule",
		"name=VsayRDP", "dir=in", "action=allow", "protocol=TCP", "localport=3389")
}
*/
