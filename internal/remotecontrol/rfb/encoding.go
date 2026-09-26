package rfb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
)

// zrleTile is fixed by the protocol: ZRLE always works in 64x64 blocks.
const zrleTile = 64

// encodeRaw appends a Raw rectangle: every pixel, row-major, in the client's
// format. Raw is the only encoding a client is required to support, so it is
// the fallback when nothing better was negotiated. It is also enormous — a
// 1080p frame is ~8 MB — which is why ZRLE is preferred whenever the client
// offers it.
func encodeRaw(dst []byte, im *Image, r Rect, conv pixelConverter) []byte {
	return encodePixels(dst, im, r, conv)
}

// cpixel converts one BGRA pixel to a ZRLE "compressed pixel" and appends it.
type cpixel func(dst []byte, b, g, r byte) []byte

// cpixelFor returns the CPIXEL writer for pf and its width in bytes.
//
// RFB §7.7.6: a CPIXEL is normally identical to a PIXEL, but when the format is
// 32bpp true-colour with depth <= 24 and all three colour channels fit inside
// either the least- or most-significant 3 bytes, the redundant byte is dropped
// and a CPIXEL is only 3 bytes. That 25% saving applies before zlib, so it is
// worth the special case.
func cpixelFor(pf PixelFormat) (cpixel, int) {
	conv := converterFor(pf)
	bpp := pf.BytesPerPixel()

	if pf.BPP != 32 || !pf.TrueColour || pf.Depth > 24 {
		return cpixel(conv), bpp
	}

	mask := uint32(pf.RedMax)<<pf.RedShift |
		uint32(pf.GreenMax)<<pf.GreenShift |
		uint32(pf.BlueMax)<<pf.BlueShift

	switch {
	case mask&0xFF000000 == 0:
		// Channels live in the low 3 bytes. Little-endian puts those first;
		// big-endian puts them last.
		if !pf.BigEndian {
			return func(dst []byte, b, g, r byte) []byte {
				n := len(dst)
				dst = conv(dst, b, g, r)
				return append(dst[:n], dst[n:n+3]...)
			}, 3
		}
		return func(dst []byte, b, g, r byte) []byte {
			n := len(dst)
			dst = conv(dst, b, g, r)
			return append(dst[:n], dst[n+1:n+4]...)
		}, 3
	case mask&0x000000FF == 0:
		// Channels live in the high 3 bytes — the mirror of the case above.
		if !pf.BigEndian {
			return func(dst []byte, b, g, r byte) []byte {
				n := len(dst)
				dst = conv(dst, b, g, r)
				return append(dst[:n], dst[n+1:n+4]...)
			}, 3
		}
		return func(dst []byte, b, g, r byte) []byte {
			n := len(dst)
			dst = conv(dst, b, g, r)
			return append(dst[:n], dst[n:n+3]...)
		}, 3
	}
	return cpixel(conv), bpp
}

// zrleEncoder holds the per-connection ZRLE state.
//
// ZRLE uses ONE zlib stream for the whole connection, not one per rectangle —
// the dictionary carries across updates, which is most of where its compression
// comes from. It follows that this type is not safe for concurrent use and that
// a dropped or reordered rectangle desynchronises the client permanently.
type zrleEncoder struct {
	out  bytes.Buffer   // compressed bytes for the rectangle being built
	zw   *zlib.Writer   // writes into out
	tile []byte         // scratch for the uncompressed tile stream
	pal  [16]uint32     // scratch palette, tile-local
	idx  map[uint32]int // colour -> palette index, tile-local
}

func newZRLEEncoder() *zrleEncoder {
	e := &zrleEncoder{idx: make(map[uint32]int, 16)}
	e.zw = zlib.NewWriter(&e.out)
	return e
}

