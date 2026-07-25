package ws

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- a minimal client, enough to exercise the server side ---

type testClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dial performs a version-13 handshake against addr and verifies the accept
// key. extra lines (each "Name: value") are appended to the request.
func dial(t *testing.T, addr string, extra ...string) *testClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	const key = "dGhlIHNhbXBsZSBub25jZQ==" // RFC 6455 §1.3 example
	req := "GET /ws HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n"
	for _, e := range extra {
		req += e + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	return &testClient{conn: conn, br: br}
}

// read returns the next frame, asserting the server never masks.
func (c *testClient) read(t *testing.T) (opcode byte, payload []byte) {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	if head[0]&0x80 == 0 {
		t.Fatalf("server sent a fragmented frame (FIN unset)")
	}
	if head[1]&0x80 != 0 {
		t.Fatalf("server masked a frame")
	}
	opcode = head[0] & 0x0F
	n := int64(head[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		io.ReadFull(c.br, ext[:])
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		io.ReadFull(c.br, ext[:])
		n = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return opcode, payload
}

// write sends a properly masked client frame.
func (c *testClient) write(t *testing.T, opcode byte, payload []byte) {
	t.Helper()
	c.writeRaw(t, opcode, payload, true)
}

func (c *testClient) writeRaw(t *testing.T, opcode byte, payload []byte, masked bool) {
	t.Helper()
	frame := []byte{0x80 | opcode}
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, maskBit|byte(n))
	default:
		frame = append(frame, maskBit|126, byte(n>>8), byte(n))
	}
	body := append([]byte(nil), payload...)
	if masked {
		mask := [4]byte{0xA1, 0xB2, 0xC3, 0xD4}
		frame = append(frame, mask[:]...)
		for i := range body {
			body[i] ^= mask[i%4]
		}
	}
	frame = append(frame, body...)
	c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// serve starts an httptest server whose handler upgrades and then runs fn
// with the connection. It returns the listener address and a channel carrying
// the handler's ReadLoop error.
func serve(t *testing.T, fn func(*Conn)) (addr string, upgradeErr <-chan error) {
	t.Helper()
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			errCh <- err
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer conn.Close()
		fn(conn)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), errCh
}

// --- tests ---

func TestAcceptKey(t *testing.T) {
	// RFC 6455 §1.3 worked example.
	if got, want := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Errorf("acceptKey = %q, want %q", got, want)
	}
}

func TestTextFrameSizes(t *testing.T) {
	// The three payload-length encodings: 7-bit, 16-bit, 64-bit.
	payloads := [][]byte{
		[]byte("hi"),
		[]byte(strings.Repeat("a", 200)),
		[]byte(strings.Repeat("b", 70000)),
	}
	done := make(chan struct{})
	addr, _ := serve(t, func(c *Conn) {
		for _, p := range payloads {
			if err := c.WriteText(p); err != nil {
				t.Errorf("WriteText: %v", err)
			}
		}
		<-done
	})
	defer close(done)

	c := dial(t, addr)
	for i, want := range payloads {
		op, got := c.read(t)
		if op != opText {
			t.Fatalf("frame %d opcode = %#x, want text", i, op)
		}
		if string(got) != string(want) {
			t.Fatalf("frame %d payload len = %d, want %d", i, len(got), len(want))
		}
	}
}

func TestReadLoopAnswersPing(t *testing.T) {
	done := make(chan struct{})
	addr, _ := serve(t, func(c *Conn) {
		c.ReadLoop()
		close(done)
	})

	c := dial(t, addr)
	c.write(t, opPing, []byte("beat"))
	op, payload := c.read(t)
	if op != opPong || string(payload) != "beat" {
		t.Fatalf("got opcode %#x payload %q, want pong/beat", op, payload)
	}

	// A close frame ends the loop and is echoed with the same status code.
	var code [2]byte
	binary.BigEndian.PutUint16(code[:], CloseNormal)
	c.write(t, opClose, code[:])
	op, payload = c.read(t)
	if op != opClose {
		t.Fatalf("got opcode %#x, want close", op)
	}
	if len(payload) != 2 || binary.BigEndian.Uint16(payload) != CloseNormal {
		t.Fatalf("close payload = %v, want status %d echoed", payload, CloseNormal)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ReadLoop did not return after the client's close frame")
	}
}

