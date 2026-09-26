package rfb

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- geometry --

func TestRectIntersect(t *testing.T) {
	tests := []struct {
		name string
		a, b Rect
		want Rect
	}{
		{"overlap", Rect{0, 0, 10, 10}, Rect{5, 5, 10, 10}, Rect{5, 5, 5, 5}},
		{"disjoint", Rect{0, 0, 5, 5}, Rect{10, 10, 5, 5}, Rect{}},
		{"contained", Rect{0, 0, 10, 10}, Rect{2, 2, 3, 3}, Rect{2, 2, 3, 3}},
		{"touching edges is empty", Rect{0, 0, 5, 5}, Rect{5, 0, 5, 5}, Rect{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Intersect(tc.b); got != tc.want {
				t.Errorf("Intersect = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRectUnionIgnoresEmpty(t *testing.T) {
	r := Rect{10, 10, 5, 5}
	// An empty operand must not drag the union back to the origin — that bug
	// would make every coalesced update a full-screen one.
	if got := r.Union(Rect{}); got != r {
		t.Errorf("Union with empty = %+v, want %+v", got, r)
	}
	if got := (Rect{}).Union(r); got != r {
		t.Errorf("empty.Union = %+v, want %+v", got, r)
	}
	if got := (Rect{0, 0, 2, 2}).Union(Rect{10, 10, 2, 2}); got != (Rect{0, 0, 12, 12}) {
		t.Errorf("Union = %+v, want {0 0 12 12}", got)
	}
}

// ------------------------------------------------------------ pixel format --

func TestPixelFormatRoundTrip(t *testing.T) {
	want := DefaultPixelFormat()
	var b [16]byte
	want.marshal(b[:])
	got, err := unmarshalPixelFormat(b[:])
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func TestPixelFormatRejectsUnsupported(t *testing.T) {
	base := DefaultPixelFormat()

	t.Run("colour map", func(t *testing.T) {
		pf := base
		pf.TrueColour = false
		var b [16]byte
		pf.marshal(b[:])
		if _, err := unmarshalPixelFormat(b[:]); err == nil {
			t.Fatal("expected colour-map format to be rejected")
		}
	})

	t.Run("odd bpp", func(t *testing.T) {
		pf := base
		pf.BPP = 24
		var b [16]byte
		pf.marshal(b[:])
		if _, err := unmarshalPixelFormat(b[:]); err == nil {
			t.Fatal("expected 24bpp to be rejected")
		}
	})

	t.Run("depth exceeds bpp", func(t *testing.T) {
		pf := base
		pf.Depth = 40
		var b [16]byte
		pf.marshal(b[:])
		if _, err := unmarshalPixelFormat(b[:]); err == nil {
			t.Fatal("expected depth > bpp to be rejected")
		}
	})
}

func TestPixelConversionDefaultFormat(t *testing.T) {
	// Default format is little-endian 0x00RRGGBB, so a BGRA source pixel goes
	// out unchanged in the first three bytes.
	conv := converterFor(DefaultPixelFormat())
	got := conv(nil, 0x11 /*B*/, 0x22 /*G*/, 0x33 /*R*/)
	want := []byte{0x11, 0x22, 0x33, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("conv = % x, want % x", got, want)
	}
}

func TestPixelConversionBigEndian(t *testing.T) {
	pf := DefaultPixelFormat()
	pf.BigEndian = true
	conv := converterFor(pf)
	got := conv(nil, 0x11, 0x22, 0x33)
	want := []byte{0x00, 0x33, 0x22, 0x11} // 0x00RRGGBB, most significant first
	if !bytes.Equal(got, want) {
		t.Errorf("conv = % x, want % x", got, want)
	}
}

func TestCPixelIsThreeBytesForDefaultFormat(t *testing.T) {
	cp, n := cpixelFor(DefaultPixelFormat())
	if n != 3 {
		t.Fatalf("cpixel width = %d, want 3 (the 25%% ZRLE saving)", n)
	}
	got := cp(nil, 0x11, 0x22, 0x33)
	if !bytes.Equal(got, []byte{0x11, 0x22, 0x33}) {
		t.Errorf("cpixel = % x, want 11 22 33", got)
	}
}

// -------------------------------------------------------------------- ZRLE --

// inflate decompresses a ZRLE payload.
//
// ZRLE ends each rectangle with Z_SYNC_FLUSH rather than closing the stream,
// so a reader fed only that rectangle hits the end of its input mid-stream.
// That surfaces as ErrUnexpectedEOF *after* all the real bytes have been
// delivered, which is expected here rather than a failure.
func inflate(t *testing.T, payload []byte) []byte {
	t.Helper()
	zr, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("inflate: %v", err)
	}
	return out
}

// splitZRLE peels the 4-byte length header off an encoded rectangle.
func splitZRLE(t *testing.T, b []byte) []byte {
	t.Helper()
	if len(b) < 4 {
		t.Fatalf("ZRLE body too short: %d bytes", len(b))
	}
	n := binary.BigEndian.Uint32(b[:4])
	if int(n) != len(b)-4 {
		t.Fatalf("ZRLE length header says %d, body has %d", n, len(b)-4)
	}
	return b[4:]
}

// TestZRLESolidTileExactBytes pins the wire format of the simplest possible
// tile against a hand-computed expectation. Without it, an encoder and a
// decoder written from the same misreading of the spec would agree with each
// other and still be wrong.
func TestZRLESolidTileExactBytes(t *testing.T) {
	im := NewImage(4, 4)
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i], im.Pix[i+1], im.Pix[i+2] = 0xAA, 0xBB, 0xCC // B, G, R
	}

	e := newZRLEEncoder()
	out, err := e.encode(nil, im, im.Bounds(), DefaultPixelFormat())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tiles := inflate(t, splitZRLE(t, out))

	// subencoding 1 (solid) + one 3-byte CPIXEL in B,G,R order.
	want := []byte{1, 0xAA, 0xBB, 0xCC}
	if !bytes.Equal(tiles, want) {
		t.Errorf("tile stream = % x, want % x", tiles, want)
	}
}

// TestZRLETwoColourPackingPadsEachRow guards the detail that is easiest to get
// wrong: with a packed palette, every ROW is padded to a byte boundary. Getting
// this wrong skews the image diagonally instead of failing outright.
func TestZRLETwoColourPackingPadsEachRow(t *testing.T) {
	// 3 pixels wide: 3 one-bit indices per row, so each row must occupy a whole
	// byte with 5 bits of padding — not pack continuously across rows.
	im := NewImage(3, 2)
	set := func(x, y int, b, g, r byte) {
		off := y*im.Stride + x*4
		im.Pix[off], im.Pix[off+1], im.Pix[off+2] = b, g, r
	}
	set(0, 0, 1, 1, 1)
	set(1, 0, 2, 2, 2)
	set(2, 0, 1, 1, 1)
	set(0, 1, 2, 2, 2)
	set(1, 1, 1, 1, 1)
	set(2, 1, 2, 2, 2)

	e := newZRLEEncoder()
	out, err := e.encode(nil, im, im.Bounds(), DefaultPixelFormat())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tiles := inflate(t, splitZRLE(t, out))

	// subencoding 2, palette [colour1, colour2], then one byte per row.
	// Row 0 is indices 0,1,0 -> bits 010 -> 0b01000000 = 0x40
	// Row 1 is indices 1,0,1 -> bits 101 -> 0b10100000 = 0xA0
	want := []byte{2, 1, 1, 1, 2, 2, 2, 0x40, 0xA0}
	if !bytes.Equal(tiles, want) {
		t.Errorf("tile stream = % x, want % x", tiles, want)
	}
}

// decodeZRLE is a minimal ZRLE client decoder used to prove round-trip
// fidelity over a realistic, multi-tile image.
func decodeZRLE(t *testing.T, tiles []byte, w, h int) *Image {
	t.Helper()
	out := NewImage(w, h)
	pos := 0
	next := func(n int) []byte {
		t.Helper()
		if pos+n > len(tiles) {
			t.Fatalf("tile stream truncated: need %d at %d of %d", n, pos, len(tiles))
		}
		b := tiles[pos : pos+n]
		pos += n
		return b
	}
	put := func(x, y int, px []byte) {
		off := y*out.Stride + x*4
		out.Pix[off], out.Pix[off+1], out.Pix[off+2] = px[0], px[1], px[2]
	}

	for ty := 0; ty < h; ty += zrleTile {
		th := min(zrleTile, h-ty)
		for tx := 0; tx < w; tx += zrleTile {
			tw := min(zrleTile, w-tx)
			sub := next(1)[0]

			switch {
			case sub == 1: // solid
				px := next(3)
				for y := 0; y < th; y++ {
					for x := 0; x < tw; x++ {
						put(tx+x, ty+y, px)
					}
				}

			case sub >= 2 && sub <= 16: // packed palette
				n := int(sub)
				pal := make([][]byte, n)
				for i := range pal {
					pal[i] = append([]byte(nil), next(3)...)
				}
				var bits int
				switch {
				case n == 2:
					bits = 1
				case n <= 4:
					bits = 2
				default:
					bits = 4
				}
				perRow := (tw*bits + 7) / 8
				for y := 0; y < th; y++ {
					row := next(perRow)
					for x := 0; x < tw; x++ {
						bitPos := x * bits
						b := row[bitPos/8]
						shift := 8 - bits - (bitPos % 8)
						idx := int(b>>shift) & ((1 << bits) - 1)
						if idx >= n {
							t.Fatalf("palette index %d out of range %d", idx, n)
						}
						put(tx+x, ty+y, pal[idx])
					}
				}

			case sub == 0: // raw
				for y := 0; y < th; y++ {
					for x := 0; x < tw; x++ {
						put(tx+x, ty+y, next(3))
					}
				}

			default:
				t.Fatalf("unexpected subencoding %d", sub)
			}
		}
	}
	if pos != len(tiles) {
		t.Fatalf("decoder consumed %d of %d tile bytes", pos, len(tiles))
	}
	return out
}

// TestZRLERoundTrip encodes an image that deliberately hits all three
// subencodings and a partial tile on both axes, then decodes it back and
// requires an exact match.
func TestZRLERoundTrip(t *testing.T) {
	const w, h = 200, 100 // tiles: 64,64,64,8 wide and 64,36 tall
	im := NewImage(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			off := y*im.Stride + x*4
			var b, g, r byte
			switch {
			case x < 64:
				b, g, r = 0x20, 0x40, 0x60 // solid tile
			case x < 128:
				if (x+y)%2 == 0 { // two-colour tile
					b, g, r = 0xFF, 0xFF, 0xFF
				} else {
					b, g, r = 0x00, 0x00, 0x00
				}
			default:
				// A gradient: far more than 16 colours, forcing raw tiles.
				b, g, r = byte(x), byte(y), byte(x*y)
			}
			im.Pix[off], im.Pix[off+1], im.Pix[off+2] = b, g, r
		}
	}

	e := newZRLEEncoder()
	out, err := e.encode(nil, im, im.Bounds(), DefaultPixelFormat())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := decodeZRLE(t, inflate(t, splitZRLE(t, out)), w, h)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			wb, wg, wr := im.pixelAt(x, y)
			gb, gg, gr := got.pixelAt(x, y)
			if wb != gb || wg != gg || wr != gr {
				t.Fatalf("pixel (%d,%d) = %02x%02x%02x, want %02x%02x%02x", x, y, gb, gg, gr, wb, wg, wr)
			}
		}
	}
}

