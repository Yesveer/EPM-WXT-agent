package agent

import (
	"strings"
	"time"
)

// Parsing of `who` output lives here, away from the Unix-only command
// invocation, so it can be tested on any build machine — the same reason
// quser.go exists for Windows.

// parseWhoLine turns one `who` row into an extSession.
//
// The two platforms print different things, and getting this wrong is silent:
// a failed timestamp parse just yields the current time, so every login looks
// like it happened the moment the agent noticed it.
//
//	Linux:  kaal  pts/3    2026-08-12 15:46 (203.0.113.5)
//	macOS:  kaal  ttys000  Sep 26 18:51
//	macOS:  kaal  ttys001  Sep 26 18:52 (203.0.113.5)
//
// A session with a source host is remote (ssh); one without is someone at the
// machine itself (console). Both are reported, matching Windows, and the
// protocol field is what tells them apart.
func parseWhoLine(line string, now time.Time) (extSession, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return extSession{}, false
	}
	user, tty := f[0], f[1]

	// The source host, when present, is the final field in parentheses.
	host := ""
	last := f[len(f)-1]
	if strings.HasPrefix(last, "(") && strings.HasSuffix(last, ")") {
		host = strings.Trim(last, "()")
		f = f[:len(f)-1] // drop it so the timestamp is the tail
	}

	// An X display such as ":0" is the local screen, not a network source.
	if strings.HasPrefix(host, ":") {
		host = ""
	}

	protocol := "console"
	if host != "" {
		protocol = "ssh"
	}

	loginTime := now
	if t, ok := parseWhoTime(f[2:], now); ok {
		loginTime = t
	}

	return extSession{
		OSUser:    user,
		Line:      tty,
		Protocol:  protocol,
		SourceIP:  host,
		LoginTime: loginTime,
	}, true
}

// parseWhoTime reads the timestamp fields, which differ by platform.
func parseWhoTime(fields []string, now time.Time) (time.Time, bool) {
	if len(fields) < 2 {
		return time.Time{}, false
	}

	// Linux: "2026-08-12 15:46" — a complete date.
	if t, err := time.ParseInLocation("2006-01-02 15:04", fields[0]+" "+fields[1], time.Local); err == nil {
		return t, true
	}

	// macOS: "Sep 26 18:51" — no year, so the current one is assumed.
	if len(fields) >= 3 {
		stamp := strings.Join(fields[:3], " ")
		if t, err := time.ParseInLocation("Jan 2 15:04", stamp, time.Local); err == nil {
			t = t.AddDate(now.Year(), 0, 0)
			// A login "in the future" means the year rolled over since — a
			// December login read in January. Without this, every such session
			// is dated eleven months ahead.
			if t.After(now.Add(24 * time.Hour)) {
				t = t.AddDate(-1, 0, 0)
			}
			return t, true
		}
	}

	return time.Time{}, false
}
