// broker-probe is a fixed-input, non-mutating broker/container verification fixture.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	socketPath       = "/run/builds-broker/docker.sock"
	healthAddress    = "127.0.0.1:8080"
	maxLifetime      = 5 * time.Minute
	requestTimeout   = 2 * time.Second
	maxResponseBytes = 64 * 1024
	includedMarker   = "broker-probe included marker v1\n"
)

type check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}
type report struct {
	Pass   bool    `json:"pass"`
	Checks []check `json:"checks"`
}

func (r *report) add(name string, pass bool, detail string) {
	r.Checks = append(r.Checks, check{name, pass, detail})
	if !pass {
		r.Pass = false
	}
}

type systemReaders struct {
	readFile   func(string) ([]byte, error)
	stat       func(string) (os.FileInfo, error)
	ids        func() (int, int)
	interfaces func() ([]net.Interface, error)
}

func boundedFile(name string) ([]byte, error) {
	f, e := os.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if len(b) > 1024*1024 {
		return nil, errors.New("file exceeds limit")
	}
	return b, e
}
func realReaders() systemReaders {
	return systemReaders{boundedFile, os.Lstat, func() (int, int) {
		if os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
			return -1, -1
		}
		return os.Getuid(), os.Getgid()
	}, net.Interfaces}
}

type mount struct{ target, kind, options string }

