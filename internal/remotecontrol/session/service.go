// Package session runs a remote-control session inside the user's desktop.
//
// It is the piece that joins everything together: it asks for consent, brings
// up screen capture and input injection, serves RFB on loopback, and keeps the
// clipboard in sync in both directions. It lives in the session helper
// process, never in the agent daemon — see the package comment on ipc for why
// that split is mandatory rather than stylistic.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/capture"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol/clipboard"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol/consent"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol/input"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol/ipc"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
	"go.uber.org/zap"
)

// clipboardPoll is how often the local clipboard is checked for a change the
// user made. Neither OS offers a notification a background process can await
// without a run loop, so this is a poll — kept cheap by comparing a change
// counter rather than re-reading the contents.
const clipboardPoll = 700 * time.Millisecond

// defaultIdleMinutes matches the portal's own default for terminal sessions,
// so an organisation that never changes the setting gets the same behaviour
// from both features.
const defaultIdleMinutes = 2

// maxIdleMinutes mirrors the bound the settings page enforces. It is re-checked
// here because this value arrives over the network and the agent should not
// trust the portal to have validated it.
const maxIdleMinutes = 240

// idleTimeout converts the configured minutes into a duration, clamping to the
// same range the portal offers.
func idleTimeout(minutes int) time.Duration {
	if minutes <= 0 {
		minutes = defaultIdleMinutes
	}
	if minutes > maxIdleMinutes {
		minutes = maxIdleMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// maxFPS caps the capture rate. A helpdesk session is mostly a static screen
// with bursts of activity; past this the extra frames cost CPU on the user's
// machine — which they WILL notice — without helping the admin.
const maxFPS = 20

// Service owns at most one session at a time.
type Service struct {
	logger *zap.Logger
	// emit publishes events to the daemon. Set by the helper at startup.
	emit func(ipc.Event)

	mu      sync.Mutex
	state   ipc.State
	sess    *live
	lastErr string
}

// live is one running session and everything it owns.
type live struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}

	ln     net.Listener
	srv    *rfb.Server
	cap    capture.Capturer
	inj    input.Injector
	clip   clipboard.Clipboard
	port   int
	w, h   int
	viewer string
	start  time.Time
}

// New creates an idle Service.
func New(logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{logger: logger, state: ipc.StateIdle}
}

// SetEmitter installs the event sink. It must be called before Start.
func (s *Service) SetEmitter(fn func(ipc.Event)) { s.emit = fn }

func (s *Service) publish(e ipc.Event) {
	if s.emit != nil {
		s.emit(e)
	}
}

func (s *Service) setState(st ipc.State, sessionID, msg string) {
	s.mu.Lock()
	s.state = st
	if st == ipc.StateError {
		s.lastErr = msg
	}
	s.mu.Unlock()
	s.publish(ipc.Event{Kind: ipc.EvtStateChanged, State: st, SessionID: sessionID, Message: msg})
}

// Start implements ipc.Handler.
//
// It returns as soon as the consent prompt is on screen. The prompt blocks for
// as long as the user takes to answer, and holding the daemon's request open
// for that whole time would make an unanswered prompt look like a hung agent,
// so the answer is reported through a state change instead.
func (s *Service) Start(arg ipc.StartArg) (ipc.Response, error) {
	s.mu.Lock()
	if s.sess != nil || s.state == ipc.StateAwaitingConsent {
		st := s.state
		s.mu.Unlock()
		return ipc.Response{State: st}, errors.New("a remote-control session is already in progress")
	}
	s.state = ipc.StateAwaitingConsent
	s.mu.Unlock()

	id, err := newSessionID()
	if err != nil {
		s.setState(ipc.StateError, "", err.Error())
		return ipc.Response{State: ipc.StateError}, err
	}

	timeout := consent.DefaultTimeout
	if arg.ConsentTimeoutSec > 0 {
		timeout = time.Duration(arg.ConsentTimeoutSec) * time.Second
	}

	go s.awaitConsentAndRun(id, arg, timeout)

	return ipc.Response{State: ipc.StateAwaitingConsent, SessionID: id}, nil
}