// TestZRLEDictionaryPersistsAcrossRectangles documents the property the rest of
// the server depends on: the zlib stream is shared, so encoding the same
// content twice costs far less the second time. A regression that reset the
// stream per rectangle would still decode correctly but throw away most of the
// compression.
func TestZRLEDictionaryPersistsAcrossRectangles(t *testing.T) {
	im := NewImage(128, 128)
	for i := range im.Pix {
		im.Pix[i] = byte(i * 7)
	}

	e := newZRLEEncoder()
	first, err := e.encode(nil, im, im.Bounds(), DefaultPixelFormat())
	if err != nil {
		t.Fatalf("first encode: %v", err)
	}
	second, err := e.encode(nil, im, im.Bounds(), DefaultPixelFormat())
	if err != nil {
		t.Fatalf("second encode: %v", err)
	}
	if len(second) >= len(first) {
		t.Errorf("repeat encode was %d bytes vs %d — the zlib dictionary is not carrying over",
			len(second), len(first))
	}
}

// --------------------------------------------------------------- VNC auth --

func TestVNCEncryptKnownAnswer(t *testing.T) {
	// The password key is NUL-padded to 8 bytes and each byte bit-reversed
	// before use as a DES key. This pins that behaviour: "pass" -> key bytes
	// are reverse("pass\0\0\0\0").
	challenge := make([]byte, 16)
	for i := range challenge {
		challenge[i] = byte(i)
	}
	got, err := vncEncrypt(challenge, "pass")
	if err != nil {
		t.Fatalf("vncEncrypt: %v", err)
	}
	if len(got) != 16 {
		t.Fatalf("response length = %d, want 16", len(got))
	}
	// A different password must produce a different response.
	other, err := vncEncrypt(challenge, "different")
	if err != nil {
		t.Fatalf("vncEncrypt: %v", err)
	}
	if bytes.Equal(got, other) {
		t.Error("different passwords produced identical responses")
	}
	// Only the first 8 bytes of the password are significant.
	trunc, err := vncEncrypt(challenge, "different-suffix-ignored")
	if err != nil {
		t.Fatalf("vncEncrypt: %v", err)
	}
	if !bytes.Equal(other, trunc) {
		t.Error("bytes past the 8th changed the response; RFB truncates the key")
	}
}

