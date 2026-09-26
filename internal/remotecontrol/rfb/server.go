package rfb

import (
	"bufio"
	"context"
	"crypto/des" // #nosec G503 -- DES is what RFB VNC authentication specifies; it is not our choice
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Source supplies screen contents. The capture package implements it per OS.
type Source interface {
	// Size is the current framebuffer size in pixels.
	Size() (w, h int)

	// Frame returns the latest screen image together with the regions that
	// changed since the previous call. A nil damage slice means "nothing
	// changed"; to force a full send, return the whole bounds.
	//
	// The returned Image is borrowed until the next Frame call — the server
	// finishes encoding before asking again, so a capturer may reuse its
	// buffer instead of allocating per frame.
	Frame(ctx context.Context) (*Image, []Rect, error)

	Close() error
}

// Sink receives what the viewer does. The input package implements it per OS.
type Sink interface {
	Key(KeyEvent) error
	Pointer(PointerEvent) error
	// SetClipboard is called when the viewer copies something on their side.
	SetClipboard(text string) error
}

// Config configures a Server.
type Config struct {
	// Name is the desktop name shown by the viewer.
	Name string

	// Password enables VNC authentication when non-empty. Only the first 8
	// bytes are significant — that is a hard limit of the RFB auth scheme, not
	// of this implementation, which is why the transport matters far more:
	// the server is expected to be reachable only over the agent's
	// authenticated gRPC tunnel.
	Password string

	Source Source
	Sink   Sink
	Logger *zap.Logger

	// MaxFPS caps how often the screen is sampled. Zero means 30.
	MaxFPS int

	// IdleTimeout ends a session after this long with no keyboard or mouse
	// input from the viewer. Zero disables it.
	//
	// It is enforced HERE rather than in the browser because the browser is
	// not trustworthy for this: a crashed tab, a closed laptop or a hung
	// renderer would otherwise leave someone's screen shared indefinitely.
	// Framebuffer update requests deliberately do not count as activity —
	// clients send those continuously whether or not anybody is watching.
	IdleTimeout time.Duration

	// OnConnect runs after the handshake and before any pixels are sent.
	// Returning an error rejects the session — this is where the consent
	// prompt goes, so that a viewer cannot see anything before the user on the
	// machine has agreed.
	OnConnect func(remoteAddr string) error

	// OnDisconnect runs once a session ends, however it ends.
	OnDisconnect func(remoteAddr string)
}

// Server accepts RFB connections. A single Server handles one viewer at a
// time: a second connection is refused rather than queued, because silently
// sharing a desktop with an unseen second viewer is exactly the surprise the
// consent gate exists to prevent.
type Server struct {
	cfg    Config
	logger *zap.Logger

	mu      sync.Mutex
	active  *conn
	clipbrd string
}

// New creates a Server. It does not listen; call Serve.
func New(cfg Config) (*Server, error) {
	if cfg.Source == nil {
		return nil, errors.New("rfb: Config.Source is required")
	}
	if cfg.Sink == nil {
		return nil, errors.New("rfb: Config.Sink is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.MaxFPS <= 0 {
		cfg.MaxFPS = 30
	}
	if cfg.Name == "" {
		cfg.Name = "wxt-agent"
	}
	return &Server{cfg: cfg, logger: cfg.Logger}, nil
}

// Serve accepts connections until ctx is cancelled or ln fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down
			}
			return fmt.Errorf("rfb: accept: %w", err)
		}

		s.mu.Lock()
		busy := s.active != nil
		s.mu.Unlock()
		if busy {
			s.logger.Warn("Rejecting RFB connection — a session is already active",
				zap.String("remote", nc.RemoteAddr().String()))
			_ = nc.Close()
			continue
		}

		go s.handle(ctx, nc)
	}
}

// SetClipboard pushes text to the connected viewer. It is a no-op when nobody
// is connected.
func (s *Server) SetClipboard(text string) {
	s.mu.Lock()
	s.clipbrd = text
	c := s.active
	s.mu.Unlock()
	if c != nil {
		c.sendServerCutText(text)
	}
}

