package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// Handler is the helper-side behaviour the daemon drives. The session service
// implements it.
type Handler interface {
	// Start raises the consent prompt and, once approved, brings up the RFB
	// server. It returns as soon as the prompt is showing — the answer is
	// reported later through an event and through Status — so that the portal
	// can display "waiting for the user" instead of hanging.
	Start(StartArg) (Response, error)

	// Stop ends the active session. Stopping when nothing is running is not an
	// error; it is what a retry looks like.
	Stop() error

	// Status reports the current state.
	Status() Response
}

// Helper is the session-helper side of the connection.
type Helper struct {
	c       *conn
	version string
}

// Dial connects back to the daemon and authenticates.
func Dial(addr, token, version string) (*Helper, error) {
	nc, err := net.DialTimeout("tcp", addr, handshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("ipc: dialling the agent at %s: %w", addr, err)
	}

	c := newConn(nc)
	if err := clientHandshake(c, token, version, os.Getpid()); err != nil {
		_ = c.close()
		return nil, err
	}
	return &Helper{c: c, version: version}, nil
}

// SendEvent pushes an unsolicited event to the daemon.
func (h *Helper) SendEvent(e Event) error {
	return h.c.writeFrame(frame{Event: &e})
}

// Serve handles daemon requests until the connection drops or ctx is done.
func (h *Helper) Serve(ctx context.Context, handler Handler) error {
	go func() {
		<-ctx.Done()
		_ = h.c.close()
	}()

	for {
		f, err := h.c.readFrame()
		if err != nil {
			if isClosed(err) {
				return nil
			}
			return err
		}
		if f.Req == nil {
			continue // responses and events only travel the other way
		}

		resp := h.handle(handler, *f.Req)
		resp.ID = f.Req.ID
		if err := h.c.writeFrame(frame{Resp: &resp}); err != nil {
			if isClosed(err) {
				return nil
			}
			return err
		}
	}
}

func (h *Helper) handle(handler Handler, req Request) Response {
	fail := func(err error) Response {
		return Response{OK: false, Error: err.Error(), State: StateError}
	}

	switch req.Op {
	case OpPing:
		return Response{OK: true, Version: h.version}

	case OpStart:
		var arg StartArg
		if err := remarshal(req.Arg, &arg); err != nil {
			return fail(fmt.Errorf("bad start arguments: %w", err))
		}
		resp, err := handler.Start(arg)
		if err != nil {
			// Keep whatever state the handler decided on — "denied" and
			// "error" mean different things to the portal.
			if resp.State == "" {
				resp.State = StateError
			}
			resp.OK = false
			resp.Error = err.Error()
			return resp
		}
		resp.OK = true
		return resp

	case OpStop:
		if err := handler.Stop(); err != nil {
			return fail(err)
		}
		return Response{OK: true, State: StateIdle}

	case OpStatus:
		resp := handler.Status()
		resp.OK = true
		return resp

	default:
		return fail(fmt.Errorf("unknown op %q", req.Op))
	}
}

// Close drops the connection.
func (h *Helper) Close() error { return h.c.close() }

// remarshal re-decodes a generically-decoded `any` into a concrete type.
// Request.Arg arrives as map[string]any because the envelope is decoded before
// the op is known.
func remarshal(src any, dst any) error {
	if src == nil {
		return nil
	}
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// KeepAlive pings the daemon periodically so that a half-open connection — a
// daemon that died without closing its socket — is noticed rather than leaving
// the helper running forever with nobody driving it.
func (h *Helper) KeepAlive(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.c.writeFrame(frame{Event: &Event{Kind: "keepalive"}}); err != nil {
				return
			}
		}
	}
}
