// Package capture reads the contents of the user's screen.
//
// It implements rfb.Source. Every implementation must run inside the user's
// GUI session: a Windows session-0 service cannot see the interactive desktop
// at all, and a macOS root LaunchDaemon can never be granted the Screen
// Recording TCC right. That is why the capture code lives in the session
// helper binary rather than in the agent daemon.
package capture

import (
	"context"
	"fmt"
	"runtime"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
)

// Capturer grabs frames from a display.
type Capturer interface {
	// Size is the display size in pixels.
	Size() (w, h int)

	// Capture fills the capturer's internal image with the current screen.
	// The returned image is owned by the Capturer and stays valid until the
	// next call.
	Capture() (*rfb.Image, error)

	Close() error
}

// New opens the primary display. It fails on platforms with no implementation
// rather than silently handing back a black screen — a blank remote desktop
// with no error is far harder to diagnose than a refusal at startup.
func New() (Capturer, error) {
	c, err := newCapturer()
	if err != nil {
		return nil, fmt.Errorf("capture: %s: %w", runtime.GOOS, err)
	}
	return c, nil
}

// Source adapts a Capturer to rfb.Source by adding damage detection: the RFB
// server wants to know which parts of the screen changed, and no OS capture API
// used here reports that directly.
type Source struct {
	cap  Capturer
	diff *Differ
	// first forces a full update for the very first frame, since the viewer
	// starts with an empty framebuffer and has nothing to diff against.
	first bool
}

// NewSource wraps a Capturer.
func NewSource(c Capturer) *Source {
	return &Source{cap: c, diff: NewDiffer(), first: true}
}

// Size implements rfb.Source.
func (s *Source) Size() (int, int) { return s.cap.Size() }

// Frame implements rfb.Source.
func (s *Source) Frame(context.Context) (*rfb.Image, []rfb.Rect, error) {
	img, err := s.cap.Capture()
	if err != nil {
		return nil, nil, err
	}
	if img == nil {
		return nil, nil, nil
	}

	if s.first {
		s.first = false
		s.diff.Reset(img)
		return img, []rfb.Rect{img.Bounds()}, nil
	}

	damage := s.diff.Damage(img)
	return img, damage, nil
}

// Close implements rfb.Source.
func (s *Source) Close() error { return s.cap.Close() }