func TestBitReverse(t *testing.T) {
	cases := map[byte]byte{0x01: 0x80, 0x80: 0x01, 0xF0: 0x0F, 0x00: 0x00, 0xFF: 0xFF}
	for in, want := range cases {
		if got := bitReverse(in); got != want {
			t.Errorf("bitReverse(%02x) = %02x, want %02x", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- session --

type fakeSource struct {
	mu     sync.Mutex
	img    *Image
	damage []Rect
}

func newFakeSource(w, h int) *fakeSource {
	im := NewImage(w, h)
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i], im.Pix[i+1], im.Pix[i+2] = 0x10, 0x20, 0x30
	}
	return &fakeSource{img: im, damage: []Rect{im.Bounds()}}
}

func (f *fakeSource) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.img.Width, f.img.Height
}

func (f *fakeSource) Frame(context.Context) (*Image, []Rect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.img, f.damage, nil
}

func (f *fakeSource) Close() error { return nil }

type fakeSink struct {
	mu        sync.Mutex
	keys      []KeyEvent
	pointers  []PointerEvent
	clipboard string
}

func (s *fakeSink) Key(e KeyEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, e)
	return nil
}

func (s *fakeSink) Pointer(e PointerEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pointers = append(s.pointers, e)
	return nil
}

func (s *fakeSink) SetClipboard(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clipboard = text
	return nil
}