func (s *Server) handle(ctx context.Context, nc net.Conn) {
	remote := nc.RemoteAddr().String()

	// established records whether this connection ever became THE session.
	//
	// Plenty of connections never do: a failed handshake, a refused consent, a
	// lost race with another viewer. Firing OnDisconnect for those would tear
	// down a session somebody else is actively using — the caller's
	// OnDisconnect is what ends the whole remote-control session — so a
	// connection that never established anything must exit silently.
	established := false

	c := &conn{
		srv:    s,
		nc:     nc,
		r:      bufio.NewReaderSize(nc, 8<<10),
		w:      bufio.NewWriterSize(nc, 256<<10),
		pf:     DefaultPixelFormat(),
		zrle:   newZRLEEncoder(),
		enc:    map[int32]bool{EncRaw: true},
		logger: s.logger.With(zap.String("remote", remote)),
	}

	defer func() {
		_ = nc.Close()
		if !established {
			return
		}
		s.mu.Lock()
		// Only clear the slot if it is still ours. Without this check a
		// connection tearing down could evict a newer session that had already
		// taken its place.
		if s.active == c {
			s.active = nil
		}
		s.mu.Unlock()
		if s.cfg.OnDisconnect != nil {
			s.cfg.OnDisconnect(remote)
		}
		s.logger.Info("RFB session ended", zap.String("remote", remote))
	}()

	if err := c.handshake(); err != nil {
		s.logger.Warn("RFB handshake failed", zap.String("remote", remote), zap.Error(err))
		return
	}

	// Consent gate. Nothing has been sent but the handshake at this point, so
	// a denial leaks no screen content.
	if s.cfg.OnConnect != nil {
		if err := s.cfg.OnConnect(remote); err != nil {
			s.logger.Info("RFB session refused", zap.String("remote", remote), zap.Error(err))
			return
		}
	}

	s.mu.Lock()
	if s.active != nil { // lost a race with another Accept
		s.mu.Unlock()
		return
	}
	s.active = c
	established = true
	pending := s.clipbrd
	s.mu.Unlock()

	s.logger.Info("RFB session started", zap.String("remote", remote))
	if pending != "" {
		c.sendServerCutText(pending)
	}
	c.run(ctx)
}

// conn is one viewer session.
type conn struct {
	srv    *Server
	nc     net.Conn
	r      *bufio.Reader
	logger *zap.Logger

	// wmu guards w: the frame loop and asynchronous clipboard pushes both
	// write, and interleaving two messages corrupts the stream irrecoverably.
	wmu sync.Mutex
	w   *bufio.Writer

	pf   PixelFormat
	enc  map[int32]bool
	zrle *zrleEncoder

	// Input counters, touched only by the read loop. They exist so a session
	// where input silently goes nowhere can be told apart from one where the
	// viewer never sent any.
	keysSeen     uint64
	pointersSeen uint64
	injectFails  uint64

	// lastInput is unix nanos of the most recent key or pointer event. Written
	// by the read loop, read by the frame loop, so it is atomic.
	lastInput atomic.Int64

	// rmu guards the fields the reader goroutine updates and the frame loop
	// consumes.
	rmu        sync.Mutex
	wantUpdate bool
	wantRect   Rect
	wantFull   bool // a non-incremental request forces a full resend
	lastW      int
	lastH      int
}

func (c *conn) handshake() error {
	if err := c.nc.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = c.nc.SetDeadline(time.Time{}) }()

	// ProtocolVersion exchange.
	if _, err := c.w.WriteString(protocolVersion); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	var ver [12]byte
	if _, err := io.ReadFull(c.r, ver[:]); err != nil {
		return fmt.Errorf("read client version: %w", err)
	}
	if string(ver[:4]) != "RFB " {
		return fmt.Errorf("not an RFB client (got %q)", ver[:])
	}

	if err := c.security(); err != nil {
		return err
	}

	// ClientInit: one byte, the shared-desktop flag. We always allow the
	// session regardless, so the value is read only to keep the stream aligned.
	if _, err := c.r.ReadByte(); err != nil {
		return fmt.Errorf("read ClientInit: %w", err)
	}

	// ServerInit.
	w, h := c.srv.cfg.Source.Size()
	c.lastW, c.lastH = w, h
	name := []byte(c.srv.cfg.Name)
	buf := make([]byte, 0, 24+len(name))
	var hdr [24]byte
	binary.BigEndian.PutUint16(hdr[0:2], uint16(w)) // #nosec G115 -- screen dimensions
	binary.BigEndian.PutUint16(hdr[2:4], uint16(h)) // #nosec G115 -- screen dimensions
	c.pf.marshal(hdr[4:20])
	binary.BigEndian.PutUint32(hdr[20:24], uint32(len(name))) // #nosec G115 -- desktop name is short
	buf = append(buf, hdr[:]...)
	buf = append(buf, name...)
	if _, err := c.w.Write(buf); err != nil {
		return err
	}
	return c.w.Flush()
}

