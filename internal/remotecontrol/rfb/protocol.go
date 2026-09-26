// Package rfb implements the server side of RFB (Remote Framebuffer) 3.8 —
// the wire protocol VNC speaks.
//
// The agent embeds its own server rather than shipping a third-party one:
// TightVNC, UltraVNC and TigerVNC are all GPL, and bundling any of them would
// put GPL obligations on the whole product. Owning the implementation also
// keeps the consent gate, recording and clipboard policy inside the agent
// instead of in a process we cannot control.
//
// The server never binds a public interface. It listens on 127.0.0.1 and is
// reachable only through the agent's existing gRPC tunnel, so a machine running
// it never exposes a VNC port to the network.
//
// Wire conventions: all protocol scalars are big-endian. Pixel data byte order
// is separate and controlled by the client's PixelFormat.BigEndian flag.
package rfb

import (
	"encoding/binary"
	"fmt"
	"io"
)

// protocolVersion is the only version we offer. 3.8 is what every modern client
// (including libvncclient, which is what guacd uses) negotiates.
const protocolVersion = "RFB 003.008\n"

// Security types (RFB §7.1.2).
const (
	secTypeInvalid = 0
	secTypeNone    = 1
	secTypeVNCAuth = 2
)

// Client-to-server message types (RFB §7.5).
const (
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3
	msgKeyEvent                 = 4
	msgPointerEvent             = 5
	msgClientCutText            = 6
)

// Server-to-client message types (RFB §7.6).
const (
	msgFramebufferUpdate  = 0
	msgSetColourMapEntrie = 1
	msgBell               = 2
	msgServerCutText      = 3
)

// Encoding numbers. The negative ones are "pseudo-encodings": a client lists
// them to advertise a capability, not to request a way of drawing pixels.
const (
	EncRaw         int32 = 0
	EncCopyRect    int32 = 1
	EncRRE         int32 = 2
	EncHextile     int32 = 5
	EncTight       int32 = 7
	EncZRLE        int32 = 16
	EncCursor      int32 = -239
	EncDesktopSize int32 = -223
)

// Rect is a rectangle in framebuffer coordinates. X/Y are the top-left corner.
type Rect struct {
	X, Y, W, H int
}

