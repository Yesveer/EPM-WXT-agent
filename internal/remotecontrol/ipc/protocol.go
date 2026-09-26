// Package ipc connects the agent daemon to the session helper.
//
// Remote control needs two processes. Screen capture, input injection and the
// consent prompt all have to happen inside the user's interactive desktop: a
// Windows service in session 0 cannot touch it, and a macOS LaunchDaemon can
// never be granted the Screen Recording or Accessibility rights. The daemon
// therefore launches a helper INTO the user's session and drives it from here.
//
// The daemon listens on loopback and the helper dials back, rather than the
// other way around. That ordering matters: the daemon creates the listener and
// generates a one-time token, then passes the address and token to the helper
// on its command line, so the secret never touches disk and there is no
// well-known endpoint for another local process to squat on.
package ipc

import "time"

// Op is a command from the daemon to the helper.
type Op string

const (
	// OpPing checks the helper is alive and reports its version.
	OpPing Op = "ping"
	// OpStart asks for consent and, if granted, starts an RFB server.
	OpStart Op = "start"
	// OpStop ends the active session.
	OpStop Op = "stop"
	// OpStatus reports the current session state.
	OpStatus Op = "status"
)

// Request travels daemon -> helper.
type Request struct {
	ID  uint64 `json:"id"`
	Op  Op     `json:"op"`
	Arg any    `json:"arg,omitempty"`
}

// StartArg is the payload of OpStart.
type StartArg struct {
	// AdminName is shown in the consent prompt. It comes from the portal's
	// authenticated identity, not from anything the machine can forge.
	AdminName string `json:"admin_name"`
	// Reason is the optional note the admin gave, e.g. a ticket reference.
	Reason string `json:"reason,omitempty"`
	// Password, when set, is the VNC password the viewer must present. It is
	// generated per session by the daemon.
	Password string `json:"password,omitempty"`
	// ConsentTimeoutSec bounds the wait for an answer. Zero uses the default.
	ConsentTimeoutSec int `json:"consent_timeout_sec,omitempty"`
	// ViewOnly starts a session the viewer cannot type or click in.
	ViewOnly bool `json:"view_only,omitempty"`
	// IdleTimeoutMinutes ends the session after this long with no viewer
	// input. Zero uses the built-in default.
	IdleTimeoutMinutes int `json:"idle_timeout_minutes,omitempty"`
}

// State is the helper's session state. It is deliberately explicit about WHY a
// session is not running, because "waiting for the user" and "the user said
// no" need completely different things shown in the portal.
type State string

const (
	StateIdle            State = "idle"
	StateAwaitingConsent State = "awaiting_consent"
	StateActive          State = "active"
	StateDenied          State = "denied"
	StateTimedOut        State = "timed_out"
	StateError           State = "error"
)

// Terminal reports whether no further transition will happen without a new
// OpStart.
func (s State) Terminal() bool {
	switch s {
	case StateDenied, StateTimedOut, StateError, StateIdle:
		return true
	}
	return false
}

// Response travels helper -> daemon.
type Response struct {
	ID    uint64 `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// Version identifies the helper binary, answering OpPing.
	Version string `json:"version,omitempty"`

	State State `json:"state,omitempty"`
	// Port is the loopback port the RFB server listens on once State is
	// active. It is only ever reachable through the agent's gRPC tunnel.
	Port int `json:"port,omitempty"`
	// SessionID correlates the helper, the daemon and the portal's audit log.
	SessionID string `json:"session_id,omitempty"`
	// Viewer is the remote address currently connected, for display.
	Viewer string `json:"viewer,omitempty"`
	// Width and Height describe the captured desktop.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// StartedAt is when the session became active.
	StartedAt time.Time `json:"started_at,omitempty"`
}

// Event travels helper -> daemon unsolicited, so the portal can react to
// something the user did (denying consent, closing the session) without
// polling for it.
type Event struct {
	// Kind is one of the Evt* constants.
	Kind string `json:"kind"`
	// ID is zero: events are distinguished from responses by this field being
	// absent, which is why Response.ID starts at one.
	SessionID string `json:"session_id,omitempty"`
	State     State  `json:"state,omitempty"`
	Message   string `json:"message,omitempty"`
	Viewer    string `json:"viewer,omitempty"`

	// Port is the loopback port the RFB server ended up on, and Width/Height
	// the captured desktop size.
	//
	// These MUST travel on the state-changed event, not only in the reply to
	// OpStart. The port does not exist yet when Start returns — the RFB server
	// is only brought up after the user consents — so a backend that learned
	// the port only from that reply would hold zero forever and refuse every
	// viewer with "no approved session is ready".
	Port   int `json:"port,omitempty"`
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Event kinds.
const (
	EvtStateChanged  = "state_changed"
	EvtViewerJoined  = "viewer_joined"
	EvtViewerLeft    = "viewer_left"
	EvtClipboard     = "clipboard"
	EvtHelperStopped = "helper_stopped"
)

// ClipboardEvent carries text the user copied on the machine, so it can be
// pushed to the viewer.
type ClipboardEvent struct {
	Text string `json:"text"`
}

// frame is the envelope actually written on the wire. Exactly one of the three
// fields is set; the reader dispatches on which.
type frame struct {
	Req   *Request       `json:"req,omitempty"`
	Resp  *Response      `json:"resp,omitempty"`
	Event *Event         `json:"event,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}