// security runs the security handshake. We offer exactly one type so there is
// no room for a client to negotiate down to something weaker than configured.
func (c *conn) security() error {
	want := byte(secTypeNone)
	if c.srv.cfg.Password != "" {
		want = secTypeVNCAuth
	}
	if _, err := c.w.Write([]byte{1, want}); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	chosen, err := c.r.ReadByte()
	if err != nil {
		return fmt.Errorf("read security type: %w", err)
	}
	if chosen != want {
		return fmt.Errorf("client chose unoffered security type %d", chosen)
	}

	if want == secTypeVNCAuth {
		if err := c.vncAuth(); err != nil {
			// SecurityResult = 1 (failed). RFB 3.8 allows a reason string, but
			// naming the cause only helps someone probing the port.
			_, _ = c.w.Write([]byte{0, 0, 0, 1, 0, 0, 0, 0})
			_ = c.w.Flush()
			return err
		}
	}

	// SecurityResult = 0 (OK).
	if _, err := c.w.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	return c.w.Flush()
}

// vncAuth performs the DES challenge-response of RFB §7.2.2.
func (c *conn) vncAuth() error {
	var challenge [16]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(challenge[:]); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	var got [16]byte
	if _, err := io.ReadFull(c.r, got[:]); err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}

	want, err := vncEncrypt(challenge[:], c.srv.cfg.Password)
	if err != nil {
		return err
	}
	// The comparison need not be constant-time against a remote attacker here
	// (the listener is loopback-only behind the gRPC tunnel), but it costs
	// nothing to avoid the habit of leaking timing on a secret.
	var diff byte
	for i := range want {
		diff |= want[i] ^ got[i]
	}
	if diff != 0 {
		return errors.New("rfb: VNC authentication failed")
	}
	return nil
}

// vncEncrypt DES-encrypts the challenge with the password as key.
//
// RFB's scheme is odd in two ways that are easy to get wrong: the key is the
// password padded with NULs to exactly 8 bytes (longer passwords are cut), and
// every key byte has its bits reversed before use.
func vncEncrypt(challenge []byte, password string) ([]byte, error) {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		key[i] = bitReverse(b)
	}
	block, err := des.NewCipher(key[:]) // #nosec G405 -- mandated by the RFB spec
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(challenge))
	for i := 0; i+8 <= len(challenge); i += 8 {
		block.Encrypt(out[i:i+8], challenge[i:i+8])
	}
	return out, nil
}

func bitReverse(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = r<<1 | (b>>i)&1
	}
	return r
}

// run drives the session: one goroutine reads client messages, this one sends
// framebuffer updates at the configured rate.
func (c *conn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		defer cancel()
		if err := c.readLoop(ctx); err != nil && ctx.Err() == nil {
			c.logger.Debug("RFB read loop ended", zap.Error(err))
		}
	}()

	c.lastInput.Store(time.Now().UnixNano())

	interval := time.Second / time.Duration(c.srv.cfg.MaxFPS)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if c.idleExpired() {
				c.logger.Info("Ending the session — no viewer input",
					zap.Duration("idle_timeout", c.srv.cfg.IdleTimeout))
				return
			}
			if err := c.pushUpdate(ctx); err != nil {
				if ctx.Err() == nil {
					c.logger.Debug("RFB update failed", zap.Error(err))
				}
				return
			}
		}
	}
}

// idleExpired reports whether the viewer has been inactive past the timeout.
func (c *conn) idleExpired() bool {
	timeout := c.srv.cfg.IdleTimeout
	if timeout <= 0 {
		return false
	}
	last := time.Unix(0, c.lastInput.Load())
	return time.Since(last) > timeout
}

// noteInput records viewer activity, resetting the idle countdown.
func (c *conn) noteInput() { c.lastInput.Store(time.Now().UnixNano()) }

