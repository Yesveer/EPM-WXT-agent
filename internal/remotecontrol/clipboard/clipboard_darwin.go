//go:build darwin

package clipboard

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation
#include <stdlib.h>

long wxt_clip_change_count(void);
char *wxt_clip_read(void);
int wxt_clip_write(const char *utf8);
*/
import "C"

import (
	"errors"
	"unsafe"
)

type darwinClipboard struct {
	lastCount int64
}

func newClipboard() (Clipboard, error) {
	c := &darwinClipboard{}
	c.lastCount = int64(C.wxt_clip_change_count())
	return c, nil
}

func (c *darwinClipboard) Changed() bool {
	n := int64(C.wxt_clip_change_count())
	if n == c.lastCount {
		return false
	}
	c.lastCount = n
	return true
}

func (c *darwinClipboard) Read() (string, error) {
	p := C.wxt_clip_read()
	if p == nil {
		// The pasteboard holds an image, a file or nothing at all. That is
		// ordinary, not a failure.
		return "", nil
	}
	defer C.free(unsafe.Pointer(p))
	return truncate(C.GoString(p)), nil
}

func (c *darwinClipboard) Write(text string) error {
	text = truncate(text)
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))

	if C.wxt_clip_write(cs) == 0 {
		return errors.New("NSPasteboard rejected the write")
	}

	// Our own write bumps the change counter; absorbing it keeps the poller
	// from echoing the viewer's paste straight back to the viewer.
	c.lastCount = int64(C.wxt_clip_change_count())
	return nil
}

func (c *darwinClipboard) Close() error { return nil }
