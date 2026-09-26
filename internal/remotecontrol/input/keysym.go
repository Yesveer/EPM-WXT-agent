// Package input injects the viewer's keyboard and mouse events into the local
// desktop.
//
// Like capture, every implementation must run inside the user's GUI session:
// a Windows service in session 0 cannot SendInput to the interactive desktop,
// and macOS requires the Accessibility TCC grant, which a root daemon has no
// way to obtain.
package input

// RFB carries keys as X11 keysyms regardless of the viewer's real keyboard, so
// both platform backends start from these values.
//
// Printable ASCII maps to itself (0x20-0x7E), Latin-1 to itself (0xA0-0xFF),
// and anything else Unicode arrives as 0x01000000+codepoint. The constants
// below are the named keys that have no character equivalent.
const (
	XKBackSpace = 0xFF08
	XKTab       = 0xFF09
	XKLinefeed  = 0xFF0A
	XKClear     = 0xFF0B
	XKReturn    = 0xFF0D
	XKPause     = 0xFF13
	XKScrollLk  = 0xFF14
	XKSysReq    = 0xFF15
	XKEscape    = 0xFF1B
	XKDelete    = 0xFFFF

	XKHome     = 0xFF50
	XKLeft     = 0xFF51
	XKUp       = 0xFF52
	XKRight    = 0xFF53
	XKDown     = 0xFF54
	XKPageUp   = 0xFF55
	XKPageDown = 0xFF56
	XKEnd      = 0xFF57
	XKBegin    = 0xFF58
	XKInsert   = 0xFF63
	XKMenu     = 0xFF67

	XKNumLock = 0xFF7F

	XKKPEnter    = 0xFF8D
	XKKPMultiply = 0xFFAA
	XKKPAdd      = 0xFFAB
	XKKPSubtract = 0xFFAD
	XKKPDecimal  = 0xFFAE
	XKKPDivide   = 0xFFAF
	XKKP0        = 0xFFB0
	XKKP9        = 0xFFB9

	XKF1  = 0xFFBE
	XKF12 = 0xFFC9

	XKShiftL   = 0xFFE1
	XKShiftR   = 0xFFE2
	XKControlL = 0xFFE3
	XKControlR = 0xFFE4
	XKCapsLock = 0xFFE5
	XKMetaL    = 0xFFE7
	XKMetaR    = 0xFFE8
	XKAltL     = 0xFFE9
	XKAltR     = 0xFFEA
	XKSuperL   = 0xFFEB
	XKSuperR   = 0xFFEC
)

// keysymToRune returns the character a keysym types, and whether it types one
// at all. Modifiers, arrows and function keys do not.
//
// Resolving printable keys to a character — rather than to a physical key
// position — is what makes the remote keyboard layout-independent: the viewer
// says "the user pressed é" and the platform backend synthesises whatever it
// takes to produce é locally, even if the two machines have different layouts.
func keysymToRune(keysym uint32) (rune, bool) {
	switch {
	case keysym >= 0x20 && keysym <= 0x7E:
		return rune(keysym), true // ASCII printable
	case keysym >= 0xA0 && keysym <= 0xFF:
		return rune(keysym), true // Latin-1 maps 1:1 to Unicode
	case keysym&0xFF000000 == 0x01000000:
		return rune(keysym & 0x00FFFFFF), true // explicit Unicode keysym
	}
	return 0, false
}

// isModifier reports whether the keysym is a modifier key.
func isModifier(keysym uint32) bool {
	switch keysym {
	case XKShiftL, XKShiftR, XKControlL, XKControlR,
		XKAltL, XKAltR, XKSuperL, XKSuperR, XKMetaL, XKMetaR, XKCapsLock:
		return true
	}
	return false
}