// Empty reports whether the rectangle covers no pixels.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Intersect returns the overlap of r and o, or an empty rect if they are
// disjoint.
func (r Rect) Intersect(o Rect) Rect {
	x0, y0 := max(r.X, o.X), max(r.Y, o.Y)
	x1, y1 := min(r.X+r.W, o.X+o.W), min(r.Y+r.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// Union returns the smallest rectangle containing both r and o. An empty
// operand is ignored rather than dragging the result to the origin.
func (r Rect) Union(o Rect) Rect {
	if r.Empty() {
		return o
	}
	if o.Empty() {
		return r
	}
	x0, y0 := min(r.X, o.X), min(r.Y, o.Y)
	x1, y1 := max(r.X+r.W, o.X+o.W), max(r.Y+r.H, o.Y+o.H)
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// PixelFormat describes how one pixel is laid out on the wire (RFB §7.4).
// The client picks it with SetPixelFormat; we convert every frame into it.
type PixelFormat struct {
	BPP        uint8 // bits per pixel: 8, 16 or 32
	Depth      uint8 // significant bits per pixel
	BigEndian  bool
	TrueColour bool
	RedMax     uint16
	GreenMax   uint16
	BlueMax    uint16
	RedShift   uint8
	GreenShift uint8
	BlueShift  uint8
}

// DefaultPixelFormat is what we advertise in ServerInit: 32bpp little-endian
// true colour, 8 bits per channel, laid out so that a pixel word reads as
// 0x00RRGGBB. Clients are free to ask for something else.
func DefaultPixelFormat() PixelFormat {
	return PixelFormat{
		BPP:        32,
		Depth:      24,
		BigEndian:  false,
		TrueColour: true,
		RedMax:     255,
		GreenMax:   255,
		BlueMax:    255,
		RedShift:   16,
		GreenShift: 8,
		BlueShift:  0,
	}
}

// BytesPerPixel is the on-wire size of one pixel.
func (p PixelFormat) BytesPerPixel() int { return int(p.BPP) / 8 }

// isStandard32 reports whether the format is the common 32bpp true-colour
// layout with full 8-bit channels. Those formats have a fast conversion path;
// anything else falls back to the generic shift-and-mask encoder.
func (p PixelFormat) isStandard32() bool {
	return p.BPP == 32 && p.TrueColour &&
		p.RedMax == 255 && p.GreenMax == 255 && p.BlueMax == 255
}

// marshal writes the 16-byte on-wire PixelFormat (3 trailing padding bytes).
func (p PixelFormat) marshal(b []byte) {
	_ = b[15]
	b[0] = p.BPP
	b[1] = p.Depth
	b[2] = boolByte(p.BigEndian)
	b[3] = boolByte(p.TrueColour)
	binary.BigEndian.PutUint16(b[4:6], p.RedMax)
	binary.BigEndian.PutUint16(b[6:8], p.GreenMax)
	binary.BigEndian.PutUint16(b[8:10], p.BlueMax)
	b[10] = p.RedShift
	b[11] = p.GreenShift
	b[12] = p.BlueShift
	b[13], b[14], b[15] = 0, 0, 0
}

// unmarshalPixelFormat parses the 16-byte on-wire form and rejects formats we
// cannot encode into. Validating here means the per-frame encoders can assume
// a sane format instead of re-checking on every tile.
func unmarshalPixelFormat(b []byte) (PixelFormat, error) {
	if len(b) < 16 {
		return PixelFormat{}, io.ErrUnexpectedEOF
	}
	p := PixelFormat{
		BPP:        b[0],
		Depth:      b[1],
		BigEndian:  b[2] != 0,
		TrueColour: b[3] != 0,
		RedMax:     binary.BigEndian.Uint16(b[4:6]),
		GreenMax:   binary.BigEndian.Uint16(b[6:8]),
		BlueMax:    binary.BigEndian.Uint16(b[8:10]),
		RedShift:   b[10],
		GreenShift: b[11],
		BlueShift:  b[12],
	}
	switch p.BPP {
	case 8, 16, 32:
	default:
		return PixelFormat{}, fmt.Errorf("rfb: unsupported bits-per-pixel %d", p.BPP)
	}
	// A colour-map client would need SetColourMapEntries and an 8-bit palette.
	// No client we care about asks for it, and silently sending true-colour
	// pixels to a colour-map client renders garbage — so refuse it loudly.
	if !p.TrueColour {
		return PixelFormat{}, fmt.Errorf("rfb: colour-map pixel formats are not supported")
	}
	if p.Depth == 0 || int(p.Depth) > int(p.BPP) {
		return PixelFormat{}, fmt.Errorf("rfb: invalid depth %d for %d bpp", p.Depth, p.BPP)
	}
	return p, nil
}

// KeyEvent is a key press or release. Key is an X11 keysym, which is what RFB
// carries regardless of the client's actual keyboard.
type KeyEvent struct {
	Down bool
	Key  uint32
}

// PointerEvent is an absolute cursor position plus the current button mask.
// Bit 0 is button 1 (left), bit 1 middle, bit 2 right, bits 3/4 wheel up/down.
type PointerEvent struct {
	X, Y       int
	ButtonMask uint8
}

// Button masks within PointerEvent.ButtonMask.
const (
	ButtonLeft      uint8 = 1 << 0
	ButtonMiddle    uint8 = 1 << 1
	ButtonRight     uint8 = 1 << 2
	ButtonWheelUp   uint8 = 1 << 3
	ButtonWheelDown uint8 = 1 << 4
)

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