func (s *fakeSink) snapshot() ([]KeyEvent, []PointerEvent, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]KeyEvent(nil), s.keys...), append([]PointerEvent(nil), s.pointers...), s.clipboard
}

// clientHandshake drives the viewer side of the handshake and returns the
// framebuffer size the server advertised.
func clientHandshake(t *testing.T, c net.Conn) (int, int) {
	t.Helper()

	var ver [12]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if string(ver[:]) != protocolVersion {
		t.Fatalf("server version = %q, want %q", ver[:], protocolVersion)
	}
	if _, err := c.Write([]byte(protocolVersion)); err != nil {
		t.Fatalf("write version: %v", err)
	}

	var nsec [1]byte
	if _, err := io.ReadFull(c, nsec[:]); err != nil {
		t.Fatalf("read security count: %v", err)
	}
	types := make([]byte, nsec[0])
	if _, err := io.ReadFull(c, types); err != nil {
		t.Fatalf("read security types: %v", err)
	}
	if len(types) != 1 || types[0] != secTypeNone {
		t.Fatalf("security types = %v, want [None]", types)
	}
	if _, err := c.Write([]byte{secTypeNone}); err != nil {
		t.Fatalf("select security: %v", err)
	}

	var res [4]byte
	if _, err := io.ReadFull(c, res[:]); err != nil {
		t.Fatalf("read SecurityResult: %v", err)
	}
	if binary.BigEndian.Uint32(res[:]) != 0 {
		t.Fatal("SecurityResult was not OK")
	}

	if _, err := c.Write([]byte{1}); err != nil { // ClientInit, shared
		t.Fatalf("write ClientInit: %v", err)
	}

	var si [24]byte
	if _, err := io.ReadFull(c, si[:]); err != nil {
		t.Fatalf("read ServerInit: %v", err)
	}
	w := int(binary.BigEndian.Uint16(si[0:2]))
	h := int(binary.BigEndian.Uint16(si[2:4]))
	nameLen := binary.BigEndian.Uint32(si[20:24])
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(c, name); err != nil {
		t.Fatalf("read desktop name: %v", err)
	}
	return w, h
}

