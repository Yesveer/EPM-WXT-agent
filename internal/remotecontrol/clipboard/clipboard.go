// Package clipboard reads and writes the local text clipboard.
//
// Clipboard sync is two independent directions and they fail differently:
// the viewer pasting INTO the machine is a write, and the user copying ON the
// machine is a read that has to be noticed first. Neither OS pushes a
// notification that a Go background process can subscribe to without a run
// loop, so the local side is polled — cheaply, via a change counter rather
// than by re-reading the contents.
package clipboard

import (
	"fmt"
	"runtime"
)

// Clipboard is the local text clipboard.
type Clipboard interface {
	// Read returns the current clipboard text. Non-text contents (an image, a
	// file promise) come back as an empty string rather than an error.
	Read() (string, error)

	// Write replaces the clipboard contents.
	Write(text string) error

	// Changed reports whether the clipboard has changed since the previous
	// call, using the OS change counter so it stays cheap enough to poll.
	Changed() bool

	Close() error
}

// New opens the platform clipboard.
func New() (Clipboard, error) {
	c, err := newClipboard()
	if err != nil {
		return nil, fmt.Errorf("clipboard: %s: %w", runtime.GOOS, err)
	}
	return c, nil
}

// maxTextBytes bounds what is exchanged in either direction. A clipboard can
// hold a great deal; shipping tens of megabytes of it down a remote-control
// session would stall the screen stream behind it.
const maxTextBytes = 256 << 10 // 256 KiB

// truncate clamps text to maxTextBytes without splitting a UTF-8 sequence.
func truncate(s string) string {
	if len(s) <= maxTextBytes {
		return s
	}
	cut := maxTextBytes
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b begins a UTF-8 sequence (i.e. is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
