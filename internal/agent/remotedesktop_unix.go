//go:build !windows

package agent

// remoteDesktopMode reports which browser remote-desktop protocol this machine
// offers. Empty on non-Windows (Linux uses a terminal, not a desktop) so the agent
// omits it from register metadata.
func remoteDesktopMode() string { return "" }

// ensureRemoteDesktop is a no-op on non-Windows.
func (a *Agent) ensureRemoteDesktop() {}
