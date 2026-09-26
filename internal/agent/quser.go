package agent

import (
	"strings"
	"time"
)

// Parsing of `quser` output lives here rather than beside the Windows-only
// command invocation: the column handling is the subtle part and deserves
// tests that run on any build machine, not only on Windows.

// parseQuserLine turns one `quser` row into an extSession.
//
// The columns are fixed-width and a disconnected session leaves SESSIONNAME
// blank, which shifts everything left — so the row is parsed by locating the
// numeric session ID rather than by counting fields.
func parseQuserLine(line string) (extSession, bool) {
	// Strip the ">" that marks the session running this command.
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ">"))
	fields := strings.Fields(trimmed)
	if len(fields) < 3 {
		return extSession{}, false
	}

	user := fields[0]

	// Find the session ID: the first purely numeric field after the username.
	idIdx := -1
	for i := 1; i < len(fields); i++ {
		if isAllDigits(fields[i]) {
			idIdx = i
			break
		}
	}
	if idIdx == -1 {
		return extSession{}, false
	}

	sessionName := ""
	if idIdx > 1 {
		sessionName = fields[1]
	}

	// Everything after STATE and IDLE TIME is the logon timestamp. Its format
	// follows the machine's locale, so a parse failure falls back to "now"
	// rather than dropping an otherwise valid session.
	logon := time.Now()
	if idIdx+3 < len(fields) {
		if t, err := parseQuserTime(strings.Join(fields[idIdx+3:], " ")); err == nil {
			logon = t
		}
	}

	protocol := "console"
	if strings.HasPrefix(strings.ToLower(sessionName), "rdp-tcp") {
		protocol = "rdp"
	}

	return extSession{
		OSUser:    user,
		Line:      sessionName + "#" + fields[idIdx],
		Protocol:  protocol,
		LoginTime: logon,
	}, true
}

// quserTimeLayouts covers the formats `quser` emits across common locales.
var quserTimeLayouts = []string{
	"1/2/2006 3:04 PM",
	"1/2/2006 15:04",
	"02/01/2006 15:04",
	"2006-01-02 15:04",
}

func parseQuserTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var lastErr error
	for _, layout := range quserTimeLayouts {
		// Local time: `quser` reports in the machine's timezone.
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
