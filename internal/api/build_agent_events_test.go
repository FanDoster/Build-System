package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

type buildAgentEvent struct {
	kind string
	data json.RawMessage
}

func nextBuildAgentEvent(t *testing.T, sc *bufio.Scanner) buildAgentEvent {
	t.Helper()
	var ev buildAgentEvent
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if ev.kind != "" {
				if !json.Valid(ev.data) {
					t.Fatalf("invalid %s JSON: %q", ev.kind, ev.data)
				}
				return ev
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			ev.kind = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			ev.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
	t.Fatalf("stream ended before next event: %v", sc.Err())
	return ev
}

func checkBuildAgentStatus(t *testing.T, ev buildAgentEvent, status models.BuildStatus, agent, executor string, started, finished *time.Time) {
	t.Helper()
	if ev.kind != "status" {
		t.Fatalf("event = %q, want status: %s", ev.kind, ev.data)
	}
	var got struct {
		Status     models.BuildStatus `json:"status"`
		Agent      *string            `json:"agent"`
		Executor   *string            `json:"executor"`
		StartedAt  *time.Time         `json:"started_at"`
		FinishedAt *time.Time         `json:"finished_at"`
	}
	if err := json.Unmarshal(ev.data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent == nil || got.Executor == nil {
		t.Fatalf("ownership fields must be present, including when empty: %s", ev.data)
	}
	if got.Status != status || *got.Agent != agent || *got.Executor != executor {
		t.Errorf("status payload = %s; want %s agent=%q executor=%q", ev.data, status, agent, executor)
	}
	sameTime := func(a, b *time.Time) bool {
		return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
	}
	if !sameTime(got.StartedAt, started) || !sameTime(got.FinishedAt, finished) {
		t.Errorf("status timestamps = %s; want started=%v finished=%v", ev.data, started, finished)
	}
}

func checkBuildAgentLog(t *testing.T, ev buildAgentEvent, text string, offset int) {
	t.Helper()
	var got struct {
		Text   string `json:"t"`
		Offset int    `json:"o"`
	}
	if ev.kind != "log" {
		t.Fatalf("event = %q, want log before terminal: %s", ev.kind, ev.data)
	}
	if err := json.Unmarshal(ev.data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != text || got.Offset != offset {
		t.Errorf("log = %s, want text=%q offset=%d", ev.data, text, offset)
	}
}

func openBuildAgentStream(t *testing.T, mux *http.ServeMux, id int64) *bufio.Scanner {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/api/builds/%d/events", srv.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	return bufio.NewScanner(resp.Body)
}

func newBuildAgentEventBuild(t *testing.T, s *Server, executor string) *models.Build {
	t.Helper()
	project := createProject(t, s, models.Project{
		Name: "agent-events", RepoURL: "https://github.com/u/game", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "game", Executor: executor,
	})
	return pending(t, s, project.ID)
}

func claimBuildAgentEvent(t *testing.T, s *Server, id int64, agent, executor string) *models.Build {
	t.Helper()
	b, err := s.DB.ClaimBuildForAgent(agent, []string{executor})
	if err != nil || b == nil || b.ID != id {
		t.Fatalf("claim = %+v, %v; want build %d", b, err, id)
	}
	return b
}

func TestSSEBuildAgentInitialOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, executor, agent string
		status                models.BuildStatus
	}{
		{"queued-remote", "unity-windows", "", models.StatusPending},
		{"running-remote", "unity-windows", "windows-laptop", models.StatusRunning},
		{"queued-local", "local", "", models.StatusPending},
		{"running-local", "local", "", models.StatusRunning},
		{"default-local", "", "", models.StatusRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mux := newTestServer(t)
			b := newBuildAgentEventBuild(t, s, tc.executor)
			if tc.agent != "" {
				claimBuildAgentEvent(t, s, b.ID, tc.agent, tc.executor)
			} else if tc.status == models.StatusRunning {
				if ok, err := s.DB.ClaimBuild(b.ID); !ok || err != nil {
					t.Fatalf("local claim: %v, %v", ok, err)
				}
			}
			stored, err := s.DB.GetBuildSummary(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			sc := openBuildAgentStream(t, mux, b.ID)
			executor := tc.executor
			if executor == "" {
				executor = models.ExecutorLocal // CreateProject's default.
			}
			checkBuildAgentStatus(t, nextBuildAgentEvent(t, sc), tc.status, tc.agent, executor, stored.StartedAt, stored.FinishedAt)
		})
	}
}

func TestSSEBuildAgentLiveAssignmentAndTerminalTail(t *testing.T) {
	s, mux := newTestServer(t)
	b := newBuildAgentEventBuild(t, s, "unity-windows")
	sc := openBuildAgentStream(t, mux, b.ID)
	checkBuildAgentStatus(t, nextBuildAgentEvent(t, sc), models.StatusPending, "", "unity-windows", nil, nil)

	// The queue name is not an agent identity. Claiming assigns the real worker.
	agent := "windows-pc-7"
	claimBuildAgentEvent(t, s, b.ID, agent, "unity-windows")
	prefix, tail := "live log\n", "stored-only final reason →\n"
	if err := s.DB.AppendBuildLog(b.ID, prefix+tail); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.FinishBuild(b.ID, models.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	// The DB is already terminal when the running event is read. Its status
	// and timestamps must not replace those carried by the FIFO bus event.
	started := time.Date(2026, 10, 15, 9, 0, 0, 123, time.UTC)
	finished := started.Add(time.Minute)
	s.Bus.PublishStatus(b.ID, models.StatusRunning, &started, nil)
	s.Bus.Publish(b.ID, []byte(prefix))
	s.Bus.PublishStatus(b.ID, models.StatusSuccess, &started, &finished)

	checkBuildAgentStatus(t, nextBuildAgentEvent(t, sc), models.StatusRunning, agent, "unity-windows", &started, nil)
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), prefix, len(prefix))
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), tail, len(prefix+tail))
	checkBuildAgentStatus(t, nextBuildAgentEvent(t, sc), models.StatusSuccess, agent, "unity-windows", &started, &finished)
	if sc.Scan() {
		t.Fatalf("unexpected event after terminal: %s", sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestSSEBuildAgentTerminalReplaySafeJSON(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("buffered=%v", buffered), func(t *testing.T) {
			s, mux := newTestServer(t)
			b := newBuildAgentEventBuild(t, s, "unity-windows")
			agent := "<script>alert(\"agent\")</script>\n\nevent: injected\ndata: {}&"
			claimBuildAgentEvent(t, s, b.ID, agent, "unity-windows")
			log := "skip\nfinal log\n"
			if err := s.DB.AppendBuildLog(b.ID, log); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.FinishBuild(b.ID, models.StatusFailed); err != nil {
				t.Fatal(err)
			}
			stored, err := s.DB.GetBuildSummary(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if buffered {
				s.Bus.Publish(b.ID, []byte(log))
				s.Bus.PublishStatus(b.ID, stored.Status, stored.StartedAt, stored.FinishedAt)
			}
			// Last-Event-ID still wins over the query offset.
			req := httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events?offset=1", b.ID), nil)
			req.Header.Set("Last-Event-ID", "5")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
			checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), log[5:], len(log))
			ev := nextBuildAgentEvent(t, sc)
			checkBuildAgentStatus(t, ev, models.StatusFailed, agent, "unity-windows", stored.StartedAt, stored.FinishedAt)
			if strings.Contains(string(ev.data), "<script>") || strings.ContainsRune(string(ev.data), '\n') {
				t.Errorf("agent was not safely JSON encoded: %s", ev.data)
			}
			if sc.Scan() {
				t.Fatalf("agent injected an SSE event: %s", sc.Text())
			}
		})
	}
}