func mountPath(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
func parseMounts(data []byte) ([]mount, error) {
	var result []mount
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(line)
		if len(f) != 6 {
			return nil, errors.New("invalid mounts")
		}
		result = append(result, mount{mountPath(f[1]), f[2], f[3]})
	}
	return result, nil
}
func option(s, want string) bool {
	for _, v := range strings.Split(s, ",") {
		if v == want {
			return true
		}
	}
	return false
}
func approvedMount(m mount) bool {
	switch m.target {
	case "/", "/run/builds-broker":
		return true
	case "/etc/hostname", "/etc/hosts", "/etc/resolv.conf":
		return true // Docker-managed files, not user-supplied binds.
	case "/proc", "/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger":
		return m.kind == "proc"
	case "/dev":
		return m.kind == "tmpfs"
	case "/dev/pts":
		return m.kind == "devpts"
	case "/dev/mqueue":
		return m.kind == "mqueue"
	case "/dev/shm":
		return m.kind == "tmpfs"
	case "/sys":
		return m.kind == "sysfs"
	case "/sys/fs/cgroup":
		return m.kind == "cgroup2" || m.kind == "cgroup"
	// Docker's standard masked kernel paths.
	case "/proc/acpi", "/proc/scsi", "/proc/asound", "/sys/firmware", "/sys/devices/virtual/powercap":
		return m.kind == "tmpfs" && option(m.options, "ro")
	case "/proc/interrupts", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug":
		return m.kind == "tmpfs"
	}
	return false
}
func filesystemChecks(r *report, s systemReaders) {
	uid, gid := s.ids()
	r.add("uid_gid_10001", uid == 10001 && gid == 10001, "UID and GID must both be 10001")
	b, e := s.readFile("/proc/self/status")
	capNames := []string{"CapInh:", "CapPrm:", "CapEff:", "CapBnd:", "CapAmb:"}
	caps := map[string]bool{}
	nnp, groupsPresent, groupsSafe := false, false, true
	if e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			if f[0] == "Groups:" {
				groupsPresent = true
				for _, group := range f[1:] {
					if group != "10001" {
						groupsSafe = false
					}
				}
			}
			if len(f) != 2 {
				continue
			}
			for _, name := range capNames {
				if f[0] == name {
					v, err := strconv.ParseUint(f[1], 16, 64)
					caps[name] = err == nil && v == 0
				}
			}
			if f[0] == "NoNewPrivs:" {
				nnp = f[1] == "1"
			}
		}
	}
	allCapsZero := true
	for _, name := range capNames {
		if !caps[name] {
			allCapsZero = false
		}
	}
	r.add("zero_all_capability_sets", allCapsZero, "CapInh, CapPrm, CapEff, CapBnd and CapAmb must all be zero")
	r.add("no_extra_supplementary_groups", groupsPresent && groupsSafe, "supplementary groups must be absent or only 10001; root is forbidden")
	r.add("no_new_privileges", nnp, "NoNewPrivs must be 1")
	b, e = s.readFile("/proc/mounts")
	mounts, parseErr := parseMounts(b)
	rootRO, brokerRO, approved := false, false, e == nil && parseErr == nil
	for _, m := range mounts {
		if m.target == "/" {
			rootRO = option(m.options, "ro") && !option(m.options, "rw")
		}
		if m.target == "/run/builds-broker" {
			brokerRO = option(m.options, "ro") && !option(m.options, "rw")
		}
		if !approvedMount(m) {
			approved = false
		}
	}
	r.add("root_mount_read_only", e == nil && parseErr == nil && rootRO, "root must be mounted ro in /proc/mounts; access denied is insufficient")
	r.add("broker_directory_read_only", e == nil && parseErr == nil && brokerRO, "exact /run/builds-broker directory mount must be ro")
	r.add("no_extra_mounts", approved, "only broker directory and standard container system mounts permitted")
	info, e := s.stat("/run/builds-broker")
	r.add("broker_directory_present", e == nil && info.IsDir(), "broker directory must exist without a symlink")
	info, e = s.stat(socketPath)
	r.add("broker_socket_present", e == nil && info.Mode()&os.ModeSocket != 0, "broker Unix socket must exist without a symlink")
	forbidden := []string{"/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock", "/run/podman/podman.sock", "/root/.ssh", "/home/builds/.ssh", "/root/.docker", "/etc/builds-broker", "/run/builds-ssh", "/run/builds-notify", "/opt/docker", "/root/.aws", "/root/.config", "/root/.kube"}
	absent := true
	for _, p := range forbidden {
		_, err := s.stat(p)
		if !os.IsNotExist(err) {
			absent = false
		}
	}
	r.add("no_host_secrets_or_daemon_paths", absent, "daemon, SSH, key, provider and host configuration paths must be absent")
	b, e = s.readFile("/context/included.marker")
	r.add("included_marker_exact", e == nil && string(b) == includedMarker, "positive context marker must match exactly")
	_, e = s.stat("/context/ignored.marker")
	r.add("dockerfile_specific_ignore", os.IsNotExist(e), "ignored context marker must not exist")
	interfaces, e := s.interfaces()
	onlyLO := e == nil && len(interfaces) == 1 && interfaces[0].Name == "lo" && interfaces[0].Flags&net.FlagLoopback != 0
	r.add("loopback_network_only", onlyLO, "only the loopback network interface is permitted")
}

type denial struct {
	name, request, output string
	emptyTar              bool
}

// Every input is permanently invalid or a read-only ping. No valid build, push,
// deploy, version, arbitrary args, configuration, or external network is used.
var denials = []denial{
	{"unknown_operation", `{"op":"broker-probe-unknown"}`, "invalid request\n", false},
	{"run_operation", `{"op":"run"}`, "invalid request\n", false},
	{"unallowlisted_tag", `{"op":"build","tag":"registry.fandoster.com/broker-probe-denied:latest","dockerfile":"Dockerfile"}`, "request not allowed\n", true},
	{"dockerfile_traversal", `{"op":"build","tag":"registry.fandoster.com/broker-probe-denied:latest","dockerfile":"../Dockerfile"}`, "request not allowed\n", true},
	{"unconfigured_probe_service", `{"op":"deploy","compose":"/opt/docker/todo/docker-compose.yml","service":"probe-denied-no-such-service"}`, "request not allowed\n", false},
	{"unknown_compose_path", `{"op":"deploy","compose":"/broker-probe-never/compose.yml","service":"broker-probe-never"}`, "request not allowed\n", false},
	{"duplicate_field", `{"op":"ping","op":"ping"}`, "invalid request\n", false},
	{"null_field", `{"op":null}`, "invalid request\n", false},
	{"unknown_field", `{"op":"ping","probe_extra":true}`, "invalid request\n", false},
}

