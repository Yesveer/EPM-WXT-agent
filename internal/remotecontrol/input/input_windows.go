//go:build windows

package input

import (
	"fmt"
	"unsafe"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procVkKeyScanW       = user32.NewProc("VkKeyScanW")
)

const (
	smCXScreen = 0
	smCYScreen = 1

	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove       = 0x0001
	mouseEventLeftDown   = 0x0002
	mouseEventLeftUp     = 0x0004
	mouseEventRightDown  = 0x0008
	mouseEventRightUp    = 0x0010
	mouseEventMiddleDown = 0x0020
	mouseEventMiddleUp   = 0x0040
	mouseEventWheel      = 0x0800
	mouseEventAbsolute   = 0x8000

	wheelDelta = 120

	keyEventExtended = 0x0001
	keyEventKeyUp    = 0x0002
	keyEventUnicode  = 0x0004
)

type mouseInput struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type keybdInput struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// rawInput mirrors the Win32 INPUT union. Declaring the mouse variant — the
// largest member — makes Go lay the struct out at exactly the size SendInput
// expects; keyboard events are written into the same storage through a
// pointer cast.
type rawInput struct {
	Type uint32
	Mi   mouseInput
}

// windowsInjector synthesises input with SendInput.
//
// SendInput reaches the interactive desktop only from a process inside that
// desktop's session, which is the whole reason for the session-helper split.
// It also cannot reach the secure desktop (UAC prompts, the lock screen) at
// any privilege level below the one that owns it — a known limitation of every
// remote-control tool on Windows, not something this code can work around.
type windowsInjector struct {
	// held tracks which modifiers the viewer currently has down. Printable
	// keys are normally synthesised as Unicode, which is layout-proof but
	// produces no keyboard SHORTCUT — Ctrl+Unicode-'c' is not Ctrl+C. When a
	// modifier is held we therefore switch to virtual-key codes.
	held map[uint32]bool

	// prevButtons is the last button mask seen. RFB reports absolute button
	// state on every pointer event, while SendInput wants press/release
	// edges, so the previous state is what turns one into the other.
	prevButtons uint8
}

func newInjector() (Injector, error) {
	return &windowsInjector{held: make(map[uint32]bool)}, nil
}

func (w *windowsInjector) send(inputs []rawInput) error {
	if len(inputs) == 0 {
		return nil
	}
	n, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(rawInput{}),
	)
	if int(n) != len(inputs) {
		return fmt.Errorf("SendInput accepted %d of %d events: %w", n, len(inputs), err)
	}
	return nil
}

func (w *windowsInjector) Pointer(x, y int, buttons uint8) error {
	sw, _, _ := procGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCYScreen)
	if sw <= 1 || sh <= 1 {
		return fmt.Errorf("screen metrics unavailable (%dx%d)", sw, sh)
	}

	// Absolute mouse coordinates are normalised to 0..65535 across the
	// screen, not measured in pixels.
	nx := int32(int64(x) * 65535 / int64(sw-1))
	ny := int32(int64(y) * 65535 / int64(sh-1))

	events := []rawInput{{
		Type: inputMouse,
		Mi: mouseInput{
			Dx:      nx,
			Dy:      ny,
			DwFlags: mouseEventMove | mouseEventAbsolute,
		},
	}}

	pressed, released := buttonEdges(w.prevButtons, buttons)
	add := func(flags uint32, data uint32) {
		events = append(events, rawInput{
			Type: inputMouse,
			Mi:   mouseInput{Dx: nx, Dy: ny, MouseData: data, DwFlags: flags | mouseEventAbsolute},
		})
	}

	if pressed&rfb.ButtonLeft != 0 {
		add(mouseEventLeftDown, 0)
	}
	if released&rfb.ButtonLeft != 0 {
		add(mouseEventLeftUp, 0)
	}
	if pressed&rfb.ButtonMiddle != 0 {
		add(mouseEventMiddleDown, 0)
	}
	if released&rfb.ButtonMiddle != 0 {
		add(mouseEventMiddleUp, 0)
	}
	if pressed&rfb.ButtonRight != 0 {
		add(mouseEventRightDown, 0)
	}
	if released&rfb.ButtonRight != 0 {
		add(mouseEventRightUp, 0)
	}

	// RFB models the wheel as buttons 4 and 5 being clicked, so a press edge
	// is one notch of scroll. There is no matching release to act on.
	if pressed&rfb.ButtonWheelUp != 0 {
		add(mouseEventWheel, wheelDelta)
	}
	if pressed&rfb.ButtonWheelDown != 0 {
		// MouseData carries a SIGNED notch count in an unsigned field. The
		// conversion has to go through a variable: Go rejects the constant
		// expression uint32(int32(-120)) outright.
		back := int32(-wheelDelta)
		add(mouseEventWheel, uint32(back))
	}

	w.prevButtons = buttons
	return w.send(events)
}