func TestReadLoopRejectsUnmaskedFrame(t *testing.T) {
	errCh := make(chan error, 1)
	addr, _ := serve(t, func(c *Conn) { errCh <- c.ReadLoop() })

	c := dial(t, addr)
	c.writeRaw(t, opText, []byte("unmasked"), false)

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "unmasked") {
			t.Fatalf("ReadLoop error = %v, want an unmasked-frame error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadLoop accepted an unmasked client frame")
	}
}

func TestReadLoopRejectsOversizedFrame(t *testing.T) {
	errCh := make(chan error, 1)
	addr, _ := serve(t, func(c *Conn) { errCh <- c.ReadLoop() })

	c := dial(t, addr)
	c.write(t, opText, make([]byte, maxClientFrame+1))

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("ReadLoop error = %v, want a size error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadLoop accepted an oversized client frame")
	}
}

func TestWriteAfterPeerCloseFails(t *testing.T) {
	// A write to a dead peer must surface an error so the pusher stops.
	writeErr := make(chan error, 1)
	addr, _ := serve(t, func(c *Conn) {
		var err error
		for i := 0; i < 200 && err == nil; i++ {
			err = c.WriteText([]byte(fmt.Sprintf(`{"n":%d}`, i)))
			time.Sleep(5 * time.Millisecond)
		}
		writeErr <- err
	})

	c := dial(t, addr)
	c.read(t) // one frame, then vanish
	c.conn.Close()

	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("writes to a closed peer never failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not notice the closed peer")
	}
}

func TestUpgradeRejectsBadHandshake(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    error
	}{
		{"plain GET", http.Header{}, ErrNotWebSocket},
		{"upgrade without connection", http.Header{"Upgrade": {"websocket"}}, ErrNotWebSocket},
		{"wrong version", http.Header{
			"Upgrade": {"websocket"}, "Connection": {"Upgrade"},
			"Sec-Websocket-Version": {"8"}, "Sec-Websocket-Key": {"x"},
		}, nil},
		{"missing key", http.Header{
			"Upgrade": {"websocket"}, "Connection": {"Upgrade"},
			"Sec-Websocket-Version": {"13"},
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/ws", nil)
			r.Header = tc.headers
			w := httptest.NewRecorder()
			conn, err := Upgrade(w, r)
			if conn != nil {
				t.Fatal("Upgrade returned a connection for a bad handshake")
			}
			if err == nil {
				t.Fatal("Upgrade accepted a bad handshake")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			// Nothing may be written: the caller still owes an HTTP response.
			if w.Body.Len() != 0 {
				t.Fatalf("Upgrade wrote %q to the response", w.Body.String())
			}
		})
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		origin     string
		forwarded  string
		wantAllows bool
	}{
		{"no origin (curl)", "builds.example.com", "", "", true},
		{"matching origin", "builds.example.com", "https://builds.example.com", "", true},
		{"port must match", "builds.example.com:8443", "https://builds.example.com", "", false},
		{"foreign origin", "builds.example.com", "https://evil.example", "", false},
		{"proxy rewrote host", "127.0.0.1:8082", "https://builds.example.com", "builds.example.com", true},
		{"garbage origin", "builds.example.com", "://", "", false},
		{"null origin", "builds.example.com", "null", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/live", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Host", tc.forwarded)
			}
			if got := SameOrigin(r); got != tc.wantAllows {
				t.Errorf("SameOrigin = %v, want %v", got, tc.wantAllows)
			}
		})
	}
}

func TestIsUpgrade(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/live", nil)
	r.Header.Set("Connection", "keep-alive, Upgrade")
	r.Header.Set("Upgrade", "WebSocket")
	if !IsUpgrade(r) {
		t.Error("IsUpgrade = false for a token-list Connection header")
	}
	post := httptest.NewRequest("POST", "/api/live", nil)
	post.Header = r.Header
	if IsUpgrade(post) {
		t.Error("IsUpgrade = true for a POST")
	}
}
