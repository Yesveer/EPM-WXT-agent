package rfb

import "encoding/binary"

// Image is one captured screen, stored as tightly-packed BGRA — 4 bytes per
// pixel, blue first, alpha ignored.
//
// BGRA is deliberate: it is what Windows Desktop Duplication hands back and
// what CoreGraphics produces on little-endian macOS, so the capture layer on
// both platforms can hand us its buffer without touching a single byte. The
// conversion to whatever the client asked for happens once, here, during
// encoding.
type Image struct {
	Width  int
	Height int
	Stride int // bytes per row; >= Width*4, rows may be padded
	Pix    []byte
}

// NewImage allocates a zeroed image with no row padding.
func NewImage(w, h int) *Image {
	return &Image{Width: w, Height: h, Stride: w * 4, Pix: make([]byte, w*h*4)}
}

// Bounds returns the whole image as a rectangle.
func (im *Image) Bounds() Rect { return Rect{W: im.Width, H: im.Height} }

// row returns the pixel bytes of row y, clipped to the image width.
func (im *Image) row(y int) []byte {
	off := y * im.Stride
	return im.Pix[off : off+im.Width*4]
}

// pixelAt returns the BGRA bytes of one pixel.
func (im *Image) pixelAt(x, y int) (b, g, r byte) {
	off := y*im.Stride + x*4
	return im.Pix[off], im.Pix[off+1], im.Pix[off+2]
}

// pixelConverter turns one BGRA pixel into the client's pixel format and
// appends it to dst. Resolving the format once per update — instead of
// branching on BPP and endianness for every pixel — is what keeps a full-screen
// Raw rectangle from being dominated by branch misprediction.
type pixelConverter func(dst []byte, b, g, r byte) []byte

// converterFor builds the fast path for pf.
func converterFor(pf PixelFormat) pixelConverter {
	bpp := pf.BytesPerPixel()

	// Fast path: 32bpp, 8 bits per channel. Every mainstream client (including
	// libvncclient as guacd configures it) lands here.
	if pf.isStandard32() {
		rs, gs, bs := pf.RedShift, pf.GreenShift, pf.BlueShift
		// The overwhelmingly common layout, 0x00RRGGBB little-endian, becomes a
		// straight byte copy with the red and blue lanes swapped.
		if !pf.BigEndian && rs == 16 && gs == 8 && bs == 0 {
			return func(dst []byte, b, g, r byte) []byte {
				return append(dst, b, g, r, 0)
			}
		}
		if pf.BigEndian && rs == 16 && gs == 8 && bs == 0 {
			return func(dst []byte, b, g, r byte) []byte {
				return append(dst, 0, r, g, b)
			}
		}
		// Unusual shifts: still 32bpp, but assemble the word explicitly.
		return func(dst []byte, b, g, r byte) []byte {
			v := uint32(r)<<rs | uint32(g)<<gs | uint32(b)<<bs
			var buf [4]byte
			if pf.BigEndian {
				binary.BigEndian.PutUint32(buf[:], v)
			} else {
				binary.LittleEndian.PutUint32(buf[:], v)
			}
			return append(dst, buf[:]...)
		}
	}

	// Generic path: scale each channel into its max, shift into place, emit the
	// low bpp bytes. Covers 8 and 16 bpp and any odd channel widths.
	rmax, gmax, bmax := uint32(pf.RedMax), uint32(pf.GreenMax), uint32(pf.BlueMax)
	rs, gs, bs := pf.RedShift, pf.GreenShift, pf.BlueShift
	return func(dst []byte, b, g, r byte) []byte {
		v := (uint32(r)*rmax/255)<<rs |
			(uint32(g)*gmax/255)<<gs |
			(uint32(b)*bmax/255)<<bs
		var buf [4]byte
		if pf.BigEndian {
			binary.BigEndian.PutUint32(buf[:], v)
			// Big-endian keeps the low-order bytes at the end of the word.
			return append(dst, buf[4-bpp:]...)
		}
		binary.LittleEndian.PutUint32(buf[:], v)
		return append(dst, buf[:bpp]...)
	}
}

// encodePixels appends every pixel of r, row-major, in the client's format.
func encodePixels(dst []byte, im *Image, r Rect, conv pixelConverter) []byte {
	for y := r.Y; y < r.Y+r.H; y++ {
		rowOff := y * im.Stride
		for x := r.X; x < r.X+r.W; x++ {
			off := rowOff + x*4
			dst = conv(dst, im.Pix[off], im.Pix[off+1], im.Pix[off+2])
		}
	}
	return dst
}
