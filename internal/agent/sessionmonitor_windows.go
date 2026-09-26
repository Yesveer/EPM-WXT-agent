//go:build windows

package agent

// listExternalSessions on Windows is intentionally a no-op for now.
//
// Detecting an EXTERNAL RDP login here is subtle: vsay's own remote-desktop feature
// tunnels RDP to 127.0.0.1:3389, so it shows up as an "rdp-tcp" session in `quser`
// exactly like a real external RDP — and `quser` doesn't expose the source IP to tell
// them apart. Doing this correctly needs the TerminalServices-LocalSessionManager
// event log (event 21/25 carry the source network address) plus filtering out
// 127.0.0.1 (vsay's own tunnel). That's a follow-up; returning empty here avoids
// false-positive intrusion alerts on every portal desktop connection.
func listExternalSessions() ([]extSession, error) {
	return nil, nil
}
