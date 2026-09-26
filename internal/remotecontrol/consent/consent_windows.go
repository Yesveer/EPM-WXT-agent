//go:build windows

package consent

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procMessageBoxW = user32.NewProc("MessageBoxW")
	// MessageBoxTimeoutW is undocumented but has shipped in user32 since
	// Windows XP and is the only way to get a message box that closes itself.
	// It is looked up defensively so that a future Windows which drops it
	// degrades to a prompt without a timeout rather than failing outright.
	procMessageBoxTimeoutW = user32.NewProc("MessageBoxTimeoutW")
)

const (
	mbYesNo         = 0x00000004
	mbIconWarning   = 0x00000030
	mbDefButton2    = 0x00000100 // make "No" the default
	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000

	idYes     = 6
	idNo      = 7
	idTimeout = 32000 // MessageBoxTimeoutW's result when it closes itself
)

func ask(req Request) (Decision, error) {
	title, err := windows.UTF16PtrFromString(promptTitle)
	if err != nil {
		return Denied, err
	}
	body, err := windows.UTF16PtrFromString(message(req))
	if err != nil {
		return Denied, err
	}

	// No is the default button and the dialog is forced to the top: a consent
	// prompt buried behind other windows is one the user never actually gave,
	// and if they dismiss it blindly the safe answer should be the one that
	// happens.
	flags := uintptr(mbYesNo | mbIconWarning | mbDefButton2 |
		mbSystemModal | mbSetForeground | mbTopMost)

	var ret uintptr
	if err := procMessageBoxTimeoutW.Find(); err == nil {
		ms := req.Timeout.Milliseconds()
		ret, _, _ = procMessageBoxTimeoutW.Call(
			0, // no owner window; the helper draws no UI of its own
			uintptr(unsafe.Pointer(body)),
			uintptr(unsafe.Pointer(title)),
			flags,
			0, // language id
			uintptr(ms),
		)
	} else {
		ret, _, _ = procMessageBoxW.Call(
			0,
			uintptr(unsafe.Pointer(body)),
			uintptr(unsafe.Pointer(title)),
			flags,
		)
	}

	switch int(ret) {
	case idYes:
		return Approved, nil
	case idNo:
		return Denied, nil
	case idTimeout:
		return TimedOut, nil
	case 0:
		// MessageBox could not be created — typically no interactive desktop,
		// which means nobody is there to consent.
		return Denied, errors.New("could not display the consent prompt")
	default:
		return Denied, nil
	}
}
