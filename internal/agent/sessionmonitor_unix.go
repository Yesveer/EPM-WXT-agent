//go:build !windows

package agent

import (
	"os/exec"
	"strings"
	"time"
)

// listExternalSessions returns the current remote (SSH) login sessions by reading
// utmp via `who`. Local console logins and vsay's own PTYs (which don't touch utmp)
// are excluded — only sessions with a remote source host are treated as external.
//
// `who` line: "amitesh-ls pts/3  2026-08-12 15:46 (203.0.113.5)"
func listExternalSessions() ([]extSession, error) {
	out, err := exec.Command("who").Output()
	if err != nil {
		return nil, err
	}

	var sessions []extSession
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		user, tty := f[0], f[1]

		// The source host, if present, is the last field wrapped in parentheses.
		host := ""
		last := f[len(f)-1]
		if strings.HasPrefix(last, "(") && strings.HasSuffix(last, ")") {
			host = strings.TrimSuffix(strings.TrimPrefix(last, "("), ")")
		}
		// No host or an X display (":0") = local console → not an external SSH login.
		if host == "" || strings.HasPrefix(host, ":") {
			continue
		}

		loginTime := time.Now()
		if len(f) >= 4 {
			if t, e := time.ParseInLocation("2006-01-02 15:04", f[2]+" "+f[3], time.Local); e == nil {
				loginTime = t
			}
		}

		sessions = append(sessions, extSession{
			OSUser:    user,
			Line:      tty,
			Protocol:  "ssh",
			SourceIP:  host,
			LoginTime: loginTime,
		})
	}
	return sessions, nil
}
