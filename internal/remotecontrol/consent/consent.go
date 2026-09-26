// Package consent asks the person sitting at the machine whether a remote
// session may start.
//
// This is the feature's safety property, not a nicety: the RFB server calls
// Ask before a single pixel leaves the machine, so a denial — or a user who is
// away from the desk and never answers — means the viewer sees nothing at all.
//
// Like capture and input, the prompt has to be raised from inside the user's
// GUI session. A Windows service in session 0 has no desktop to draw on and a
// macOS LaunchDaemon has no window server connection, so in both cases the
// dialog would be invisible and the user would be consenting to nothing.
package consent

import "time"

// Request describes the session awaiting approval.
type Request struct {
	// AdminName identifies who is asking, as the portal knows them. Showing a
	// real name is the whole point — "someone wants to view your screen" is
	// not a decision anybody can make.
	AdminName string

	// Reason is the optional note the admin supplied, e.g. a ticket number.
	Reason string

	// Timeout is how long to wait for an answer. Expiry counts as a denial:
	// an unattended machine must not become remotely viewable just because
	// nobody was there to say no.
	Timeout time.Duration
}

// Decision is the outcome of a prompt.
type Decision int

const (
	// Denied means the user actively refused.
	Denied Decision = iota
	// Approved means the user agreed.
	Approved
	// TimedOut means nobody answered in time. Callers must treat it exactly
	// like Denied; it is distinguished only so the audit log can record which
	// of the two happened.
	TimedOut
)

func (d Decision) String() string {
	switch d {
	case Approved:
		return "approved"
	case TimedOut:
		return "timed_out"
	default:
		return "denied"
	}
}

// Allowed reports whether the session may proceed.
func (d Decision) Allowed() bool { return d == Approved }

// DefaultTimeout is used when Request.Timeout is zero. Long enough for someone
// to walk back to their desk, short enough that a stale prompt is not sitting
// on the screen an hour later.
const DefaultTimeout = 60 * time.Second

// Ask prompts the user and blocks until they answer or the timeout expires.
func Ask(req Request) (Decision, error) {
	if req.Timeout <= 0 {
		req.Timeout = DefaultTimeout
	}
	if req.AdminName == "" {
		req.AdminName = "An administrator"
	}
	return ask(req)
}

// message builds the prompt body shared by both platforms.
func message(req Request) string {
	s := req.AdminName + " is requesting remote control of this computer.\n\n" +
		"They will be able to see your screen and use your mouse and keyboard. " +
		"You can watch everything they do, and the session will be recorded."
	if req.Reason != "" {
		s += "\n\nReason: " + req.Reason
	}
	return s
}

const promptTitle = "Remote control request"
