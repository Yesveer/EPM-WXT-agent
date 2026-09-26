package ipc

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHandler stands in for the session service.
type fakeHandler struct {
	mu        sync.Mutex
	state     State
	startArg  StartArg
	startErr  error
	stopCalls int
	port      int
}

func (f *fakeHandler) Start(arg StartArg) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startArg = arg
	if f.startErr != nil {
		return Response{State: StateDenied}, f.startErr
	}
	f.state = StateAwaitingConsent
	return Response{State: StateAwaitingConsent, SessionID: "rc_test"}, nil
}

func (f *fakeHandler) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.state = StateIdle
	return nil
}

func (f *fakeHandler) Status() Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Response{State: f.state, Port: f.port, SessionID: "rc_test"}
}

func (f *fakeHandler) setState(s State, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.port = s, port
}

func (f *fakeHandler) capturedArg() StartArg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startArg
}

// inProcessHelper connects a real Helper to the host over loopback, standing
// in for the process a real launch would spawn.
type inProcessHelper struct {
	helper *Helper
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *inProcessHelper) Kill() error {
	p.cancel()
	<-p.done
	return nil
}

// startHost wires a Host to an in-process Helper driven by handler.
func startHost(t *testing.T, handler Handler) (*Host, *inProcessHelper) {
	t.Helper()

	h := NewHost("unused", nil)
	var hp *inProcessHelper

	h.launch = func(_, addr, token string) (helperProcess, error) {
		helper, err := Dial(addr, token, "test-1.0")
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = helper.Serve(ctx, handler)
		}()
		hp = &inProcessHelper{helper: helper, cancel: cancel, done: done}
		return hp, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := h.Start(ctx); err != nil {
		t.Fatalf("Host.Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, hp
}

func TestPingCarriesHelperVersion(t *testing.T) {
	h, _ := startHost(t, &fakeHandler{state: StateIdle})

	resp, err := h.Call(context.Background(), OpPing, nil)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if resp.Version != "test-1.0" {
		t.Errorf("version = %q, want test-1.0", resp.Version)
	}
}

func TestStartForwardsArgumentsAndReturnsAwaitingConsent(t *testing.T) {
	fh := &fakeHandler{state: StateIdle}
	h, _ := startHost(t, fh)

	arg := StartArg{
		AdminName:         "Priya Sharma",
		Reason:            "ticket #4821",
		Password:          "s3cr3t00",
		ConsentTimeoutSec: 45,
		ViewOnly:          true,
	}
	resp, err := h.Call(context.Background(), OpStart, arg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// The state must be awaiting_consent, never active: nothing may be shown
	// to the viewer before the user has agreed.
	if resp.State != StateAwaitingConsent {
		t.Errorf("state = %q, want %q", resp.State, StateAwaitingConsent)
	}
	if resp.SessionID == "" {
		t.Error("no session id returned")
	}

	got := fh.capturedArg()
	if got != arg {
		t.Errorf("helper received %+v, want %+v", got, arg)
	}
}

func TestStartErrorSurfacesWithItsState(t *testing.T) {
	fh := &fakeHandler{state: StateIdle, startErr: errors.New("the user did not approve")}
	h, _ := startHost(t, fh)

	resp, err := h.Call(context.Background(), OpStart, StartArg{AdminName: "Admin"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "did not approve") {
		t.Errorf("error = %v, want it to mention the refusal", err)
	}
	// "denied" and "error" mean different things in the portal, so the
	// handler's state has to survive the trip rather than being flattened.
	if resp == nil || resp.State != StateDenied {
		t.Errorf("state = %v, want %q", resp, StateDenied)
	}
}

func TestStatusReportsThePort(t *testing.T) {
	fh := &fakeHandler{state: StateIdle}
	h, _ := startHost(t, fh)

	fh.setState(StateActive, 54321)

	resp, err := h.Call(context.Background(), OpStatus, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if resp.State != StateActive {
		t.Errorf("state = %q, want active", resp.State)
	}
	if resp.Port != 54321 {
		t.Errorf("port = %d, want 54321", resp.Port)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	fh := &fakeHandler{state: StateActive}
	h, _ := startHost(t, fh)

	for i := 0; i < 3; i++ {
		if _, err := h.Call(context.Background(), OpStop, nil); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
	}
	fh.mu.Lock()
	calls := fh.stopCalls
	fh.mu.Unlock()
	if calls != 3 {
		t.Errorf("handler saw %d stops, want 3", calls)
	}
}

func TestEventsReachTheHost(t *testing.T) {
	fh := &fakeHandler{state: StateIdle}
	h, hp := startHost(t, fh)

	got := make(chan Event, 4)
	h.OnEvent(func(e Event) { got <- e })

	want := Event{
		Kind:      EvtStateChanged,
		State:     StateDenied,
		SessionID: "rc_test",
		Message:   "the user did not approve the session",
	}
	if err := hp.helper.SendEvent(want); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	select {
	case e := <-got:
		if e != want {
			t.Errorf("event = %+v, want %+v", e, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the event never arrived")
	}
}

func TestBadTokenIsRejected(t *testing.T) {
	h := NewHost("unused", nil)
	dialErr := make(chan error, 1)

	h.launch = func(_, addr, _ string) (helperProcess, error) {
		// Present a token the host never issued.
		_, err := Dial(addr, "0000000000000000000000000000000000000000000000000000000000000000", "test")
		dialErr <- err
		return nopProcess{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer h.Close()

	// Start will time out waiting for a helper that never authenticates; that
	// is the point.
	go func() { _ = h.Start(ctx) }()

	select {
	case err := <-dialErr:
		if err == nil {
			t.Fatal("a helper with the wrong token was accepted")
		}
		if !strings.Contains(err.Error(), "bad token") {
			t.Errorf("error = %v, want it to mention the token", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handshake never completed")
	}
}

func TestCallFailsWhenTheHelperIsGone(t *testing.T) {
	fh := &fakeHandler{state: StateIdle}
	h, hp := startHost(t, fh)

	_ = hp.Kill()

	// Give the host a moment to notice the closed connection.
	deadline := time.Now().Add(3 * time.Second)
	for h.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := h.Call(context.Background(), OpStatus, nil); err == nil {
		t.Fatal("a call succeeded with no helper attached")
	}
}

func TestOversizedFrameIsRefused(t *testing.T) {
	// A frame past the limit must be rejected before it is written, or a
	// confused peer could make the other side allocate without bound.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	// Drain the far end so a legitimate write would not simply block.
	go func() { _, _ = io.Copy(io.Discard, right) }()

	c := newConn(left)
	huge := strings.Repeat("a", maxFrameBytes+1)

	err := c.writeFrame(frame{Event: &Event{Kind: huge}})
	if err == nil {
		t.Fatal("an oversized frame was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to mention the limit", err)
	}
}

type nopProcess struct{}

func (nopProcess) Kill() error { return nil }

// TestActiveEventCarriesThePort is the regression test for a bug that made
// every session unusable while looking perfectly healthy.
//
// The RFB port does not exist when OpStart returns — the server is only
// started after the user consents — so the port can ONLY reach the backend on
// the state-changed event. When Event had no Port field the backend held zero
// forever and refused each viewer with "no approved session is ready", while
// the portal cheerfully reported the session as active.
func TestActiveEventCarriesThePort(t *testing.T) {
	fh := &fakeHandler{state: StateIdle}
	h, hp := startHost(t, fh)

	got := make(chan Event, 4)
	h.OnEvent(func(e Event) { got <- e })

	want := Event{
		Kind:      EvtStateChanged,
		State:     StateActive,
		SessionID: "rc_test",
		Port:      54321,
		Width:     1920,
		Height:    1080,
	}
	if err := hp.helper.SendEvent(want); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	select {
	case e := <-got:
		if e.Port != want.Port {
			t.Errorf("port = %d, want %d — the backend cannot reach the RFB server without it", e.Port, want.Port)
		}
		if e.Width != want.Width || e.Height != want.Height {
			t.Errorf("size = %dx%d, want %dx%d", e.Width, e.Height, want.Width, want.Height)
		}
		if e != want {
			t.Errorf("event = %+v, want %+v", e, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the event never arrived")
	}
}
