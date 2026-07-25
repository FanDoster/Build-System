package api

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/live"
	"github.com/FanDoster/Build-System/internal/models"
)

// liveServer wires a real HTTP server (httptest.NewServer, not a recorder —
// the endpoint hijacks its connection) around the live hub.
func liveServer(t *testing.T) (*Server, *http.ServeMux, string) {
	t.Helper()
	s, mux := newTestServer(t)
	s.Runner = &fakeCanceler{step: "build"}
	s.Live = live.New(s.DB, s.Runner)
	s.Live.Tick = 10 * time.Millisecond
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, mux, strings.TrimPrefix(srv.URL, "http://")
}

// wsClient is the handful of RFC 6455 client bits these tests need.
type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialLive(t *testing.T, addr string, headers ...string) (*wsClient, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	req := "GET /api/live HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"
	for _, h := range headers {
		req += h + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return &wsClient{conn: conn, br: br}, resp
}

// nextMessage returns the next text frame's payload, skipping pings.
func (c *wsClient) nextMessage(t *testing.T) live.Message {
	t.Helper()
	for {
		c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var head [2]byte
		if _, err := io.ReadFull(c.br, head[:]); err != nil {
			t.Fatalf("read frame: %v", err)
		}
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
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			t.Fatalf("read payload: %v", err)
		}
		if head[0]&0x0F != 0x1 { // not a text frame
			continue
		}
		var msg live.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Fatalf("unmarshal %q: %v", payload, err)
		}
		return msg
	}
}

func TestLiveSocketPushesNewBuilds(t *testing.T) {
	s, mux, addr := liveServer(t)
	project := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	client, resp := dialLive(t, addr)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	// A dashboard opened with nothing running still gets its first frame...
	if msg := client.nextMessage(t); len(msg.Builds) != 0 {
		t.Fatalf("first frame = %+v, want no builds", msg.Builds)
	}

	// ...and learns about a build triggered afterwards, unprompted.
	rec := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", project.ID), nil)
	if rec.Code != 201 {
		t.Fatalf("trigger build: %d %s", rec.Code, rec.Body.String())
	}
	msg := client.nextMessage(t)
	if len(msg.Builds) != 1 {
		t.Fatalf("frame after trigger = %+v, want one build", msg.Builds)
	}
	b := msg.Builds[0]
	if b.Status != models.StatusPending || b.ProjectName != project.Name {
		t.Errorf("pushed build = %+v, want a pending build for %s", b, project.Name)
	}
	if b.QueuePosition != 1 {
		t.Errorf("queue_position = %d, want 1", b.QueuePosition)
	}

	// Status transitions arrive too.
	if ok, err := s.DB.ClaimBuild(b.ID); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	msg = client.nextMessage(t)
	if msg.Builds[0].Status != models.StatusRunning {
		t.Fatalf("status = %q, want running", msg.Builds[0].Status)
	}
	if msg.Builds[0].CurrentStep != "build" {
		t.Errorf("current_step = %q, want the runner's step", msg.Builds[0].CurrentStep)
	}
	if msg.Builds[0].Log != "" {
		t.Errorf("feed carried log bytes: %q", msg.Builds[0].Log)
	}
}

func TestLivePlainGetReturnsSameSnapshot(t *testing.T) {
	// The polling fallback and `curl /api/live` must see the socket's payload.
	s, _, addr := liveServer(t)
	project := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})
	build := &models.Build{ProjectID: project.ID, Status: models.StatusPending, CommitSHA: "abc", CommitMessage: "m"}
	if err := s.DB.CreateBuild(build); err != nil {
		t.Fatalf("create build: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/api/live")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var msg live.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != "builds" || len(msg.Builds) != 1 || msg.Builds[0].ID != build.ID {
		t.Fatalf("snapshot = %+v, want the pending build", msg)
	}
}

func TestLiveRejectsForeignOrigin(t *testing.T) {
	_, _, addr := liveServer(t)
	_, resp := dialLive(t, addr, "Origin: https://evil.example")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-origin handshake", resp.StatusCode)
	}
}

func TestLiveAcceptsSameOrigin(t *testing.T) {
	_, _, addr := liveServer(t)
	_, resp := dialLive(t, addr, "Origin: http://"+addr)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101 for a same-origin handshake", resp.StatusCode)
	}
}

func TestLiveWithoutHubIsUnavailable(t *testing.T) {
	s, mux := newTestServer(t)
	s.Live = nil
	rec := doJSON(t, mux, "GET", "/api/live", nil)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 when no hub is configured", rec.Code)
	}
}