func (w *windowsInjector) Key(down bool, keysym uint32) error {
	if isModifier(keysym) {
		w.held[keysym] = down
	}

	if vk, extended, ok := keysymToVK(keysym); ok {
		return w.sendVK(vk, extended, down)
	}

	r, ok := keysymToRune(keysym)
	if !ok {
		return nil // nothing sensible to synthesise
	}

	// With Ctrl/Alt/Win held, Unicode injection produces a character rather
	// than a shortcut, so fall back to the virtual key for this layout.
	if w.modifierHeld() {
		if vk, ok := w.vkForRune(r); ok {
			return w.sendVK(vk, false, down)
		}
	}
	return w.sendUnicode(r, down)
}

// modifierHeld reports whether a shortcut-forming modifier is down. Shift is
// excluded: it only selects a character, which Unicode injection already
// encodes.
func (w *windowsInjector) modifierHeld() bool {
	return w.held[XKControlL] || w.held[XKControlR] ||
		w.held[XKAltL] || w.held[XKAltR] ||
		w.held[XKSuperL] || w.held[XKSuperR]
}

// vkForRune asks Windows which virtual key produces r on the current layout.
func (w *windowsInjector) vkForRune(r rune) (uint16, bool) {
	if r > 0xFFFF {
		return 0, false
	}
	res, _, _ := procVkKeyScanW.Call(uintptr(uint16(r)))
	v := int16(uint16(res))
	if v == -1 {
		return 0, false
	}
	return uint16(v & 0xFF), true
}

func (w *windowsInjector) sendVK(vk uint16, extended, down bool) error {
	var flags uint32
	if extended {
		flags |= keyEventExtended
	}
	if !down {
		flags |= keyEventKeyUp
	}
	ri := rawInput{Type: inputKeyboard}
	*(*keybdInput)(unsafe.Pointer(&ri.Mi)) = keybdInput{WVk: vk, DwFlags: flags}
	return w.send([]rawInput{ri})
}

// sendUnicode types a character directly, bypassing the keyboard layout.
func (w *windowsInjector) sendUnicode(r rune, down bool) error {
	var units []uint16
	if r > 0xFFFF {
		// Outside the BMP: SendInput takes UTF-16, so an astral character has
		// to go as its surrogate pair, one event each.
		r -= 0x10000
		units = []uint16{
			uint16(0xD800 + (r >> 10)),
			uint16(0xDC00 + (r & 0x3FF)),
		}
	} else {
		units = []uint16{uint16(r)}
	}

	events := make([]rawInput, 0, len(units))
	for _, u := range units {
		flags := uint32(keyEventUnicode)
		if !down {
			flags |= keyEventKeyUp
		}
		ri := rawInput{Type: inputKeyboard}
		*(*keybdInput)(unsafe.Pointer(&ri.Mi)) = keybdInput{WScan: u, DwFlags: flags}
		events = append(events, ri)
	}
	return w.send(events)
}

func (w *windowsInjector) Close() error { return nil }

// keysymToVK maps the named (non-character) keys to virtual-key codes. The
// second result says whether the key needs KEYEVENTF_EXTENDEDKEY — without it
// the arrow cluster and the right-hand modifiers are delivered as their numpad
// twins.
func keysymToVK(keysym uint32) (vk uint16, extended bool, ok bool) {
	switch keysym {
	case XKBackSpace:
		return 0x08, false, true
	case XKTab:
		return 0x09, false, true
	case XKClear:
		return 0x0C, false, true
	case XKReturn:
		return 0x0D, false, true
	case XKPause:
		return 0x13, false, true
	case XKCapsLock:
		return 0x14, false, true
	case XKEscape:
		return 0x1B, false, true

	case XKPageUp:
		return 0x21, true, true
	case XKPageDown:
		return 0x22, true, true
	case XKEnd:
		return 0x23, true, true
	case XKHome:
		return 0x24, true, true
	case XKLeft:
		return 0x25, true, true
	case XKUp:
		return 0x26, true, true
	case XKRight:
		return 0x27, true, true
	case XKDown:
		return 0x28, true, true
	case XKInsert:
		return 0x2D, true, true
	case XKDelete:
		return 0x2E, true, true

	case XKSuperL:
		return 0x5B, true, true
	case XKSuperR, XKMetaR:
		return 0x5C, true, true
	case XKMenu:
		return 0x5D, true, true

	case XKKPMultiply:
		return 0x6A, false, true
	case XKKPAdd:
		return 0x6B, false, true
	case XKKPSubtract:
		return 0x6D, false, true
	case XKKPDecimal:
		return 0x6E, false, true
	case XKKPDivide:
		return 0x6F, true, true
	case XKKPEnter:
		return 0x0D, true, true

	case XKNumLock:
		return 0x90, true, true
	case XKScrollLk:
		return 0x91, false, true

	case XKShiftL:
		return 0xA0, false, true
	case XKShiftR:
		return 0xA1, true, true
	case XKControlL:
		return 0xA2, false, true
	case XKControlR:
		return 0xA3, true, true
	case XKAltL, XKMetaL:
		return 0xA4, false, true
	case XKAltR:
		return 0xA5, true, true
	}

	if keysym >= XKF1 && keysym <= XKF12 {
		return uint16(0x70 + (keysym - XKF1)), false, true
	}
	if keysym >= XKKP0 && keysym <= XKKP9 {
		return uint16(0x60 + (keysym - XKKP0)), false, true
	}
	return 0, false, false
}