// encode appends a ZRLE rectangle body: a 4-byte length followed by the
// zlib-compressed tile stream.
func (e *zrleEncoder) encode(dst []byte, im *Image, r Rect, pf PixelFormat) ([]byte, error) {
	cp, _ := cpixelFor(pf)

	e.tile = e.tile[:0]
	for ty := r.Y; ty < r.Y+r.H; ty += zrleTile {
		th := min(zrleTile, r.Y+r.H-ty)
		for tx := r.X; tx < r.X+r.W; tx += zrleTile {
			tw := min(zrleTile, r.X+r.W-tx)
			e.tile = e.encodeTile(e.tile, im, Rect{X: tx, Y: ty, W: tw, H: th}, cp)
		}
	}

	e.out.Reset()
	if _, err := e.zw.Write(e.tile); err != nil {
		return nil, fmt.Errorf("rfb: zrle deflate: %w", err)
	}
	// Z_SYNC_FLUSH — ends the rectangle at a byte boundary so the client can
	// decode it now, while keeping the dictionary for the next one.
	if err := e.zw.Flush(); err != nil {
		return nil, fmt.Errorf("rfb: zrle flush: %w", err)
	}

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(e.out.Len())) // #nosec G115 -- a rectangle never reaches 4 GiB
	dst = append(dst, hdr[:]...)
	return append(dst, e.out.Bytes()...), nil
}

// encodeTile appends one 64x64-or-smaller tile, picking the cheapest of the
// three subencodings we emit:
//
//	solid (1)            — the whole tile is one colour. Desktop wallpaper,
//	                       window chrome and empty space all collapse to 4 bytes.
//	packed palette (2-16) — few distinct colours. This is the big one for UI and
//	                       text, where a tile is usually 2-8 colours.
//	raw (0)              — anything busier, e.g. photos or video. zlib still
//	                       compresses it.
//
// The RLE subencodings (128, 130-255) are deliberately not emitted: they are
// optional, and with solid+palette already covering flat regions the extra
// gain does not pay for the complexity. Clients must accept all of these.
func (e *zrleEncoder) encodeTile(dst []byte, im *Image, t Rect, cp cpixel) []byte {
	// Build the tile palette, bailing out as soon as it exceeds 16 entries.
	clear(e.idx)
	n := 0
	tooMany := false
	for y := t.Y; y < t.Y+t.H && !tooMany; y++ {
		rowOff := y * im.Stride
		for x := t.X; x < t.X+t.W; x++ {
			off := rowOff + x*4
			key := uint32(im.Pix[off]) | uint32(im.Pix[off+1])<<8 | uint32(im.Pix[off+2])<<16
			if _, ok := e.idx[key]; ok {
				continue
			}
			if n == len(e.pal) {
				tooMany = true
				break
			}
			e.pal[n] = key
			e.idx[key] = n
			n++
		}
	}

	switch {
	case !tooMany && n == 1:
		dst = append(dst, 1) // solid
		c := e.pal[0]
		return cp(dst, byte(c), byte(c>>8), byte(c>>16))

	case !tooMany && n <= 16:
		dst = append(dst, byte(n)) // packed palette
		for i := 0; i < n; i++ {
			c := e.pal[i]
			dst = cp(dst, byte(c), byte(c>>8), byte(c>>16))
		}
		return e.packIndices(dst, im, t, n)
	}

	dst = append(dst, 0) // raw
	for y := t.Y; y < t.Y+t.H; y++ {
		rowOff := y * im.Stride
		for x := t.X; x < t.X+t.W; x++ {
			off := rowOff + x*4
			dst = cp(dst, im.Pix[off], im.Pix[off+1], im.Pix[off+2])
		}
	}
	return dst
}

// packIndices appends the bit-packed palette indices for a tile.
//
// Index width is 1 bit for a 2-colour palette, 2 bits for 3-4 and 4 bits for
// 5-16. Each ROW is packed independently and padded to a byte boundary — a
// detail that is easy to miss and produces a diagonally-skewed image when
// missed.
func (e *zrleEncoder) packIndices(dst []byte, im *Image, t Rect, palSize int) []byte {
	var bits int
	switch {
	case palSize == 2:
		bits = 1
	case palSize <= 4:
		bits = 2
	default:
		bits = 4
	}

	for y := t.Y; y < t.Y+t.H; y++ {
		rowOff := y * im.Stride
		var acc byte
		used := 0
		for x := t.X; x < t.X+t.W; x++ {
			off := rowOff + x*4
			key := uint32(im.Pix[off]) | uint32(im.Pix[off+1])<<8 | uint32(im.Pix[off+2])<<16
			acc = acc<<bits | byte(e.idx[key])
			used += bits
			if used == 8 {
				dst = append(dst, acc)
				acc, used = 0, 0
			}
		}
		if used > 0 {
			dst = append(dst, acc<<(8-used)) // pad the tail of the row
		}
	}
	return dst
}
