//go:build windows

package clipboard

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

type windowsClipboard struct {
	lastSeq uint32
}

func newClipboard() (Clipboard, error) {
	c := &windowsClipboard{}
	c.lastSeq = c.sequence()
	return c, nil
}

func (c *windowsClipboard) sequence() uint32 {
	n, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(n)
}

// Changed uses the OS sequence number, which increments on every clipboard
// write system-wide. Comparing that integer is far cheaper than opening the
// clipboard and diffing its contents on every poll — and opening it contends
// with whatever application the user is actually using.
func (c *windowsClipboard) Changed() bool {
	seq := c.sequence()
	if seq == c.lastSeq {
		return false
	}
	c.lastSeq = seq
	return true
}

// open acquires the clipboard, retrying briefly.
//
// Only one process may hold the Windows clipboard at a time, and applications
// grab it for short bursts while the user copies. A single failed attempt is
// therefore normal rather than exceptional, so this retries instead of
// surfacing an error the caller could only respond to by retrying anyway.
func openClipboard() error {
	const attempts = 10
	for i := 0; i < attempts; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("clipboard is held by another process")
}

func (c *windowsClipboard) Read() (string, error) {
	if avail, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); avail == 0 {
		return "", nil // non-text contents; not an error
	}
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call() // #nosec G104 -- nothing actionable on failure

	h, _, err := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", fmt.Errorf("GetClipboardData: %w", err)
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		return "", fmt.Errorf("GlobalLock: %w", err)
	}
	defer procGlobalUnlock.Call(h) // #nosec G104 -- nothing actionable on failure

	// The handle owns a NUL-terminated UTF-16 string of unknown length.
	// go vet's unsafeptr check flags this uintptr→Pointer conversion, and
	// cannot tell that it is safe here: GlobalLock returns a pointer into
	// memory the OS owns, which the Go collector neither tracks nor moves, so
	// the address cannot go stale. x/sys/windows ships no typed GlobalLock, so
	// the raw syscall is the only route.
	return truncate(windows.UTF16PtrToString((*uint16)(unsafe.Pointer(p)))), nil
}

func (c *windowsClipboard) Write(text string) error {
	text = truncate(text)
	utf16, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("clipboard text is not valid UTF-16: %w", err)
	}

	if err := openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call() // #nosec G104 -- nothing actionable on failure

	if r, _, err := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", err)
	}

	size := uintptr(len(utf16) * 2)
	h, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %w", err)
	}

	p, _, lockErr := procGlobalLock.Call(h)
	if p == 0 {
		_, _, _ = procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %w", lockErr)
	}
	// go vet's unsafeptr check flags this uintptr→Pointer conversion, and
	// cannot tell that it is safe here: GlobalLock returns a pointer into
	// memory the OS owns, which the Go collector neither tracks nor moves, so
	// the address cannot go stale. x/sys/windows ships no typed GlobalLock, so
	// the raw syscall is the only route.
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(utf16))
	copy(dst, utf16)
	_, _, _ = procGlobalUnlock.Call(h)

	if r, _, setErr := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		// Ownership only transfers to the system on success, so a failure
		// leaves this block for us to release.
		_, _, _ = procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %w", setErr)
	}

	// This write bumps the sequence number too; absorbing it here stops the
	// poller from immediately reporting our own paste back to the viewer as a
	// local change.
	c.lastSeq = c.sequence()
	return nil
}

func (c *windowsClipboard) Close() error { return nil }
