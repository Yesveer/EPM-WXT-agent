//go:build !windows

package agent

import (
	"os/exec"
	"strings"
	"time"
)

// listExternalSessions returns the interactive logins on this machine, read
// from utmp via `who`.
//
// Both remote (ssh) and local (console) logins are reported, matching the
// Windows side; the protocol field distinguishes them. The agent's own web
// terminal PTYs never touch utmp, so they are never mistaken for logins.
//
// macOS and Linux print `who` differently — see parseWhoLine, which is where
// that lives so it can be tested.
func listExternalSessions() ([]extSession, error) {
	out, err := exec.Command("who").Output()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var sessions []extSession
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		s, ok := parseWhoLine(line, now)
		if !ok {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}
