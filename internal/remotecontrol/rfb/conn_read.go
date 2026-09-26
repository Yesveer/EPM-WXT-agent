package rfb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"go.uber.org/zap"
)

// maxCutTextBytes caps an inbound clipboard paste. RFB gives the length as a
// 32-bit count, so without a limit a peer could make us allocate 4 GiB with an
// 8-byte message.
const maxCutTextBytes = 1 << 20 // 1 MiB

// maxEncodings caps SetEncodings for the same reason.
const maxEncodings = 1024

// readLoop consumes client-to-server messages until the peer goes away.
func (c *conn) readLoop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		msgType, err := c.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		switch msgType {
		case msgSetPixelFormat:
			err = c.readSetPixelFormat()
		case msgSetEncodings:
			err = c.readSetEncodings()
		case msgFramebufferUpdateRequest:
			err = c.readUpdateRequest()
		case msgKeyEvent:
			err = c.readKeyEvent()
		case msgPointerEvent:
			err = c.readPointerEvent()
		case msgClientCutText:
			err = c.readCutText()
		default:
			// An unknown message has an unknown length, so the stream can no
			// longer be parsed — there is nothing to do but drop the session.
			return fmt.Errorf("rfb: unknown client message type %d", msgType)
		}
		if err != nil {
			return err
		}
	}
}

func (c *conn) readSetPixelFormat() error {
	var b [19]byte // 3 padding + 16 PixelFormat
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	pf, err := unmarshalPixelFormat(b[3:])
	if err != nil {
		return err
	}

	// The format changes how every subsequent pixel is encoded, and ZRLE's
	// zlib dictionary holds pixels in the OLD format — carrying it across a
	// format change would decode as garbage. Reset the stream.
	c.wmu.Lock()
	c.pf = pf
	c.zrle = newZRLEEncoder()
	c.wmu.Unlock()

	// The client's framebuffer is undefined after a format change, so the next
	// update has to be a full one rather than incremental damage.
	c.rmu.Lock()
	c.wantFull = true
	c.rmu.Unlock()

	c.logger.Debug("Client set pixel format",
		zap.Uint8("bpp", pf.BPP), zap.Uint8("depth", pf.Depth), zap.Bool("big_endian", pf.BigEndian))
	return nil
}

func (c *conn) readSetEncodings() error {
	var b [3]byte // 1 padding + 2 count
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(b[1:3]))
	if n > maxEncodings {
		return fmt.Errorf("rfb: client offered %d encodings, refusing", n)
	}

	raw := make([]byte, 4*n)
	if _, err := io.ReadFull(c.r, raw); err != nil {
		return err
	}

	// Raw is mandatory for every client, so it stays available as a fallback
	// even if the client does not bother to list it.
	enc := map[int32]bool{EncRaw: true}
	names := make([]int32, 0, n)
	for i := 0; i < n; i++ {
		e := int32(binary.BigEndian.Uint32(raw[4*i : 4*i+4])) // #nosec G115 -- pseudo-encodings are deliberately negative
		enc[e] = true
		names = append(names, e)
	}

	c.wmu.Lock()
	c.enc = enc
	c.wmu.Unlock()

	c.logger.Debug("Client set encodings",
		zap.Int32s("encodings", names),
		zap.Bool("zrle", enc[EncZRLE]),
		zap.Bool("desktop_size", enc[EncDesktopSize]))
	return nil
}

func (c *conn) readUpdateRequest() error {
	var b [9]byte // incremental + x + y + w + h
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	incremental := b[0] != 0
	r := Rect{
		X: int(binary.BigEndian.Uint16(b[1:3])),
		Y: int(binary.BigEndian.Uint16(b[3:5])),
		W: int(binary.BigEndian.Uint16(b[5:7])),
		H: int(binary.BigEndian.Uint16(b[7:9])),
	}

	c.rmu.Lock()
	// Requests coalesce: a client may send several before we answer, and the
	// reply has to satisfy all of them, so widen the region rather than
	// replacing it.
	if c.wantUpdate {
		c.wantRect = c.wantRect.Union(r)
	} else {
		c.wantRect = r
	}
	c.wantUpdate = true
	if !incremental {
		c.wantFull = true
	}
	c.rmu.Unlock()
	return nil
}

func (c *conn) readKeyEvent() error {
	var b [7]byte // down-flag + 2 padding + keysym
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	ev := KeyEvent{Down: b[0] != 0, Key: binary.BigEndian.Uint32(b[3:7])}

	if c.keysSeen == 0 {
		// One line proving keyboard input reaches the machine at all. Without
		// it, "nothing happens when I type" is indistinguishable from "the
		// keystrokes never arrived".
		c.logger.Info("First keyboard event received from the viewer",
			zap.Uint32("keysym", ev.Key))
	}
	c.keysSeen++
	c.noteInput()

	if err := c.srv.cfg.Sink.Key(ev); err != nil {
		// Injection can fail transiently — a locked desktop, a UAC prompt
		// stealing focus. Dropping the session over it would be worse than
		// dropping the keystroke, so this warns and carries on.
		c.warnInjection("Key injection failed", err, zap.Uint32("keysym", ev.Key))
	}
	return nil
}

func (c *conn) readPointerEvent() error {
	var b [5]byte // button-mask + x + y
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	ev := PointerEvent{
		ButtonMask: b[0],
		X:          int(binary.BigEndian.Uint16(b[1:3])),
		Y:          int(binary.BigEndian.Uint16(b[3:5])),
	}
	if c.pointersSeen == 0 {
		c.logger.Info("First pointer event received from the viewer",
			zap.Int("x", ev.X), zap.Int("y", ev.Y))
	}
	c.pointersSeen++
	c.noteInput()

	if err := c.srv.cfg.Sink.Pointer(ev); err != nil {
		c.warnInjection("Pointer injection failed", err,
			zap.Int("x", ev.X), zap.Int("y", ev.Y))
	}
	return nil
}

func (c *conn) readCutText() error {
	var b [7]byte // 3 padding + 4 length
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(b[3:7])
	if n > maxCutTextBytes {
		return fmt.Errorf("rfb: clipboard payload of %d bytes exceeds limit", n)
	}
	if n == 0 {
		return nil
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return err
	}
	if err := c.srv.cfg.Sink.SetClipboard(string(buf)); err != nil {
		c.logger.Warn("Clipboard write failed", zap.Error(err))
	}
	return nil
}

// injectionWarnEvery rate-limits injection warnings. A pointer that cannot be
// injected fails on every mouse move, which at 60 Hz would bury every other
// line in the log.
const injectionWarnEvery = 50

// warnInjection reports an injection failure, logging the first and then every
// injectionWarnEvery-th occurrence.
func (c *conn) warnInjection(msg string, err error, fields ...zap.Field) {
	c.injectFails++
	if c.injectFails != 1 && c.injectFails%injectionWarnEvery != 0 {
		return
	}
	fields = append(fields, zap.Error(err), zap.Uint64("failures", c.injectFails))
	c.logger.Warn(msg, fields...)
}