func (s *Service) awaitConsentAndRun(id string, arg ipc.StartArg, timeout time.Duration) {
	s.logger.Info("Asking the user for consent",
		zap.String("session", id), zap.String("admin", arg.AdminName))

	decision, err := consent.Ask(consent.Request{
		AdminName: arg.AdminName,
		Reason:    arg.Reason,
		Timeout:   timeout,
	})
	if err != nil {
		s.logger.Warn("Consent prompt failed", zap.String("session", id), zap.Error(err))
		s.reset(ipc.StateError, id, err.Error())
		return
	}
	if !decision.Allowed() {
		s.logger.Info("Remote control refused",
			zap.String("session", id), zap.String("decision", decision.String()))
		st := ipc.StateDenied
		if decision == consent.TimedOut {
			st = ipc.StateTimedOut
		}
		s.reset(st, id, "the user did not approve the session")
		return
	}

	if err := s.run(id, arg); err != nil {
		s.logger.Error("Could not start the session", zap.String("session", id), zap.Error(err))
		s.reset(ipc.StateError, id, err.Error())
	}
}

// reset returns the service to a terminal state with no session attached.
func (s *Service) reset(st ipc.State, id, msg string) {
	s.mu.Lock()
	s.sess = nil
	s.mu.Unlock()
	s.setState(st, id, msg)
}

// run brings up capture, input and the RFB listener. Consent has already been
// given by the time this is called.
func (s *Service) run(id string, arg ipc.StartArg) error {
	cap, err := capture.New()
	if err != nil {
		return err
	}

	var inj input.Injector
	if arg.ViewOnly {
		// A view-only session still needs a Sink for the RFB server; a
		// no-op injector is what makes "the admin cannot touch anything"
		// true at the lowest level rather than by hiding a button.
		inj = noopInjector{}
	} else {
		inj, err = input.New()
		if err != nil {
			_ = cap.Close()
			return err
		}
	}

	clip, err := clipboard.New()
	if err != nil {
		// Clipboard sync is a convenience; losing it should not cost the
		// session that someone just approved.
		s.logger.Warn("Clipboard sync unavailable", zap.Error(err))
		clip = nil
	}

	// Loopback only. Nothing outside this machine can reach the RFB port; the
	// only route in is the agent's authenticated gRPC tunnel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = cap.Close()
		_ = inj.Close()
		return fmt.Errorf("listening for the viewer: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	l := &live{
		id:     id,
		cancel: cancel,
		done:   make(chan struct{}),
		ln:     ln,
		cap:    cap,
		inj:    inj,
		clip:   clip,
		port:   port,
		start:  time.Now(),
	}
	l.w, l.h = cap.Size()

	var clipWriter input.ClipboardWriter
	if clip != nil {
		clipWriter = clipboardWriter{clip}
	}

	srv, err := rfb.New(rfb.Config{
		Name:        "wxt-agent",
		Password:    arg.Password,
		Source:      capture.NewSource(cap),
		Sink:        input.NewSink(inj, clipWriter),
		Logger:      s.logger,
		MaxFPS:      maxFPS,
		IdleTimeout: idleTimeout(arg.IdleTimeoutMinutes),
		OnConnect: func(remote string) error {
			s.mu.Lock()
			if s.sess != nil {
				s.sess.viewer = remote
			}
			s.mu.Unlock()
			s.publish(ipc.Event{Kind: ipc.EvtViewerJoined, SessionID: id, Viewer: remote})
			return nil
		},
		OnDisconnect: func(remote string) {
			s.publish(ipc.Event{Kind: ipc.EvtViewerLeft, SessionID: id, Viewer: remote})
			// The viewer closing the tab ends the session. Leaving capture
			// running afterwards would keep the user's screen live for a
			// consent they gave to a session that is over.
			s.Stop()
		},
	})
	if err != nil {
		cancel()
		_ = ln.Close()
		_ = cap.Close()
		_ = inj.Close()
		return err
	}
	l.srv = srv

	s.mu.Lock()
	s.sess = l
	s.state = ipc.StateActive
	s.mu.Unlock()

	go func() {
		defer close(l.done)
		if err := srv.Serve(ctx, ln); err != nil && ctx.Err() == nil {
			s.logger.Warn("RFB server stopped", zap.String("session", id), zap.Error(err))
		}
	}()

	if clip != nil {
		go s.watchClipboard(ctx, srv, clip)
	}

	s.logger.Info("Remote-control session active",
		zap.String("session", id),
		zap.Int("port", port),
		zap.Int("width", l.w), zap.Int("height", l.h),
		zap.Bool("view_only", arg.ViewOnly),
		zap.Duration("idle_timeout", idleTimeout(arg.IdleTimeoutMinutes)))

	// The port and size ride along here because this is the first moment they
	// exist — the RFB server is only started once consent has been given.
	s.publish(ipc.Event{
		Kind:      ipc.EvtStateChanged,
		State:     ipc.StateActive,
		SessionID: id,
		Port:      port,
		Width:     l.w,
		Height:    l.h,
	})
	return nil
}