type frame struct {
	Output   *string `json:"output"`
	ExitCode *int    `json:"exit_code"`
}
type reply struct {
	output string
	code   int
}

func readReply(input io.Reader) (reply, error) {
	var result reply
	reader := bufio.NewReader(io.LimitReader(input, maxResponseBytes+1))
	total := 0
	exited := false
	for {
		line, err := reader.ReadBytes('\n')
		total += len(line)
		if total > maxResponseBytes {
			return result, errors.New("response exceeds limit")
		}
		if err == io.EOF && len(line) == 0 {
			if !exited {
				return result, errors.New("missing exit frame")
			}
			return result, nil
		}
		if err != nil {
			return result, errors.New("truncated response")
		}
		if exited {
			return result, errors.New("frames after exit")
		}
		// Reject duplicate fields, null/missing output, unknown fields, wrong types,
		// malformed JSON, and anything except one JSON object per line.
		var raw map[string]json.RawMessage
		if json.Unmarshal(line, &raw) != nil || raw == nil {
			return result, errors.New("malformed frame")
		}
		d := json.NewDecoder(bytes.NewReader(line))
		tok, e := d.Token()
		if e != nil || tok != json.Delim('{') {
			return result, errors.New("malformed frame")
		}
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			key, ok := k.(string)
			if e != nil || !ok || seen[key] {
				return result, errors.New("duplicate frame field")
			}
			seen[key] = true
			var value json.RawMessage
			if d.Decode(&value) != nil {
				return result, errors.New("malformed frame")
			}
		}
		for k, v := range raw {
			if (k != "output" && k != "exit_code") || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return result, errors.New("unexpected frame field")
			}
		}
		var f frame
		if json.Unmarshal(line, &f) != nil || f.Output == nil {
			return result, errors.New("invalid frame")
		}
		result.output += *f.Output
		if f.ExitCode != nil {
			result.code = *f.ExitCode
			exited = true
		}
	}
}
func rawRequest(ctx context.Context, socket, request string, emptyTar bool) (reply, error) {
	// A total per-request deadline includes dialing, writing and reading through EOF.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	c, e := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if e != nil {
		return reply{}, errors.New("broker connection failed")
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	if e = c.SetDeadline(deadline); e != nil {
		return reply{}, errors.New("broker deadline failed")
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	// Send the bounded header and optional empty tar in one write. The server
	// may reject the header immediately without reading the archive.
	wire := []byte(request + "\n")
	if emptyTar {
		// Two zero-filled 512-byte records: no checkout or real Dockerfile.
		wire = append(wire, make([]byte, 1024)...)
	}
	if n, err := c.Write(wire); err != nil || n != len(wire) {
		return reply{}, errors.New("broker request write failed")
	}
	unix, ok := c.(*net.UnixConn)
	if !ok {
		return reply{}, errors.New("not a Unix connection")
	}
	if err := unix.CloseWrite(); err != nil && !errors.Is(err, syscall.ENOTCONN) {
		return reply{}, errors.New("broker half-close failed")
	}
	// ENOTCONN can mean an early-rejecting peer already closed its end.
	// Still require the complete, exact terminal response; it is not success.
	response, e := readReply(c)
	if e != nil {
		return reply{}, errors.New("invalid, oversized, incomplete or timed-out broker response")
	}
	return response, nil
}
func brokerChecks(ctx context.Context, r *report, socket string) {
	pong, e := rawRequest(ctx, socket, `{"op":"ping"}`, false)
	r.add("broker_ping_allowed", e == nil && pong.code == 0 && pong.output == "pong\n", "raw broker ping must return pong and exit 0")
	for _, d := range denials {
		response, e := rawRequest(ctx, socket, d.request, d.emptyTar)
		// A busy response is a failure, even if it has code 1. This ensures server
		// denials cannot be replaced with slot contention (including during a build).
		pass := e == nil && response.code == 1 && response.output == d.output
		detail := "server must return exact denial and exit 1, never busy or success"
		if e != nil {
			detail = "broker response unavailable, malformed, incomplete, oversized or timed out"
		} else if response.code == 0 {
			detail = "broker unexpectedly accepted a permanently denied input"
		} else if strings.Contains(response.output, "broker busy") {
			detail = "broker returned busy instead of validating and denying the request first"
		} else if response.code != 1 {
			detail = "broker returned the wrong denial exit status"
		} else if response.output != d.output {
			detail = "broker did not return the exact expected server denial"
		}
		r.add("server_denies_"+d.name, pass, detail)
	}
}
func runChecks(ctx context.Context) report {
	r := report{Pass: true}
	filesystemChecks(&r, realReaders())
	brokerChecks(ctx, &r, socketPath)
	return r
}
func reportValid(r report) bool {
	if !r.Pass || len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return true
}
func printReport(out io.Writer, r report) {
	for _, c := range r.Checks {
		label := "FAIL"
		if c.Pass {
			label = "PASS"
		}
		fmt.Fprintf(out, "%s %s: %s\n", label, c.Name, c.Detail)
	}
	if reportValid(r) {
		fmt.Fprintln(out, "PASS broker-probe")
	} else {
		fmt.Fprintln(out, "FAIL broker-probe")
	}
}
func frozenHandler(r report) http.Handler {
	// Serialize once. Requests cannot mutate the report or rerun security calls.
	data, _ := json.Marshal(r)
	ok := reportValid(r)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/health" || req.Method != "GET" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write(data)
	})
}
func healthcheck(url string, out io.Writer) int {
	client := &http.Client{Timeout: 2500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	response, e := client.Get(url)
	if e != nil {
		fmt.Fprintln(out, "FAIL health: frozen startup report unavailable")
		return 1
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if e != nil || len(data) > maxResponseBytes {
		fmt.Fprintln(out, "FAIL health: bounded report read failed")
		return 1
	}
	var r report
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		fmt.Fprintln(out, "FAIL health: invalid report")
		return 1
	}
	printReport(out, r)
	if response.StatusCode == http.StatusOK && reportValid(r) {
		return 0
	}
	return 1
}
func serve(ctx context.Context, listener net.Listener, lifetime time.Duration, checks func(context.Context) report, out io.Writer) int {
	// The deadline begins before checks, not after HTTP startup. The only caller
	// outside tests supplies maxLifetime. No environment can extend the lifetime.
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, lifetime)
	defer cancel()
	defer listener.Close()
	r := checks(ctx)
	printReport(out, r)
	server := &http.Server{Handler: frozenHandler(r), ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		_ = server.Close()
		<-done
	case <-done:
		_ = server.Close()
		fmt.Fprintln(out, "FAIL health: report server stopped before lifetime deadline")
		return 1
	}
	if parent.Err() != nil {
		fmt.Fprintln(out, "FAIL lifetime: interrupted before natural five-minute completion")
		return 1
	}
	if ctx.Err() == context.DeadlineExceeded && reportValid(r) {
		return 0
	}
	return 1
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck("http://"+healthAddress+"/health", os.Stdout))
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "FAIL usage: /probe [healthcheck]")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, e := net.Listen("tcp4", healthAddress)
	if e != nil {
		fmt.Fprintln(os.Stderr, "FAIL health: loopback listener unavailable")
		os.Exit(1)
	}
	os.Exit(serve(ctx, listener, maxLifetime, runChecks, os.Stdout))
}