// pushUpdate sends one FramebufferUpdate if the client has an outstanding
// request and there is something to draw.
func (c *conn) pushUpdate(ctx context.Context) error {
	c.rmu.Lock()
	want, region, full := c.wantUpdate, c.wantRect, c.wantFull
	c.rmu.Unlock()
	if !want {
		return nil
	}

	img, damage, err := c.srv.cfg.Source.Frame(ctx)
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	if img == nil {
		return nil
	}

	// A resolution change invalidates every coordinate the client holds, so it
	// has to be announced before any pixels for the new size.
	if img.Width != c.lastW || img.Height != c.lastH {
		if c.enc[EncDesktopSize] {
			if err := c.sendDesktopSize(img.Width, img.Height); err != nil {
				return err
			}
			c.lastW, c.lastH = img.Width, img.Height
			c.clearRequest()
			return nil
		}
		// Without the pseudo-encoding the client cannot be told, so the best
		// we can do is keep sending what still fits inside its framebuffer.
		c.lastW, c.lastH = img.Width, img.Height
	}

	if full {
		damage = []Rect{img.Bounds()}
	}
	if len(damage) == 0 {
		return nil // nothing changed; leave the request outstanding
	}

	// Clip damage to what the client asked for and drop anything empty.
	clip := region.Intersect(img.Bounds())
	rects := make([]Rect, 0, len(damage))
	for _, d := range damage {
		if r := d.Intersect(clip); !r.Empty() {
			rects = append(rects, r)
		}
	}
	if len(rects) == 0 {
		return nil
	}

	if err := c.sendFramebufferUpdate(img, rects); err != nil {
		return err
	}
	c.clearRequest()
	return nil
}

func (c *conn) clearRequest() {
	c.rmu.Lock()
	c.wantUpdate, c.wantFull = false, false
	c.rmu.Unlock()
}

// bestEncoding picks the encoding to use for pixel data, preferring ZRLE
// because Raw at 1080p is roughly 8 MB per frame.
func (c *conn) bestEncoding() int32 {
	if c.enc[EncZRLE] {
		return EncZRLE
	}
	return EncRaw
}

func (c *conn) sendFramebufferUpdate(img *Image, rects []Rect) error {
	enc := c.bestEncoding()
	conv := converterFor(c.pf)

	body := make([]byte, 0, 64<<10)
	var hdr [4]byte
	hdr[0] = msgFramebufferUpdate
	hdr[1] = 0
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(rects))) // #nosec G115 -- bounded by damage count
	body = append(body, hdr[:]...)

	for _, r := range rects {
		body = appendRectHeader(body, r, enc)
		switch enc {
		case EncZRLE:
			var err error
			body, err = c.zrle.encode(body, img, r, c.pf)
			if err != nil {
				return err
			}
		default:
			body = encodeRaw(body, img, r, conv)
		}
	}

	return c.write(body)
}

func (c *conn) sendDesktopSize(w, h int) error {
	body := make([]byte, 0, 16)
	var hdr [4]byte
	hdr[0] = msgFramebufferUpdate
	binary.BigEndian.PutUint16(hdr[2:4], 1)
	body = append(body, hdr[:]...)
	body = appendRectHeader(body, Rect{W: w, H: h}, EncDesktopSize)
	return c.write(body)
}

func (c *conn) sendServerCutText(text string) {
	b := []byte(text)
	body := make([]byte, 0, 8+len(b))
	var hdr [8]byte
	hdr[0] = msgServerCutText
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(b))) // #nosec G115 -- clipboard payloads are bounded by the caller
	body = append(body, hdr[:]...)
	body = append(body, b...)
	if err := c.write(body); err != nil {
		c.logger.Debug("Failed to send clipboard to viewer", zap.Error(err))
	}
}

func (c *conn) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	return c.w.Flush()
}

func appendRectHeader(dst []byte, r Rect, enc int32) []byte {
	var h [12]byte
	binary.BigEndian.PutUint16(h[0:2], uint16(r.X))  // #nosec G115 -- framebuffer coords
	binary.BigEndian.PutUint16(h[2:4], uint16(r.Y))  // #nosec G115 -- framebuffer coords
	binary.BigEndian.PutUint16(h[4:6], uint16(r.W))  // #nosec G115 -- framebuffer coords
	binary.BigEndian.PutUint16(h[6:8], uint16(r.H))  // #nosec G115 -- framebuffer coords
	binary.BigEndian.PutUint32(h[8:12], uint32(enc)) // #nosec G115 -- encoding ids are small, negatives are intentional
	return append(dst, h[:]...)
}
