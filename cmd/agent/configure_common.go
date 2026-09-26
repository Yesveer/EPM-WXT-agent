package main

// configureParams carries the parsed configure flags into the OS-specific configure
// flow. Shared by both platforms; only the Windows flow (configure_windows.go) reads
// the WindowsUser/WindowsPassword fields.
type configureParams struct {
	Token           string
	Tenant          string
	Org             string
	Project         string
	User            string
	Host            string
	APIHost         string
	MachineName     string
	WindowsUser     string
	WindowsPassword string
	TunnelURL       string
}
