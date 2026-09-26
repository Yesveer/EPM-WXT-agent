//go:build darwin

package capture

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -Wno-unused-parameter
#cgo LDFLAGS: -framework Foundation -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo -framework CoreGraphics
#include <stdlib.h>
#include "capture_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
)

// ErrScreenRecordingDenied means macOS refused to hand over the screen. It is
// its own error because the fix is a specific user action, and a generic
// "capture failed" would send someone hunting through logs for a permission
// dialog they simply never approved.
var ErrScreenRecordingDenied = errors.New(
	"macOS denied screen capture — grant Screen Recording to wxt-agent in " +
		"System Settings › Privacy & Security › Screen Recording, then restart the agent")

// ErrCaptureStopped means the stream ended underneath us: the user logged out,
// the display was disconnected, or the Screen Recording grant was revoked
// mid-session.
var ErrCaptureStopped = errors.New("screen capture stream stopped")

// frameTimeoutMS bounds the wait for the very first frame. Later frames never
// wait — see the note in wxt_capture_copy.
const frameTimeoutMS = 5000

// darwinCapturer streams the main display through ScreenCaptureKit.
//
// Capture runs at POINT resolution rather than backing-store pixels. On a
// Retina display that halves the bytes, and it makes framebuffer coordinates
// identical to the ones CGEvent wants for input — so the input layer needs no
// scale conversion and cannot drift out of sync with the capture.
type darwinCapturer struct {
	w, h int
	img  *rfb.Image
}

func newCapturer() (Capturer, error) {
	var cw, ch C.int
	errbuf := make([]byte, 512)

	rc := C.wxt_capture_start(&cw, &ch,
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.int(len(errbuf)))

	if rc != C.WXT_OK {
		detail := cstr(errbuf)
		switch rc {
		case C.WXT_ERR_PERMISSION:
			return nil, fmt.Errorf("%w (%s)", ErrScreenRecordingDenied, detail)
		case C.WXT_ERR_NO_DISPLAY:
			return nil, fmt.Errorf("no display available: %s", detail)
		default:
			return nil, fmt.Errorf("ScreenCaptureKit failed to start: %s", detail)
		}
	}

	return &darwinCapturer{
		w:   int(cw),
		h:   int(ch),
		img: rfb.NewImage(int(cw), int(ch)),
	}, nil
}

func (c *darwinCapturer) Size() (int, int) { return c.w, c.h }

func (c *darwinCapturer) Capture() (*rfb.Image, error) {
	// A resolution change means the running stream is configured for the old
	// size, so the stream itself has to be rebuilt — SCStreamConfiguration is
	// fixed once the stream starts.
	var cw, ch C.int
	C.wxt_capture_size(&cw, &ch)
	if int(cw) != c.w || int(ch) != c.h {
		if err := c.restart(); err != nil {
			return nil, err
		}
	}

	rc := C.wxt_capture_copy(unsafe.Pointer(&c.img.Pix[0]),
		C.int(c.w), C.int(c.h), C.int(frameTimeoutMS))

	switch rc {
	case C.WXT_OK:
		return c.img, nil
	case C.WXT_ERR_TIMEOUT:
		// No frame yet. Reporting "no image, no damage" keeps the session
		// alive; the RFB server simply leaves the client's request pending.
		return nil, nil
	case C.WXT_ERR_STOPPED:
		return nil, ErrCaptureStopped
	default:
		return nil, fmt.Errorf("screen capture failed (code %d)", int(rc))
	}
}

func (c *darwinCapturer) restart() error {
	C.wxt_capture_stop()

	var cw, ch C.int
	errbuf := make([]byte, 512)
	rc := C.wxt_capture_start(&cw, &ch,
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.int(len(errbuf)))
	if rc != C.WXT_OK {
		return fmt.Errorf("could not restart capture after a display change: %s", cstr(errbuf))
	}

	c.w, c.h = int(cw), int(ch)
	c.img = rfb.NewImage(c.w, c.h)
	return nil
}

func (c *darwinCapturer) Close() error {
	C.wxt_capture_stop()
	return nil
}

// cstr turns a NUL-terminated C string held in a Go byte slice into a string.
func cstr(b []byte) string {
	for i, ch := range b {
		if ch == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
