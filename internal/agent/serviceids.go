package agent

// Service identifiers, defined once.
//
// These were previously written out separately in the configure flow and in
// the self-update restart, and the two drifted: configure registered a launchd
// label of io.wxt.agent while the restart asked launchd for com.vsay.agent,
// and configure registered a Windows scheduled task while the restart tried to
// stop a Windows service of a different name. Both failures were silent — the
// update downloaded and installed, the restart did nothing, and the machine
// carried on running the old binary while reporting success.
//
// Anything that starts, stops or restarts the agent must use these.
const (
	// LaunchDaemonLabel is the macOS launchd label.
	LaunchDaemonLabel = "io.wxt.agent"

	// WindowsTaskName is the Windows scheduled task the agent runs under.
	// It is a scheduled task rather than a service because the agent is a
	// console program with no Service Control Manager integration.
	WindowsTaskName = "VsayAgent"

	// SystemdUnit is the Linux systemd unit.
	SystemdUnit = "wxt-agent"
)
