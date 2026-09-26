package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// helperConnectTimeout bounds how long Start waits for the launched helper to
// dial back. A helper that never connects usually means no interactive session
// exists — nobody is logged in — which is a normal state, not a crash.
const helperConnectTimeout = 30 * time.Second

// callTimeout bounds an ordinary request. OpStart is excluded: it returns as
// soon as the prompt is raised, and the wait for an answer happens through
// status polling instead of by blocking here.
const callTimeout = 15 * time.Second

// Host is the daemon side of the helper connection.
type Host struct {
	logger     *zap.Logger
	helperPath string

	ln    net.Listener
	token string

	mu        sync.Mutex
	helper    *conn
	version   string
	helperPID int
	proc      helperProcess
	pending   map[uint64]chan *Response
	connected chan struct{}

	launch func(helperPath, addr, token string) (helperProcess, error)

	nextID  atomic.Uint64
	onEvent atomic.Pointer[func(Event)]

	closed atomic.Bool
}

// NewHost prepares a host for the helper binary at helperPath.
func NewHost(helperPath string, logger *zap.Logger) *Host {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Host{
		logger:     logger,
		helperPath: helperPath,
		pending:    make(map[uint64]chan *Response),
		connected:  make(chan struct{}),
		// launch is indirected so tests can stand in an in-process helper
		// instead of spawning a real one — launching into a GUI session is
		// exactly the part that cannot run on a build machine.
		launch: launchHelper,
	}
}

// OnEvent registers the callback for unsolicited helper events. It replaces
// any previous callback.
func (h *Host) OnEvent(fn func(Event)) {
	h.onEvent.Store(&fn)
}

// Start listens on loopback, launches the helper into the interactive session
// and waits for it to connect back.
func (h *Host) Start(ctx context.Context) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	h.token = token

	// Loopback only. The helper is a local process, and binding anywhere else
	// would expose the control channel to the network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("ipc: listening for the session helper: %w", err)
	}
	h.ln = ln

	go h.acceptLoop(ctx)

	proc, err := h.launch(h.helperPath, ln.Addr().String(), token)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("ipc: launching the session helper: %w", err)
	}
	h.mu.Lock()
	h.proc = proc
	h.mu.Unlock()

	select {
	case <-h.connected:
		h.mu.Lock()
		v, pid := h.version, h.helperPID
		h.mu.Unlock()
		h.logger.Info("Session helper connected",
			zap.String("version", v), zap.Int("pid", pid))
		return nil
	case <-time.After(helperConnectTimeout):
		return errors.New("ipc: the session helper did not connect — is anyone logged in?")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Host) acceptLoop(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = h.ln.Close()
	}()

	for {
		nc, err := h.ln.Accept()
		if err != nil {
			if !isClosed(err) && ctx.Err() == nil {
				h.logger.Warn("Session helper listener failed", zap.Error(err))
			}
			return
		}

		c := newConn(nc)
		hello, err := serverHandshake(c, h.token)
		if err != nil {
			h.logger.Warn("Rejected a session-helper connection", zap.Error(err))
			_ = c.close()
			continue
		}

		h.mu.Lock()
		// Only one helper at a time. A second one means the first is stale, so
		// the newcomer wins and the old connection is dropped.
		if h.helper != nil {
			_ = h.helper.close()
		}
		h.helper = c
		h.version = hello.Version
		h.helperPID = hello.PID
		select {
		case <-h.connected:
		default:
			close(h.connected)
		}
		h.mu.Unlock()

		go h.readLoop(c)
	}
}

func (h *Host) readLoop(c *conn) {
	defer func() {
		h.mu.Lock()
		if h.helper == c {
			h.helper = nil
		}
		// Every in-flight call is now unanswerable; fail them rather than let
		// the callers block until their own deadlines.
		for id, ch := range h.pending {
			close(ch)
			delete(h.pending, id)
		}
		h.mu.Unlock()
		_ = c.close()

		if !h.closed.Load() {
			h.dispatch(Event{Kind: EvtHelperStopped,
				Message: "the session helper disconnected"})
		}
	}()

	for {
		f, err := c.readFrame()
		if err != nil {
			if !isClosed(err) {
				h.logger.Debug("Session helper read ended", zap.Error(err))
			}
			return
		}

		switch {
		case f.Resp != nil:
			h.mu.Lock()
			ch, ok := h.pending[f.Resp.ID]
			if ok {
				delete(h.pending, f.Resp.ID)
			}
			h.mu.Unlock()
			if ok {
				ch <- f.Resp
				close(ch)
			}
		case f.Event != nil:
			h.dispatch(*f.Event)
		}
	}
}

func (h *Host) dispatch(e Event) {
	if fn := h.onEvent.Load(); fn != nil {
		(*fn)(e)
	}
}

// Call sends one request and waits for its response.
func (h *Host) Call(ctx context.Context, op Op, arg any) (*Response, error) {
	h.mu.Lock()
	c := h.helper
	h.mu.Unlock()
	if c == nil {
		return nil, errors.New("ipc: the session helper is not connected")
	}

	// IDs start at 1 so that a zero ID unambiguously means "not a response".
	id := h.nextID.Add(1)
	ch := make(chan *Response, 1)

	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()

	if err := c.writeFrame(frame{Req: &Request{ID: id, Op: op, Arg: arg}}); err != nil {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, fmt.Errorf("ipc: sending %s: %w", op, err)
	}

	timer := time.NewTimer(callTimeout)
	defer timer.Stop()

	select {
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return nil, errors.New("ipc: the session helper disconnected mid-request")
		}
		if !resp.OK && resp.Error != "" {
			return resp, errors.New(resp.Error)
		}
		return resp, nil
	case <-timer.C:
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, fmt.Errorf("ipc: %s timed out", op)
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Connected reports whether a helper is currently attached.
func (h *Host) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.helper != nil
}

// Close stops the listener and terminates the helper.
func (h *Host) Close() error {
	h.closed.Store(true)

	h.mu.Lock()
	c, proc := h.helper, h.proc
	h.helper, h.proc = nil, nil
	h.mu.Unlock()

	if c != nil {
		_ = c.close()
	}
	if h.ln != nil {
		_ = h.ln.Close()
	}
	if proc != nil {
		_ = proc.Kill()
	}
	return nil
}

// helperProcess is as much of the launched helper as the host needs. The two
// platforms create it in completely different ways — exec on macOS,
// CreateProcessAsUser on Windows — so only the ability to stop it is shared.
type helperProcess interface {
	// Kill terminates the helper and releases any handles held for it.
	Kill() error
}
