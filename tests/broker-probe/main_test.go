package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeBroker(t *testing.T, respond func(string) string) (string, <-chan string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "broker.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	requests := make(chan string, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				data, e := io.ReadAll(io.LimitReader(c, 4097))
				if e != nil {
					return
				}
				requests <- string(data)
				_, _ = io.WriteString(c, respond(string(data)))
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return socket, requests
}
func denialWire(d denial) string {
	wire := d.request + "\n"
	if d.emptyTar {
		wire += strings.Repeat("\x00", 1024)
	}
	return wire
}
func goodResponse(request string) string {
	if request == "{\"op\":\"ping\"}\n" {
		return "{\"output\":\"\"}\n{\"output\":\"pong\\n\",\"exit_code\":0}\n"
	}
	for _, d := range denials {
		if request == denialWire(d) {
			return fmt.Sprintf("{\"output\":%q,\"exit_code\":1}\n", d.output)
		}
	}
	return "{\"output\":\"unexpected\",\"exit_code\":1}\n"
}
func TestRawWireAndAllDenials(t *testing.T) {
	socket, requests := fakeBroker(t, goodResponse)
	r := report{Pass: true}
	brokerChecks(context.Background(), &r, socket)
	if !reportValid(r) || len(r.Checks) != len(denials)+1 {
		t.Fatalf("unexpected report: %+v", r)
	}
	// The fake waits for EOF before responding, so success proves CloseWrite,
	// exactly one newline header, with 1024 zero bytes only for build negatives.
	expected := []string{"{\"op\":\"ping\"}\n"}
	for _, d := range denials {
		expected = append(expected, denialWire(d))
	}
	for _, want := range expected {
		select {
		case got := <-requests:
			if got != want {
				t.Fatalf("wire: %q != %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing request")
		}
	}
}
func TestDenialMustNotBeBusySuccessOrOtherFailure(t *testing.T) {
	for name, response := range map[string]string{
		"busy":            "{\"output\":\"broker busy: another operation is executing\\n\",\"exit_code\":1}\n",
		"success":         "{\"output\":\"request not allowed\\n\",\"exit_code\":0}\n",
		"arbitrary_error": "{\"output\":\"other error\\n\",\"exit_code\":1}\n",
		"wrong_code":      "{\"output\":\"request not allowed\\n\",\"exit_code\":2}\n",
		"wrong_denial":    "{\"output\":\"invalid request\\n\",\"exit_code\":1}\n",
		"truncated":       "{\"output\":\"request not allowed\\n\",\"exit_code\":1}",
		"malformed":       "no json\n",
	} {
		t.Run(name, func(t *testing.T) {
			socket, _ := fakeBroker(t, func(req string) string {
				if req == "{\"op\":\"ping\"}\n" {
					return goodResponse(req)
				}
				return response
			})
			r := report{Pass: true}
			brokerChecks(context.Background(), &r, socket)
			if r.Pass {
				t.Fatal("false green")
			}
			for _, c := range r.Checks {
				if name != "wrong_denial" && strings.HasPrefix(c.Name, "server_denies_") && c.Pass {
					t.Fatalf("incorrect pass: %+v", c)
				}
			}
		})
	}
}
func TestReadReplyStrictAndBounded(t *testing.T) {
	cases := map[string]string{
		"empty": "", "missing_exit": "{\"output\":\"\"}\n", "truncated": "{\"output\":\"\",\"exit_code\":0}",
		"truncated_json": "{\"output\":", "malformed": "bad\n", "unknown": "{\"output\":\"\",\"exit_code\":0,\"extra\":1}\n",
		"duplicate": "{\"output\":\"\",\"exit_code\":0,\"exit_code\":1}\n",
		"null_exit": "{\"output\":\"\",\"exit_code\":null}\n", "null_output": "{\"output\":null,\"exit_code\":0}\n",
		"missing_output": "{\"exit_code\":0}\n", "wrong_type": "{\"output\":5,\"exit_code\":0}\n",
		"trailing_json": "{\"output\":\"\",\"exit_code\":0}{}\n", "trailing_frames": "{\"output\":\"\",\"exit_code\":0}\n{\"output\":\"\"}\n",
		"null": "null\n", "array": "[]\n", "oversized": strings.Repeat(" ", maxResponseBytes+1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := readReply(strings.NewReader(input)); e == nil {
				t.Fatal("accepted bad frame")
			}
		})
	}
	r, e := readReply(strings.NewReader("{\"output\":\"\"}\n{\"output\":\"po\"}\n{\"output\":\"ng\\n\",\"exit_code\":0}\n"))
	if e != nil || r.code != 0 || r.output != "pong\n" {
		t.Fatalf("good response failed: %+v %v", r, e)
	}
}
func TestRawRequestCancellationAndNoExit(t *testing.T) {
	socket, _ := fakeBroker(t, func(string) string { return "{\"output\":\"\"}\n" })
	if _, e := rawRequest(context.Background(), socket, `{"op":"ping"}`, false); e == nil {
		t.Fatal("missing exit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := rawRequest(ctx, socket, `{"op":"ping"}`, false); e == nil {
		t.Fatal("cancellation ignored")
	}
	// A broker which reads the half-close then never finishes must be bounded
	// by the caller's deadline, including when the last frame has exit_code.
	stalledSocket := filepath.Join(t.TempDir(), "stall.sock")
	l, e := net.Listen("unix", stalledSocket)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	finishedRelease := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
		_, _ = io.WriteString(c, "{\"output\":\"pong\\n\",\"exit_code\":0}\n")
		var b [1]byte
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = c.Read(b[:]) // half-closed reads EOF; keep open until client deadline below.
		<-finishedRelease
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, e = rawRequest(ctx, stalledSocket, `{"op":"ping"}`, false)
	close(finishedRelease)
	<-finished
	if e == nil || time.Since(start) > time.Second {
		t.Fatalf("response deadline failed: %v", e)
	}
}

type fakeInfo struct{ mode os.FileMode }

func (f fakeInfo) Name() string       { return "fixture" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }
func cleanReaders() (systemReaders, map[string][]byte, map[string]os.FileInfo) {
	files := map[string][]byte{
		"/proc/self/status":        []byte("Uid:\t10001\t10001\t10001\t10001\nGroups:\t10001\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapBnd:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\n"),
		"/proc/mounts":             []byte("overlay / overlay ro,relatime 0 0\nproc /proc proc rw,nosuid,nodev,noexec 0 0\nsysfs /sys sysfs ro 0 0\ntmpfs /dev tmpfs rw 0 0\n/dev/sda /run/builds-broker ext4 ro,relatime 0 0\n/dev/sda /etc/hosts ext4 rw 0 0\n"),
		"/context/included.marker": []byte(includedMarker),
	}
	stats := map[string]os.FileInfo{"/run/builds-broker": fakeInfo{os.ModeDir | 0750}, socketPath: fakeInfo{os.ModeSocket | 0660}}
	return systemReaders{func(p string) ([]byte, error) {
		v, ok := files[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return v, nil
	}, func(p string) (os.FileInfo, error) {
		v, ok := stats[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return v, nil
	}, func() (int, int) { return 10001, 10001 }, func() ([]net.Interface, error) { return []net.Interface{{Name: "lo", Flags: net.FlagLoopback}}, nil }}, files, stats
}
func TestFilesystemAndNetworkChecks(t *testing.T) {
	s, _, _ := cleanReaders()
	r := report{Pass: true}
	filesystemChecks(&r, s)
	if !reportValid(r) {
		t.Fatalf("clean fixture failed: %+v", r)
	}
	changes := map[string]func(*systemReaders, map[string][]byte, map[string]os.FileInfo){
		"wrong_uid": func(s *systemReaders, _ map[string][]byte, _ map[string]os.FileInfo) {
			s.ids = func() (int, int) { return 0, 10001 }
		},
		"wrong_gid": func(s *systemReaders, _ map[string][]byte, _ map[string]os.FileInfo) {
			s.ids = func() (int, int) { return 10001, 0 }
		},
		"capabilities": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/self/status"] = []byte("CapEff:\t0001\nNoNewPrivs:\t1\n")
		},
		"new_privileges": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/self/status"] = []byte("CapEff:\t0\nNoNewPrivs:\t0\n")
		},
		"writable_root_even_if_unwritable_uid": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/mounts"] = bytes.Replace(f["/proc/mounts"], []byte("overlay ro,"), []byte("overlay rw,"), 1)
		},
		"writable_broker": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/mounts"] = bytes.Replace(f["/proc/mounts"], []byte("ext4 ro,"), []byte("ext4 rw,"), 1)
		},
		"no_mounts": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) { delete(f, "/proc/mounts") },
		"invalid_mounts": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/mounts"] = []byte("bad")
		},
		"extra_mount": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/mounts"] = append(f["/proc/mounts"], []byte("/host/key /secrets/key ext4 ro 0 0\n")...)
		},
		"ssh_mount": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/proc/mounts"] = append(f["/proc/mounts"], []byte("/host/.ssh /root/.ssh ext4 ro 0 0\n")...)
		},
		"missing_socket": func(_ *systemReaders, _ map[string][]byte, s map[string]os.FileInfo) { delete(s, socketPath) },
		"socket_symlink": func(_ *systemReaders, _ map[string][]byte, s map[string]os.FileInfo) {
			s[socketPath] = fakeInfo{os.ModeSymlink | 0777}
		},
		"directory_symlink": func(_ *systemReaders, _ map[string][]byte, s map[string]os.FileInfo) {
			s["/run/builds-broker"] = fakeInfo{os.ModeSymlink | 0777}
		},
		"daemon_path": func(_ *systemReaders, _ map[string][]byte, s map[string]os.FileInfo) {
			s["/var/run/docker.sock"] = fakeInfo{os.ModeSocket}
		},
		"secret_config_path": func(_ *systemReaders, _ map[string][]byte, s map[string]os.FileInfo) {
			s["/etc/builds-broker"] = fakeInfo{os.ModeDir}
		},
		"marker_wrong": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			f["/context/included.marker"] = []byte("wrong")
		},
		"marker_missing": func(_ *systemReaders, f map[string][]byte, _ map[string]os.FileInfo) {
			delete(f, "/context/included.marker")
		},
		"ignored_marker_present": func(_ *systemReaders, _ map[string][]byte, s map[string]os.FileInfo) {
			s["/context/ignored.marker"] = fakeInfo{0444}
		},
		"network_eth0": func(s *systemReaders, _ map[string][]byte, _ map[string]os.FileInfo) {
			s.interfaces = func() ([]net.Interface, error) {
				return []net.Interface{{Name: "lo", Flags: net.FlagLoopback}, {Name: "eth0"}}, nil
			}
		},
		"network_error": func(s *systemReaders, _ map[string][]byte, _ map[string]os.FileInfo) {
			s.interfaces = func() ([]net.Interface, error) { return nil, os.ErrPermission }
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, files, stats := cleanReaders()
			change(&s, files, stats)
			r := report{Pass: true}
			filesystemChecks(&r, s)
			if reportValid(r) {
				t.Fatal("false green")
			}
		})
	}
}
func TestFrozenReportHealthAndLifetime(t *testing.T) {
	for _, pass := range []bool{true, false} {
		t.Run(fmt.Sprint(pass), func(t *testing.T) {
			listener, e := net.Listen("tcp4", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			var calls atomic.Int32
			var output bytes.Buffer
			done := make(chan int, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				done <- serve(ctx, listener, 300*time.Millisecond, func(context.Context) report {
					calls.Add(1)
					return report{Pass: pass, Checks: []check{{"startup_fixed", pass, "fixture result"}}}
				}, &output)
			}()
			for i := 0; i < 3; i++ {
				var health bytes.Buffer
				code := healthcheck("http://"+listener.Addr().String()+"/health", &health)
				if (code == 0) != pass {
					t.Fatalf("health: code %d %s", code, health.String())
				}
				if !strings.Contains(health.String(), "startup_fixed") {
					t.Fatal("missing clear check labels")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("checks rerun %d times", calls.Load())
			}
			select {
			case code := <-done:
				if (code == 0) != pass {
					t.Fatalf("lifetime exit %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lifetime not bounded")
			}
			if calls.Load() != 1 {
				t.Fatal("lifetime reran checks")
			}
		})
	}
}
func TestSignalContextStopsAndPreservesFailure(t *testing.T) {
	for _, pass := range []bool{true, false} {
		listener, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() {
			done <- serve(ctx, listener, maxLifetime, func(context.Context) report { return report{Pass: pass, Checks: []check{{"fixed", pass, "fixed"}}} }, io.Discard)
		}()
		// Cancellation must fail even after the frozen report was served healthy.
		code := healthcheck("http://"+listener.Addr().String()+"/health", io.Discard)
		if (code == 0) != pass {
			t.Fatal("wrong report before cancellation")
		}
		cancel()
		select {
		case code := <-done:
			if code != 1 {
				t.Fatal("signal cancellation must never be success")
			}
		case <-time.After(time.Second):
			t.Fatal("signal ignored")
		}
	}
}
func TestFrozenHandlerCopiesReport(t *testing.T) {
	r := report{Pass: true, Checks: []check{{"fixed", true, "fixed"}}}
	handler := frozenHandler(r)
	r.Checks[0].Pass = false
	r.Pass = false
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/health", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"pass":true`) {
		t.Fatal("report not frozen")
	}
	for _, path := range []string{"/", "/other"} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 404 {
			t.Fatal("unexpected endpoint")
		}
	}
}
func TestHealthNeverGreensOnHTTPStartAlone(t *testing.T) {
	bodies := []string{"", `{}`, `{"pass":true,"checks":[]}`, `{"pass":true,"checks":[{"name":"bad","pass":false,"detail":"failure"}]}`, `{"pass":true,"checks":[{"pass":true}]} garbage`, strings.Repeat("a", maxResponseBytes+1)}
	for _, body := range bodies {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		var out bytes.Buffer
		if healthcheck(server.URL, &out) == 0 {
			t.Fatalf("false green: %q", body[:min(len(body), 100)])
		}
		if !strings.Contains(out.String(), "FAIL") {
			t.Fatal("no failure label")
		}
		server.Close()
	}
	// A valid report with HTTP 503 is still unhealthy.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"pass":true,"checks":[{"name":"fixed","pass":true,"detail":"fixed"}]}`)
	}))
	defer server.Close()
	if healthcheck(server.URL, io.Discard) == 0 {
		t.Fatal("503 accepted")
	}
}
func TestSafeFixedInputs(t *testing.T) {
	for _, d := range denials {
		if strings.Contains(d.request, "/opt/docker/food/") || strings.Contains(d.request, `"service":"db"`) {
			t.Fatal("live production database target")
		}
		if strings.Contains(d.request, `"op":"build"`) && !strings.Contains(d.request, "registry.fandoster.com/broker-probe-denied:latest") {
			t.Fatal("potentially allowed build tag")
		}
		if strings.Contains(d.request, `"op":"push"`) {
			t.Fatal("live push is forbidden")
		}
		if d.emptyTar != strings.Contains(d.request, `"op":"build"`) {
			t.Fatal("build requests must include exactly an empty tar")
		}
		if strings.Contains(d.request, `"op":"deploy"`) && !strings.Contains(d.request, "probe-") {
			t.Fatal("non-probe deploy")
		}
	}
}