func startServer(t *testing.T, cfg Config) (net.Conn, *Server) {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func TestHandshakeAndFramebufferUpdate(t *testing.T) {
	src := newFakeSource(96, 64)
	sink := &fakeSink{}
	c, _ := startServer(t, Config{Name: "test-desktop", Source: src, Sink: sink, MaxFPS: 60})

	w, h := clientHandshake(t, c)
	if w != 96 || h != 64 {
		t.Fatalf("ServerInit size = %dx%d, want 96x64", w, h)
	}

	// Ask for ZRLE, then request the whole screen.
	setEnc := []byte{msgSetEncodings, 0, 0, 1}
	setEnc = binary.BigEndian.AppendUint32(setEnc, uint32(EncZRLE))
	if _, err := c.Write(setEnc); err != nil {
		t.Fatalf("SetEncodings: %v", err)
	}
	req := []byte{msgFramebufferUpdateRequest, 0} // non-incremental
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, 96)
	req = binary.BigEndian.AppendUint16(req, 64)
	if _, err := c.Write(req); err != nil {
		t.Fatalf("UpdateRequest: %v", err)
	}

	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatalf("read update header: %v", err)
	}
	if hdr[0] != msgFramebufferUpdate {
		t.Fatalf("message type = %d, want FramebufferUpdate", hdr[0])
	}
	if n := binary.BigEndian.Uint16(hdr[2:4]); n != 1 {
		t.Fatalf("rect count = %d, want 1", n)
	}

	var rh [12]byte
	if _, err := io.ReadFull(c, rh[:]); err != nil {
		t.Fatalf("read rect header: %v", err)
	}
	if enc := int32(binary.BigEndian.Uint32(rh[8:12])); enc != EncZRLE {
		t.Fatalf("encoding = %d, want ZRLE (%d)", enc, EncZRLE)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		t.Fatalf("read ZRLE length: %v", err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(c, payload); err != nil {
		t.Fatalf("read ZRLE payload: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("empty ZRLE payload")
	}
}

func TestInputEventsReachSink(t *testing.T) {
	src := newFakeSource(64, 64)
	sink := &fakeSink{}
	c, _ := startServer(t, Config{Source: src, Sink: sink, MaxFPS: 60})
	clientHandshake(t, c)

	key := []byte{msgKeyEvent, 1, 0, 0}
	key = binary.BigEndian.AppendUint32(key, 0x0041) // keysym 'A'
	if _, err := c.Write(key); err != nil {
		t.Fatalf("KeyEvent: %v", err)
	}

	ptr := []byte{msgPointerEvent, ButtonLeft}
	ptr = binary.BigEndian.AppendUint16(ptr, 12)
	ptr = binary.BigEndian.AppendUint16(ptr, 34)
	if _, err := c.Write(ptr); err != nil {
		t.Fatalf("PointerEvent: %v", err)
	}

	text := "hello from the viewer"
	cut := []byte{msgClientCutText, 0, 0, 0}
	cut = binary.BigEndian.AppendUint32(cut, uint32(len(text)))
	cut = append(cut, text...)
	if _, err := c.Write(cut); err != nil {
		t.Fatalf("ClientCutText: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		keys, ptrs, clip := sink.snapshot()
		if len(keys) == 1 && len(ptrs) == 1 && clip == text {
			if !keys[0].Down || keys[0].Key != 0x41 {
				t.Errorf("key = %+v, want {Down:true Key:0x41}", keys[0])
			}
			if ptrs[0].X != 12 || ptrs[0].Y != 34 || ptrs[0].ButtonMask != ButtonLeft {
				t.Errorf("pointer = %+v, want {12 34 left}", ptrs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events did not arrive: keys=%d pointers=%d clipboard=%q", len(keys), len(ptrs), clip)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConsentDenialSendsNoPixels(t *testing.T) {
	src := newFakeSource(64, 64)
	c, _ := startServer(t, Config{
		Source:    src,
		Sink:      &fakeSink{},
		OnConnect: func(string) error { return errors.New("user declined") },
	})
	clientHandshake(t, c)

	// The handshake completes, then the server must hang up without sending a
	// single framebuffer byte.
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, err := c.Read(b[:])
	if err == nil {
		t.Fatalf("server sent byte %#x after consent was denied", b[0])
	}
	if !errors.Is(err, io.EOF) {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("connection stayed open after consent was denied; expected close")
		}
	}
}

func TestSecondViewerIsRejected(t *testing.T) {
	src := newFakeSource(64, 64)
	srvCfg := Config{Source: src, Sink: &fakeSink{}, MaxFPS: 60}

	srv, err := New(srvCfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()

	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	defer first.Close()
	clientHandshake(t, first)

	// Give the server a moment to register the session before racing it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		active := srv.active != nil
		srv.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first session never became active")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	defer second.Close()
	if err := second.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := second.Read(b[:]); err == nil {
		t.Fatal("second viewer was served instead of refused")
	}
}

func TestUnknownClientMessageEndsSession(t *testing.T) {
	src := newFakeSource(64, 64)
	c, _ := startServer(t, Config{Source: src, Sink: &fakeSink{}, MaxFPS: 60})
	clientHandshake(t, c)

	// 200 is not a defined client message type. Its length is unknowable, so
	// the stream can no longer be parsed and the session must end rather than
	// guess.
	if _, err := c.Write([]byte{200}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("session survived an unparseable message")
	}
}

func TestOversizedClipboardIsRefused(t *testing.T) {
	src := newFakeSource(64, 64)
	sink := &fakeSink{}
	c, _ := startServer(t, Config{Source: src, Sink: sink, MaxFPS: 60})
	clientHandshake(t, c)

	// Claim a 2 GiB paste without sending it. The server must refuse on the
	// length alone rather than try to allocate the buffer.
	cut := []byte{msgClientCutText, 0, 0, 0}
	cut = binary.BigEndian.AppendUint32(cut, 1<<31)
	if _, err := c.Write(cut); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("server accepted an oversized clipboard length")
	}
	if _, _, clip := sink.snapshot(); clip != "" {
		t.Errorf("clipboard was written: %q", clip)
	}
}

// TestFailedHandshakeDoesNotEndTheLiveSession is the regression test for a bug
// that killed working sessions: the teardown path fired OnDisconnect for EVERY
// connection, including ones that never got past the handshake. Since
// OnDisconnect is what ends the whole remote-control session, a single stray
// connect to the RFB port — a probe, a retry, a client that failed auth —
// would drop the admin who was already connected.
func TestFailedHandshakeDoesNotEndTheLiveSession(t *testing.T) {
	src := newFakeSource(64, 64)
	disconnects := make(chan string, 4)

	srv, err := New(Config{
		Source:       src,
		Sink:         &fakeSink{},
		MaxFPS:       60,
		OnDisconnect: func(remote string) { disconnects <- remote },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()

	// Something connects and hangs up without completing the handshake.
	junk, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = junk.Close()

	select {
	case remote := <-disconnects:
		t.Fatalf("OnDisconnect fired for a connection that never established a session (%s)", remote)
	case <-time.After(500 * time.Millisecond):
		// Correct: nothing was established, so nothing ended.
	}
}

// TestDisconnectFiresForAnEstablishedSession is the other half: a session that
// really did start must still report its end, or the portal would show a
// session that is long gone as still live.
func TestDisconnectFiresForAnEstablishedSession(t *testing.T) {
	src := newFakeSource(64, 64)
	disconnects := make(chan string, 4)

	srv, err := New(Config{
		Source:       src,
		Sink:         &fakeSink{},
		MaxFPS:       60,
		OnDisconnect: func(remote string) { disconnects <- remote },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	clientHandshake(t, c)
	_ = c.Close()

	select {
	case <-disconnects:
		// Correct.
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect never fired for an established session")
	}
}

// TestRejectedSecondViewerLeavesTheFirstAlone guards the slot bookkeeping: a
// refused second connection must not clear the active session's record, or the
// first viewer would be silently disowned while still connected.
func TestRejectedSecondViewerLeavesTheFirstAlone(t *testing.T) {
	src := newFakeSource(64, 64)
	srv, err := New(Config{Source: src, Sink: &fakeSink{}, MaxFPS: 60})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()

	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	defer first.Close()
	clientHandshake(t, first)

	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		active := srv.active != nil
		srv.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first session never became active")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	_ = second.Close()

	// Give the refused connection time to run its teardown.
	time.Sleep(300 * time.Millisecond)

	srv.mu.Lock()
	stillActive := srv.active != nil
	srv.mu.Unlock()
	if !stillActive {
		t.Fatal("a refused second viewer cleared the first viewer's session")
	}
}

// TestIdleTimeoutEndsTheSession covers the property that matters: a forgotten
// session stops sharing the user's screen on its own. Enforcing this on the
// machine rather than in the browser is deliberate — a crashed tab must not be
// able to leave a desktop exposed.
func TestIdleTimeoutEndsTheSession(t *testing.T) {
	src := newFakeSource(64, 64)
	c, _ := startServer(t, Config{
		Source:      src,
		Sink:        &fakeSink{},
		MaxFPS:      60,
		IdleTimeout: 250 * time.Millisecond,
	})
	clientHandshake(t, c)

	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("the session survived past its idle timeout")
	}
}

// TestInputResetsTheIdleCountdown makes sure an admin who is actually working
// is not disconnected mid-session.
func TestInputResetsTheIdleCountdown(t *testing.T) {
	src := newFakeSource(64, 64)
	c, _ := startServer(t, Config{
		Source:      src,
		Sink:        &fakeSink{},
		MaxFPS:      60,
		IdleTimeout: 400 * time.Millisecond,
	})
	clientHandshake(t, c)

	// Keep tapping a key for well over the timeout.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		key := []byte{msgKeyEvent, 1, 0, 0}
		key = binary.BigEndian.AppendUint32(key, 0x0041)
		if _, err := c.Write(key); err != nil {
			t.Fatalf("the session dropped while input was still arriving: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still alive.
	req := []byte{msgFramebufferUpdateRequest, 0}
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, 64)
	req = binary.BigEndian.AppendUint16(req, 64)
	if _, err := c.Write(req); err != nil {
		t.Fatalf("the session ended despite continuous input: %v", err)
	}
}

// Framebuffer update requests must NOT count as activity: clients send them
// continuously whether or not a human is present, so treating them as input
// would make the timeout never fire.
func TestUpdateRequestsDoNotCountAsActivity(t *testing.T) {
	src := newFakeSource(64, 64)
	c, _ := startServer(t, Config{
		Source:      src,
		Sink:        &fakeSink{},
		MaxFPS:      60,
		IdleTimeout: 300 * time.Millisecond,
	})
	clientHandshake(t, c)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-done:
				return
			default:
			}
			req := []byte{msgFramebufferUpdateRequest, 1}
			req = binary.BigEndian.AppendUint16(req, 0)
			req = binary.BigEndian.AppendUint16(req, 0)
			req = binary.BigEndian.AppendUint16(req, 64)
			req = binary.BigEndian.AppendUint16(req, 64)
			if _, err := c.Write(req); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Drain until the server hangs up. Reads succeed in the meantime — the
	// server is answering those update requests with real framebuffer data —
	// so the close is the only meaningful signal.
	buf := make([]byte, 4096)
	closed := false
	for {
		if _, err := c.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break // deadline hit: never closed
			}
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("the session survived — update requests were treated as activity")
	}
}

func TestZeroIdleTimeoutNeverExpires(t *testing.T) {
	src := newFakeSource(64, 64)
	c, _ := startServer(t, Config{Source: src, Sink: &fakeSink{}, MaxFPS: 60, IdleTimeout: 0})
	clientHandshake(t, c)

	time.Sleep(600 * time.Millisecond)

	req := []byte{msgFramebufferUpdateRequest, 0}
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, 64)
	req = binary.BigEndian.AppendUint16(req, 64)
	if _, err := c.Write(req); err != nil {
		t.Fatalf("a session with no timeout was closed anyway: %v", err)
	}
}
