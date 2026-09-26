package input

import (
	"fmt"
	"runtime"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
)

// Injector delivers viewer input to the local desktop.
type Injector interface {
	// Key presses or releases one key, given as an X11 keysym.
	Key(down bool, keysym uint32) error

	// Pointer moves the cursor to (x, y) in framebuffer coordinates and
	// applies the given button state.
	Pointer(x, y int, buttons uint8) error

	Close() error
}

// ClipboardWriter puts text on the local clipboard. It is separate from
// Injector because the clipboard is owned by the OS pasteboard rather than the
// input queue, and on some platforms it is a different subsystem entirely.
type ClipboardWriter interface {
	Write(text string) error
}

// New opens the platform input backend.
func New() (Injector, error) {
	inj, err := newInjector()
	if err != nil {
		return nil, fmt.Errorf("input: %s: %w", runtime.GOOS, err)
	}
	return inj, nil
}

// Sink adapts an Injector and a clipboard writer to rfb.Sink.
type Sink struct {
	inj  Injector
	clip ClipboardWriter
}

// NewSink wraps an Injector. clip may be nil, in which case pastes from the
// viewer are dropped rather than failing the session.
func NewSink(inj Injector, clip ClipboardWriter) *Sink {
	return &Sink{inj: inj, clip: clip}
}

// Key implements rfb.Sink.
func (s *Sink) Key(e rfb.KeyEvent) error {
	return s.inj.Key(e.Down, e.Key)
}

// Pointer implements rfb.Sink.
func (s *Sink) Pointer(e rfb.PointerEvent) error {
	return s.inj.Pointer(e.X, e.Y, e.ButtonMask)
}

// SetClipboard implements rfb.Sink.
func (s *Sink) SetClipboard(text string) error {
	if s.clip == nil {
		return nil
	}
	return s.clip.Write(text)
}

// Close releases the injector.
func (s *Sink) Close() error { return s.inj.Close() }

// buttonEdges reports which buttons were pressed and released between two
// masks. Backends need the edges, not the level.
func buttonEdges(prev, cur uint8) (pressed, released uint8) {
	return cur &^ prev, prev &^ cur
}