func TestEveryCapabilitySetAndSupplementaryGroups(t *testing.T) {
	for _, name := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		for _, value := range []string{"0000000000000001", "malformed", "MISSING"} {
			t.Run(name+"_"+value, func(t *testing.T) {
				s, f, _ := cleanReaders()
				old := name + ":\t0000000000000000\n"
				replacement := name + ":\t" + value + "\n"
				if value == "MISSING" {
					replacement = ""
				}
				f["/proc/self/status"] = bytes.Replace(f["/proc/self/status"], []byte(old), []byte(replacement), 1)
				r := report{Pass: true}
				filesystemChecks(&r, s)
				if reportValid(r) {
					t.Fatal("nonzero/malformed/missing capability set accepted")
				}
			})
		}
	}
	for _, groups := range []string{"0", "10001 0", "10002", "10001 10002", "invalid", "MISSING", "", "10001"} {
		t.Run("groups_"+groups, func(t *testing.T) {
			s, f, _ := cleanReaders()
			replacement := "Groups:\t" + groups + "\n"
			if groups == "MISSING" {
				replacement = ""
			}
			f["/proc/self/status"] = bytes.Replace(f["/proc/self/status"], []byte("Groups:\t10001\n"), []byte(replacement), 1)
			r := report{Pass: true}
			filesystemChecks(&r, s)
			want := groups == "" || groups == "10001"
			if reportValid(r) != want {
				t.Fatalf("groups accepted=%t want=%t", reportValid(r), want)
			}
		})
	}
}

func TestEarlyHeaderRejectionBeforeArchiveOrHalfClose(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "early.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				// Like the real broker, read only a header, deny it immediately and close.
				// Deliberately do not wait for archive bytes or the client's CloseWrite.
				header, err := bufio.NewReaderSize(conn, 4096).ReadString('\n')
				if err != nil {
					return
				}
				for _, d := range denials {
					if d.emptyTar && header == d.request+"\n" {
						_, _ = io.WriteString(conn, goodResponse(denialWire(d)))
						return
					}
				}
			}()
		}
	}()
	defer func() { listener.Close(); <-done }()
	for i := 0; i < 64; i++ {
		d := denials[2+i%2]
		response, err := rawRequest(context.Background(), socket, d.request, true)
		if err != nil || response.code != 1 || response.output != d.output {
			t.Fatalf("early rejection %d: %+v %v", i, response, err)
		}
	}
}