// watchClipboard pushes anything the user copies locally to the viewer.
func (s *Service) watchClipboard(ctx context.Context, srv *rfb.Server, clip clipboard.Clipboard) {
	t := time.NewTicker(clipboardPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !clip.Changed() {
				continue
			}
			text, err := clip.Read()
			if err != nil || text == "" {
				continue
			}
			srv.SetClipboard(text)
		}
	}
}

// Stop implements ipc.Handler. Stopping an idle service is deliberately not an
// error: the daemon retries Stop on cleanup paths where it cannot know whether
// a session survived.
func (s *Service) Stop() error {
	s.mu.Lock()
	l := s.sess
	s.sess = nil
	if l != nil {
		s.state = ipc.StateIdle
	}
	s.mu.Unlock()

	if l == nil {
		return nil
	}

	l.cancel()
	_ = l.ln.Close()
	<-l.done

	_ = l.cap.Close()
	_ = l.inj.Close()
	if l.clip != nil {
		_ = l.clip.Close()
	}

	s.logger.Info("Remote-control session ended",
		zap.String("session", l.id),
		zap.Duration("duration", time.Since(l.start).Round(time.Second)))
	s.publish(ipc.Event{Kind: ipc.EvtStateChanged, State: ipc.StateIdle, SessionID: l.id})
	return nil
}

// Status implements ipc.Handler.
func (s *Service) Status() ipc.Response {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := ipc.Response{State: s.state}
	if s.state == ipc.StateError {
		resp.Error = s.lastErr
	}
	if l := s.sess; l != nil {
		resp.SessionID = l.id
		resp.Port = l.port
		resp.Width, resp.Height = l.w, l.h
		resp.Viewer = l.viewer
		resp.StartedAt = l.start
	}
	return resp
}

// Close shuts everything down.
func (s *Service) Close() error { return s.Stop() }

func newSessionID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a session id: %w", err)
	}
	return "rc_" + hex.EncodeToString(b[:]), nil
}

// clipboardWriter adapts clipboard.Clipboard to input.ClipboardWriter.
type clipboardWriter struct{ c clipboard.Clipboard }

func (w clipboardWriter) Write(text string) error { return w.c.Write(text) }

// noopInjector backs a view-only session.
type noopInjector struct{}

func (noopInjector) Key(bool, uint32) error        { return nil }
func (noopInjector) Pointer(int, int, uint8) error { return nil }
func (noopInjector) Close() error                  { return nil }