// Hooks make races deterministic without sleeping or changing the bus API.
type buildAgentHookWriter struct {
	*httptest.ResponseRecorder
	onHeader func()
	onFlush  func()
}

func (w *buildAgentHookWriter) WriteHeader(code int) {
	w.ResponseRecorder.WriteHeader(code)
	if hook := w.onHeader; hook != nil {
		w.onHeader = nil
		hook()
	}
}

func (w *buildAgentHookWriter) Flush() {
	w.ResponseRecorder.Flush()
	if hook := w.onFlush; hook != nil {
		w.onFlush = nil
		hook()
	}
}

func TestSSEBuildAgentFinishesBeforeReplay(t *testing.T) {
	s, mux := newTestServer(t)
	b := newBuildAgentEventBuild(t, s, "unity-windows")
	w := &buildAgentHookWriter{ResponseRecorder: httptest.NewRecorder()}
	var stored *models.Build
	w.onHeader = func() {
		claimBuildAgentEvent(t, s, b.ID, "remote-after-snapshot", "unity-windows")
		if err := s.DB.AppendBuildLog(b.ID, "final replay\n"); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.FinishBuild(b.ID, models.StatusSuccess); err != nil {
			t.Fatal(err)
		}
		var err error
		stored, err = s.DB.GetBuildSummary(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		s.Bus.Publish(b.ID, []byte("final replay\n"))
		s.Bus.PublishStatus(b.ID, stored.Status, stored.StartedAt, stored.FinishedAt)
	}
	mux.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events", b.ID), nil))
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), "final replay\n", len("final replay\n"))
	checkBuildAgentStatus(t, nextBuildAgentEvent(t, sc), models.StatusSuccess, "remote-after-snapshot", "unity-windows", stored.StartedAt, stored.FinishedAt)
}

func TestSSEBuildAgentDroppedSubscriberTerminalOwnership(t *testing.T) {
	s, mux := newTestServer(t)
	b := newBuildAgentEventBuild(t, s, "unity-windows")
	w := &buildAgentHookWriter{ResponseRecorder: httptest.NewRecorder()}
	var stored *models.Build
	w.onFlush = func() {
		claimBuildAgentEvent(t, s, b.ID, "remote-after-subscribe", "unity-windows")
		// Overflow the subscriber while the handler is still in its initial
		// Flush. The closed-channel fallback must recover tail and ownership.
		log := strings.Repeat("x", 100)
		if err := s.DB.AppendBuildLog(b.ID, log); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(log); i++ {
			s.Bus.Publish(b.ID, []byte("x"))
		}
		if err := s.DB.FinishBuild(b.ID, models.StatusSuccess); err != nil {
			t.Fatal(err)
		}
		var err error
		stored, err = s.DB.GetBuildSummary(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		s.Bus.PublishStatus(b.ID, stored.Status, stored.StartedAt, stored.FinishedAt)
	}
	mux.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events", b.ID), nil))
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	checkBuildAgentStatus(t, nextBuildAgentEvent(t, sc), models.StatusPending, "", "unity-windows", nil, nil)
	var log strings.Builder
	for {
		ev := nextBuildAgentEvent(t, sc)
		if ev.kind == "status" {
			if log.String() != strings.Repeat("x", 100) {
				t.Fatalf("terminal status arrived before full log: %d bytes", log.Len())
			}
			checkBuildAgentStatus(t, ev, models.StatusSuccess, "remote-after-subscribe", "unity-windows", stored.StartedAt, stored.FinishedAt)
			break
		}
		var chunk struct {
			Text string `json:"t"`
		}
		if err := json.Unmarshal(ev.data, &chunk); err != nil {
			t.Fatal(err)
		}
		log.WriteString(chunk.Text)
	}
}
