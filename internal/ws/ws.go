// Package ws is a minimal RFC 6455 WebSocket server for one-way JSON pushes.
//
// The scope is deliberately narrow, and that is what makes a hand-rolled
// implementation reasonable here instead of a dependency: the server sends
// unfragmented text frames, the client sends nothing but control frames.
// There is no extension negotiation, no compression, no write fragmentation,
// and inbound data frames are read and discarded. That covers the dashboard
// feed (internal/live); anything richer should reach for a real library.
//
// Server frames are never masked, client frames must be — both are protocol
// requirements, not choices.
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// handshakeGUID is the RFC 6455 constant mixed into Sec-WebSocket-Accept.
const handshakeGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// CloseNormal is RFC 6455's 1000 status: the connection completed normally.
const CloseNormal = 1000

const (
	// maxClientFrame caps an inbound payload. Clients of this endpoint have
	// nothing to say beyond control frames, so anything larger is abuse
	// rather than a big message.
	maxClientFrame = 4096
	writeTimeout   = 10 * time.Second
	// DefaultIdleTimeout bounds how long a connection may go without any
	// inbound frame. Browsers answer server pings automatically, so a peer
	// that has gone away (a laptop lid, a dropped NAT mapping) is detected
	// within this window instead of leaking a goroutine forever.
	DefaultIdleTimeout = 90 * time.Second
)

// ErrNotWebSocket means the request was not a version-13 upgrade handshake.
var ErrNotWebSocket = errors.New("ws: not a websocket handshake")

// Conn is an upgraded connection. Writes are serialized internally, so the
// read loop's pong replies can race application writes safely.
type Conn struct {
	// IdleTimeout is the read deadline refreshed on every inbound frame.
	IdleTimeout time.Duration

	conn      net.Conn
	br        *bufio.Reader
	wmu       sync.Mutex
	closeOnce sync.Once
}

// IsUpgrade reports whether r is a WebSocket upgrade request. Both headers
// are token lists compared case-insensitively (Firefox sends
// "Connection: keep-alive, Upgrade").
func IsUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

// SameOrigin reports whether a browser-initiated handshake came from this
// server's own origin. WebSocket has no CORS: without this check any page on
// the internet could open a socket to the dashboard with the operator's
// cookies attached. A missing Origin means a non-browser client (curl, a
// script) and is allowed — those cannot be tricked into carrying someone
// else's session.
//
// Behind a reverse proxy the Host header may be rewritten to the upstream
// address, so X-Forwarded-Host is accepted too; a proxy that forwards neither
// makes every browser fail this check, which is why the UI falls back to
// polling rather than going dark.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, host := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
		if host != "" && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return false
}

// Upgrade completes the handshake and hijacks the connection. The caller owns
// the returned Conn and must Close it. Nothing is written to w on error, so
// the caller is free to send an ordinary HTTP response instead.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !IsUpgrade(r) {
		return nil, ErrNotWebSocket
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, fmt.Errorf("ws: unsupported version %q", r.Header.Get("Sec-WebSocket-Version"))
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("ws: missing Sec-WebSocket-Key")
	}

	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("ws: hijack failed: %w", err)
	}

	// Past this point the response is ours to write byte for byte.
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws: handshake write failed: %w", err)
	}
	if err := brw.Writer.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws: handshake flush failed: %w", err)
	}
	// Read through the hijacked bufio.Reader, never the raw conn: it may
	// already hold bytes the client pipelined behind the handshake.
	return &Conn{IdleTimeout: DefaultIdleTimeout, conn: conn, br: brw.Reader}, nil
}

// acceptKey computes the Sec-WebSocket-Accept response value.
func acceptKey(clientKey string) string {
	h := sha1.New()
	io.WriteString(h, clientKey+handshakeGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// WriteText sends one unfragmented text frame.
func (c *Conn) WriteText(payload []byte) error { return c.writeFrame(opText, payload) }

// Ping sends an empty ping frame. A conforming client answers with a pong,
// which refreshes the read deadline in ReadLoop.
func (c *Conn) Ping() error { return c.writeFrame(opPing, nil) }

// ReadLoop consumes client frames until the peer closes or errors, answering
// pings and discarding data frames. Callers must run it for the lifetime of
// the connection: it is what enforces the idle timeout and observes the
// client's close frame. It returns nil on a clean close.
func (c *Conn) ReadLoop() error {
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return err
		}
		switch opcode {
		case opClose:
			// Echo the peer's status code back, per RFC 6455 §5.5.1.
			echo := []byte{}
			if len(payload) >= 2 {
				echo = payload[:2]
			}
			c.writeFrame(opClose, echo)
			return nil
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return err
			}
		case opPong, opText, opBinary, opContinuation:
			// Nothing here reads from clients; drop it.
		default:
			return fmt.Errorf("ws: unknown opcode %#x", opcode)
		}
	}
}

// Close sends a normal close frame (best effort) and closes the socket. Safe
// to call more than once and from any goroutine; a blocked ReadLoop unblocks
// with an error.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		var code [2]byte
		binary.BigEndian.PutUint16(code[:], CloseNormal)
		c.writeFrame(opClose, code[:])
		err = c.conn.Close()
	})
	return err
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	// One buffer, one Write: a header sent in its own segment would be a
	// wasted round trip on every push.
	frame := make([]byte, 0, len(payload)+10)
	frame = append(frame, 0x80|opcode) // FIN, no RSV bits
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, payload...)

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err := c.conn.Write(frame)
	return err
}

func (c *Conn) readFrame() (opcode byte, payload []byte, err error) {
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	if err = c.conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
		return 0, nil, err
	}

	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, err
	}
	if head[0]&0x70 != 0 {
		return 0, nil, errors.New("ws: reserved bits set")
	}
	fin := head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	if head[1]&0x80 == 0 {
		// RFC 6455 §5.1: client-to-server frames are always masked.
		return 0, nil, errors.New("ws: unmasked client frame")
	}

	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		n := binary.BigEndian.Uint64(ext[:])
		if n > uint64(maxClientFrame) {
			return 0, nil, errors.New("ws: client frame too large")
		}
		length = int64(n)
	}
	if opcode >= opClose && (!fin || length > 125) {
		return 0, nil, errors.New("ws: invalid control frame")
	}
	if length > maxClientFrame {
		return 0, nil, errors.New("ws: client frame too large")
	}

	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return 0, nil, err
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload, nil
}

// headerHasToken reports whether a comma-separated header contains a token,
// case-insensitively.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
