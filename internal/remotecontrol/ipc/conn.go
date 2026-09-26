package ipc

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// maxFrameBytes bounds one JSON frame. Clipboard text is the largest thing
// that travels here and is already capped well below this.
const maxFrameBytes = 1 << 20 // 1 MiB

// handshakeTimeout bounds how long a freshly accepted connection may take to
// prove itself before being dropped.
const handshakeTimeout = 10 * time.Second

// newToken returns a fresh 256-bit authentication token.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ipc: generating token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// conn is a framed JSON connection. Frames are newline-delimited, which keeps
// the wire format trivially debuggable while still being unambiguous — no JSON
// value we send contains a raw newline once encoded.
type conn struct {
	nc net.Conn
	r  *bufio.Reader

	wmu sync.Mutex
	w   *bufio.Writer
}

func newConn(nc net.Conn) *conn {
	return &conn{
		nc: nc,
		r:  bufio.NewReaderSize(nc, 16<<10),
		w:  bufio.NewWriterSize(nc, 16<<10),
	}
}

func (c *conn) writeFrame(f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("ipc: encoding frame: %w", err)
	}
	if len(b) > maxFrameBytes {
		return fmt.Errorf("ipc: frame of %d bytes exceeds the limit", len(b))
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	if err := c.w.WriteByte('\n'); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *conn) readFrame() (frame, error) {
	line, err := c.readLine()
	if err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return frame{}, fmt.Errorf("ipc: decoding frame: %w", err)
	}
	return f, nil
}

// readLine reads one newline-terminated frame, refusing anything oversized
// rather than buffering it — an unbounded read here would let a confused peer
// exhaust memory.
func (c *conn) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxFrameBytes {
			return nil, fmt.Errorf("ipc: frame exceeds %d bytes", maxFrameBytes)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func (c *conn) close() error { return c.nc.Close() }

// authHello is the first frame the helper sends. Version is carried here so a
// mismatched helper is rejected at connect time rather than failing later on
// some message it does not understand.
type authHello struct {
	Token   string `json:"token"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// clientHandshake proves the helper's identity to the daemon.
func clientHandshake(c *conn, token, version string, pid int) error {
	if err := c.nc.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	defer func() { _ = c.nc.SetDeadline(time.Time{}) }()

	hello := authHello{Token: token, Version: version, PID: pid}
	raw, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if err := c.writeFrame(frame{Data: payload}); err != nil {
		return err
	}

	f, err := c.readFrame()
	if err != nil {
		return fmt.Errorf("ipc: reading handshake result: %w", err)
	}
	if f.Resp == nil || !f.Resp.OK {
		msg := "rejected"
		if f.Resp != nil && f.Resp.Error != "" {
			msg = f.Resp.Error
		}
		return fmt.Errorf("ipc: handshake %s", msg)
	}
	return nil
}

// serverHandshake validates an inbound helper connection.
func serverHandshake(c *conn, wantToken string) (authHello, error) {
	if err := c.nc.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return authHello{}, err
	}
	defer func() { _ = c.nc.SetDeadline(time.Time{}) }()

	f, err := c.readFrame()
	if err != nil {
		return authHello{}, err
	}
	if f.Data == nil {
		return authHello{}, errors.New("ipc: first frame was not a handshake")
	}
	raw, err := json.Marshal(f.Data)
	if err != nil {
		return authHello{}, err
	}
	var hello authHello
	if err := json.Unmarshal(raw, &hello); err != nil {
		return authHello{}, err
	}

	// Constant-time: the token is a secret and this is the one place an
	// attacker gets unlimited guesses at it.
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(wantToken)) != 1 {
		_ = c.writeFrame(frame{Resp: &Response{OK: false, Error: "bad token"}})
		return authHello{}, errors.New("ipc: helper presented an invalid token")
	}

	if err := c.writeFrame(frame{Resp: &Response{OK: true}}); err != nil {
		return authHello{}, err
	}
	return hello, nil
}

// isClosed reports whether err is the ordinary end of a connection rather than
// a fault worth logging.
func isClosed(err error) bool {
	return err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}
