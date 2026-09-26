//go:build darwin

package input

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework ApplicationServices
#include <CoreGraphics/CoreGraphics.h>
#include <ApplicationServices/ApplicationServices.h>

// wxt_input_trusted reports whether this process holds the Accessibility
// right. Without it CGEventPost silently does nothing — no error, no event —
// so checking up front is the only way to tell "denied" from "broken".
static int wxt_input_trusted(void) {
	return AXIsProcessTrusted() ? 1 : 0;
}

static void wxt_post_mouse(int type, double x, double y, int button, uint64_t flags) {
	CGEventRef e = CGEventCreateMouseEvent(NULL, (CGEventType)type,
		CGPointMake(x, y), (CGMouseButton)button);
	if (!e) return;
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

// wxt_post_mouse_click adds the click count, which is what turns two rapid
// clicks into a double-click the way a real mouse would.
static void wxt_post_mouse_click(int type, double x, double y, int button,
                                 uint64_t flags, int clickCount) {
	CGEventRef e = CGEventCreateMouseEvent(NULL, (CGEventType)type,
		CGPointMake(x, y), (CGMouseButton)button);
	if (!e) return;
	CGEventSetIntegerValueField(e, kCGMouseEventClickState, clickCount);
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void wxt_post_scroll(int lines, uint64_t flags) {
	CGEventRef e = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitLine, 1, lines);
	if (!e) return;
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void wxt_post_key(uint16_t keycode, int down, uint64_t flags) {
	CGEventRef e = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)keycode, down ? true : false);
	if (!e) return;
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

// wxt_post_unicode types a character without reference to the keyboard layout.
// The virtual keycode is meaningless here; the attached string is what macOS
// actually inserts.
static void wxt_post_unicode(uint16_t *utf16, int len, int down, uint64_t flags) {
	CGEventRef e = CGEventCreateKeyboardEvent(NULL, 0, down ? true : false);
	if (!e) return;
	CGEventKeyboardSetUnicodeString(e, len, utf16);
	CGEventSetFlags(e, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}
*/
import "C"

import (
	"errors"
	"unicode/utf16"
	"unsafe"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
)

// ErrAccessibilityDenied means macOS will not let this process synthesise
// input. Like the Screen Recording grant it is a one-time user action, and it
// gets its own error because CGEventPost fails silently — a session would
// otherwise look perfectly healthy while ignoring every click.
var ErrAccessibilityDenied = errors.New(
	"macOS denied input control — grant Accessibility to wxt-agent in " +
		"System Settings › Privacy & Security › Accessibility, then restart the agent")

// CGEventType values.
const (
	evMouseMoved     = 5
	evLeftMouseDown  = 1
	evLeftMouseUp    = 2
	evRightMouseDown = 3
	evRightMouseUp   = 4
	evLeftMouseDrag  = 6
	evRightMouseDrag = 7
	evOtherMouseDown = 25
	evOtherMouseUp   = 26
	evOtherMouseDrag = 27
)

// CGMouseButton values.
const (
	btnLeft   = 0
	btnRight  = 1
	btnCenter = 2
)

// CGEventFlags for the modifier keys.
const (
	flagShift   = 0x00020000
	flagControl = 0x00040000
	flagAlt     = 0x00080000
	flagCommand = 0x00100000
)

// macOS virtual keycodes (kVK_*) for the keys that have no character.
var keysymToMacVK = map[uint32]uint16{
	XKReturn:    0x24,
	XKKPEnter:   0x4C,
	XKTab:       0x30,
	XKBackSpace: 0x33,
	XKEscape:    0x35,
	XKCapsLock:  0x39,
	XKDelete:    0x75, // forward delete, not backspace
	XKHome:      0x73,
	XKEnd:       0x77,
	XKPageUp:    0x74,
	XKPageDown:  0x79,
	XKLeft:      0x7B,
	XKRight:     0x7C,
	XKDown:      0x7D,
	XKUp:        0x7E,

	XKShiftL:   0x38,
	XKShiftR:   0x3C,
	XKControlL: 0x3B,
	XKControlR: 0x3E,
	XKAltL:     0x3A,
	XKAltR:     0x3D,
	XKSuperL:   0x37,
	XKSuperR:   0x36,
	XKMetaL:    0x37,
	XKMetaR:    0x36,

	// The function-key row is deliberately not contiguous on macOS.
	XKF1:      0x7A,
	XKF1 + 1:  0x78,
	XKF1 + 2:  0x63,
	XKF1 + 3:  0x76,
	XKF1 + 4:  0x60,
	XKF1 + 5:  0x61,
	XKF1 + 6:  0x62,
	XKF1 + 7:  0x64,
	XKF1 + 8:  0x65,
	XKF1 + 9:  0x6D,
	XKF1 + 10: 0x67,
	XKF1 + 11: 0x6F,

	XKKPMultiply: 0x43,
	XKKPAdd:      0x45,
	XKKPSubtract: 0x4E,
	XKKPDecimal:  0x41,
	XKKPDivide:   0x4B,
}

// asciiToMacVK covers the US-layout keys needed to build a shortcut. It is
// used ONLY while a command/control/option modifier is held: typing normally
// goes through the layout-independent Unicode path, but Cmd+C has to be a real
// keycode or macOS sees no shortcut at all.
var asciiToMacVK = map[rune]uint16{
	'a': 0x00, 'b': 0x0B, 'c': 0x08, 'd': 0x02, 'e': 0x0E, 'f': 0x03,
	'g': 0x05, 'h': 0x04, 'i': 0x22, 'j': 0x26, 'k': 0x28, 'l': 0x25,
	'm': 0x2E, 'n': 0x2D, 'o': 0x1F, 'p': 0x23, 'q': 0x0C, 'r': 0x0F,
	's': 0x01, 't': 0x11, 'u': 0x20, 'v': 0x09, 'w': 0x0D, 'x': 0x07,
	'y': 0x10, 'z': 0x06,
	'0': 0x1D, '1': 0x12, '2': 0x13, '3': 0x14, '4': 0x15,
	'5': 0x17, '6': 0x16, '7': 0x1A, '8': 0x1C, '9': 0x19,
	'-': 0x1B, '=': 0x18, '[': 0x21, ']': 0x1E, '\\': 0x2A,
	';': 0x29, '\'': 0x27, ',': 0x2B, '.': 0x2F, '/': 0x2C, '`': 0x32,
	' ': 0x31,
}

// darwinInjector synthesises input with CGEvent.
//
// Coordinates arrive in framebuffer pixels, which the capture side
// deliberately keeps equal to macOS points, so they are passed straight
// through with no scaling.
type darwinInjector struct {
	held        map[uint32]bool
	prevButtons uint8
	lastX       int
	lastY       int
}

func newInjector() (Injector, error) {
	if C.wxt_input_trusted() == 0 {
		return nil, ErrAccessibilityDenied
	}
	return &darwinInjector{held: make(map[uint32]bool)}, nil
}

// flags builds the CGEventFlags mask for the modifiers the viewer holds down.
// macOS does not infer modifier state from earlier key events — every
// synthesised event has to carry the flags itself.
func (d *darwinInjector) flags() C.uint64_t {
	var f C.uint64_t
	if d.held[XKShiftL] || d.held[XKShiftR] {
		f |= flagShift
	}
	if d.held[XKControlL] || d.held[XKControlR] {
		f |= flagControl
	}
	if d.held[XKAltL] || d.held[XKAltR] {
		f |= flagAlt
	}
	if d.held[XKSuperL] || d.held[XKSuperR] || d.held[XKMetaL] || d.held[XKMetaR] {
		f |= flagCommand
	}
	return f
}

func (d *darwinInjector) Pointer(x, y int, buttons uint8) error {
	fx, fy := C.double(x), C.double(y)
	fl := d.flags()
	pressed, released := buttonEdges(d.prevButtons, buttons)

	// A move while a button is held must be posted as a DRAG event. Sending a
	// plain mouseMoved during a drag makes macOS drop the drag, which breaks
	// text selection and window dragging.
	if x != d.lastX || y != d.lastY || (pressed|released) == 0 {
		moveType := evMouseMoved
		btn := btnLeft
		switch {
		case buttons&rfb.ButtonLeft != 0:
			moveType, btn = evLeftMouseDrag, btnLeft
		case buttons&rfb.ButtonRight != 0:
			moveType, btn = evRightMouseDrag, btnRight
		case buttons&rfb.ButtonMiddle != 0:
			moveType, btn = evOtherMouseDrag, btnCenter
		}
		C.wxt_post_mouse(C.int(moveType), fx, fy, C.int(btn), fl)
	}

	click := func(evType, btn int) {
		C.wxt_post_mouse_click(C.int(evType), fx, fy, C.int(btn), fl, 1)
	}
	if pressed&rfb.ButtonLeft != 0 {
		click(evLeftMouseDown, btnLeft)
	}
	if released&rfb.ButtonLeft != 0 {
		click(evLeftMouseUp, btnLeft)
	}
	if pressed&rfb.ButtonRight != 0 {
		click(evRightMouseDown, btnRight)
	}
	if released&rfb.ButtonRight != 0 {
		click(evRightMouseUp, btnRight)
	}
	if pressed&rfb.ButtonMiddle != 0 {
		click(evOtherMouseDown, btnCenter)
	}
	if released&rfb.ButtonMiddle != 0 {
		click(evOtherMouseUp, btnCenter)
	}

	// RFB sends wheel movement as clicks of buttons 4 and 5, so only the press
	// edge means anything.
	if pressed&rfb.ButtonWheelUp != 0 {
		C.wxt_post_scroll(C.int(1), fl)
	}
	if pressed&rfb.ButtonWheelDown != 0 {
		C.wxt_post_scroll(C.int(-1), fl)
	}

	d.prevButtons = buttons
	d.lastX, d.lastY = x, y
	return nil
}

func (d *darwinInjector) Key(down bool, keysym uint32) error {
	if isModifier(keysym) {
		// Update the held set BEFORE building flags so that pressing Command
		// is itself posted with the Command flag already set — macOS expects
		// the modifier event to reflect the new state.
		d.held[keysym] = down
	}
	fl := d.flags()

	if vk, ok := keysymToMacVK[keysym]; ok {
		C.wxt_post_key(C.uint16_t(vk), cbool(down), fl)
		return nil
	}

	r, ok := keysymToRune(keysym)
	if !ok {
		return nil
	}

	// Shortcuts need a real keycode; plain typing does not and is better off
	// layout-independent.
	if d.shortcutModifierHeld() {
		lower := r
		if lower >= 'A' && lower <= 'Z' {
			lower = lower - 'A' + 'a'
		}
		if vk, ok := asciiToMacVK[lower]; ok {
			C.wxt_post_key(C.uint16_t(vk), cbool(down), fl)
			return nil
		}
	}

	units := utf16.Encode([]rune{r})
	if len(units) == 0 {
		return nil
	}
	C.wxt_post_unicode((*C.uint16_t)(unsafe.Pointer(&units[0])),
		C.int(len(units)), cbool(down), fl)
	return nil
}

// shortcutModifierHeld excludes Shift, which only selects a character and is
// already reflected in the Unicode value.
func (d *darwinInjector) shortcutModifierHeld() bool {
	return d.held[XKControlL] || d.held[XKControlR] ||
		d.held[XKAltL] || d.held[XKAltR] ||
		d.held[XKSuperL] || d.held[XKSuperR] ||
		d.held[XKMetaL] || d.held[XKMetaR]
}

func (d *darwinInjector) Close() error { return nil }

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}
