//go:build windows

package agent

import (
	"os/exec"
	"strings"
)

// listExternalSessions returns the interactive logins currently on this machine,
// read from `quser`.
//
// This used to return nothing on purpose. EPM's predecessor tunnelled its own
// remote-desktop feature to 127.0.0.1:3389, which `quser` reports as an
// "rdp-tcp" session indistinguishable from a real one — so every portal desktop
// connection would have raised an intrusion alert against itself. EPM removed
// RDP entirely: remote control joins the session the user is already in and
// opens no listener, so an rdp-tcp session now genuinely means somebody came in
// from outside.
//
// `quser` output looks like:
//
//	 USERNAME              SESSIONNAME        ID  STATE   IDLE TIME  LOGON TIME
//	>kaal                  console             1  Active      none   9/26/2026 1:03 PM
//	 admin                 rdp-tcp#2           3  Active         .   9/26/2026 2:11 PM
//
// The leading ">" marks the current session and is not part of the username.
// Note that `quser` does not report the source address; the backend records the
// session without one rather than guessing.
func listExternalSessions() ([]extSession, error) {
	out, err := exec.Command("quser").Output()
	if err != nil {
		// `quser` exits non-zero with "No User exists for *" when nobody is
		// logged in. That is a normal state, not a failure to report.
		if strings.Contains(string(out), "No User exists") {
			return nil, nil
		}
		return nil, err
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return nil, nil
	}

	var sessions []extSession
	for _, line := range lines[1:] { // skip the header
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		s, ok := parseQuserLine(line)
		if !ok {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}
