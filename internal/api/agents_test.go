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

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

// newAgentServer is newTestServer with the claim long-poll shortened, so tests
// that expect "nothing to claim" do not sit through the production hold.
func newAgentServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	s, mux := newTestServer(t)
	s.AgentPollHold = 20 * time.Millisecond
	s.AgentPollInterval = 5 * time.Millisecond
	return s, mux
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, dst interface{}) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

func macProject(t *testing.T, s *Server) *models.Project {
	t.Helper()
	return createProject(t, s, models.Project{
		Name: "game", RepoURL: "https://github.com/nmr/ship", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "game",
		Executor: "mac", CloneToken: "ghp_secret",
	})
}

func pending(t *testing.T, s *Server, projectID int64) *models.Build {
	t.Helper()
	b := &models.Build{ProjectID: projectID, Status: models.StatusPending, CommitSHA: "abc123"}
	if err := s.DB.CreateBuild(b); err != nil {
		t.Fatalf("create build: %v", err)
	}
	return b
}

// claimOne performs the claim an agent named mac-1 would make, and fails the
// test if there was nothing to claim.
func claimOne(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"agent": "mac-1", "executors": []string{"mac"}})
	if w.Code != 200 {
		t.Fatalf("claim: status %d body %s", w.Code, w.Body.String())
	}
}

func agentLog(t *testing.T, mux *http.ServeMux, id int64, agent string, offset int, data string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, mux, "POST", buildPath(id, "log"),
		map[string]interface{}{"agent": agent, "offset": offset, "data": data})
}

func buildPath(id int64, action string) string {
	return fmt.Sprintf("/api/builds/%d/%s", id, action)
}

func TestAgentClaimReturnsBuildAndCredentials(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)

	w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"agent": "mac-1", "executors": []string{"mac"}})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Build   *models.Build   `json:"build"`
		Project *models.Project `json:"project"`
	}
	decodeJSON(t, w, &got)

	if got.Build == nil || got.Build.ID != build.ID {
		t.Fatalf("claimed %v, want build %d", got.Build, build.ID)
	}
	if got.Build.Status != models.StatusRunning || got.Build.Agent != "mac-1" {
		t.Errorf("build: status=%s agent=%q, want running/mac-1", got.Build.Status, got.Build.Agent)
	}
	if got.Project == nil {
		t.Fatal("no project in claim response")
	}
	// The agent has to clone the same private repo the local runner would, so
	// unlike every other project response this one keeps the token.
	if got.Project.CloneToken != "ghp_secret" {
		t.Errorf("clone_token = %q, want it delivered to the agent", got.Project.CloneToken)
	}
	if got.Project.RepoURL != "https://github.com/nmr/ship" {
		t.Errorf("repo_url = %q", got.Project.RepoURL)
	}
}

func TestAgentClaimIgnoresLocalProjects(t *testing.T) {
	s, mux := newAgentServer(t)
	local := createProject(t, s, models.Project{
		Name: "web", RepoURL: "https://github.com/u/web", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "web",
	})
	b := pending(t, s, local.ID)

	w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"agent": "mac-1", "executors": []string{"mac"}})
	if w.Code != 204 {
		t.Fatalf("status %d (%s), want 204 — an agent took a local build",
			w.Code, w.Body.String())
	}
	got, _ := s.DB.GetBuild(b.ID)
	if got.Status != models.StatusPending {
		t.Errorf("local build status = %s, want pending", got.Status)
	}
}

func TestAgentClaimRejectsLocalExecutor(t *testing.T) {
	_, mux := newAgentServer(t)
	for _, executors := range [][]string{{"local"}, {""}, {"mac", "local"}} {
		w := doJSON(t, mux, "POST", "/api/agents/claim",
			map[string]interface{}{"agent": "mac-1", "executors": executors})
		if w.Code != 400 {
			t.Errorf("executors %v: status %d, want 400 (the local queue is the runner's)",
				executors, w.Code)
		}
	}
}

func TestAgentClaimRequiresIdentity(t *testing.T) {
	_, mux := newAgentServer(t)
	if w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"executors": []string{"mac"}}); w.Code != 400 {
		t.Errorf("nameless claim: status %d, want 400", w.Code)
	}
	if w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"agent": "mac-1"}); w.Code != 400 {
		t.Errorf("claim without executors: status %d, want 400", w.Code)
	}
}

// The log endpoint is the whole live-log path for agent builds: what it writes
// has to reach both the durable row and the bus that feeds SSE, byte for byte.
func TestAgentLogAppendsToBothDBAndBus(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	first := "[12:00:00] ##[step:checkout] Fetching nmr/ship @ main\n"
	second := "[12:00:03] ##[step:unity] 2022.3.62f2 → StandaloneWindows64\n"

	w := agentLog(t, mux, build.ID, "mac-1", 0, first)
	if w.Code != 200 {
		t.Fatalf("first append: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Len    int  `json:"len"`
		Cancel bool `json:"cancel"`
	}
	decodeJSON(t, w, &got)
	if got.Len != len(first) {
		t.Errorf("len = %d, want %d", got.Len, len(first))
	}

	w = agentLog(t, mux, build.ID, "mac-1", len(first), second)
	if w.Code != 200 {
		t.Fatalf("second append: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &got)
	if want := len(first) + len(second); got.Len != want {
		t.Errorf("len = %d, want %d", got.Len, want)
	}

	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != first+second {
		t.Errorf("stored log = %q, want %q", stored.Log, first+second)
	}
	tail, cur, ok := s.Bus.LogTail(build.ID, 0)
	if !ok {
		t.Fatal("no live topic for a running agent build")
	}
	if string(tail) != stored.Log || cur != len(stored.Log) {
		t.Errorf("bus buffer %q (len %d) != stored log %q (len %d) — the byte-identity invariant is broken",
			tail, cur, stored.Log, len(stored.Log))
	}
}

// A retry after a lost response must not print the log twice.
func TestAgentLogRetryIsIdempotent(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	chunk := "compiling shaders\n"
	if w := agentLog(t, mux, build.ID, "mac-1", 0, chunk); w.Code != 200 {
		t.Fatalf("append: %d", w.Code)
	}
	// Same offset, same bytes: the agent never saw the first ack.
	w := agentLog(t, mux, build.ID, "mac-1", 0, chunk)
	if w.Code != 200 {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Len int `json:"len"`
	}
	decodeJSON(t, w, &got)
	if got.Len != len(chunk) {
		t.Errorf("len = %d after replay, want %d", got.Len, len(chunk))
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != chunk {
		t.Errorf("log = %q, want the chunk exactly once", stored.Log)
	}

	// Partial overlap: the agent resends from an older offset with more data.
	if w := agentLog(t, mux, build.ID, "mac-1", 0, chunk+"linking\n"); w.Code != 200 {
		t.Fatalf("overlapping append: %d %s", w.Code, w.Body.String())
	}
	stored, _ = s.DB.GetBuild(build.ID)
	if want := chunk + "linking\n"; stored.Log != want {
		t.Errorf("log = %q, want %q — only the new tail should have been appended", stored.Log, want)
	}
	if _, cur, _ := s.Bus.LogTail(build.ID, 0); cur != len(stored.Log) {
		t.Errorf("bus len %d != db len %d after an overlapping append", cur, len(stored.Log))
	}
}

func TestAgentLogRejectsOffsetPastEnd(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)
	agentLog(t, mux, build.ID, "mac-1", 0, "hello\n")

	w := agentLog(t, mux, build.ID, "mac-1", 999, "way ahead\n")
	if w.Code != 409 {
		t.Fatalf("status %d, want 409", w.Code)
	}
	var got struct {
		Len int `json:"len"`
	}
	decodeJSON(t, w, &got)
	if got.Len != len("hello\n") {
		t.Errorf("resync len = %d, want %d", got.Len, len("hello\n"))
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != "hello\n" {
		t.Errorf("log = %q — a rejected append must write nothing", stored.Log)
	}
}

// A build that outlives the process streaming it: the topic is gone, the row
// is not. The next append must not strand the earlier output.
func TestAgentLogSeedsBusAfterServerRestart(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	before := "[12:00:00] ##[step:unity] importing assets\n"
	agentLog(t, mux, build.ID, "mac-1", 0, before)

	// Restart: same DB, brand-new bus with no memory of this build.
	s.Bus = logbus.New()

	after := "[12:09:00] ##[step:steam] uploading\n"
	w := agentLog(t, mux, build.ID, "mac-1", len(before), after)
	if w.Code != 200 {
		t.Fatalf("append after restart: %d %s", w.Code, w.Body.String())
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != before+after {
		t.Errorf("stored log = %q, want %q", stored.Log, before+after)
	}
	tail, cur, ok := s.Bus.LogTail(build.ID, 0)
	if !ok {
		t.Fatal("no topic after restart append")
	}
	if string(tail) != stored.Log || cur != len(stored.Log) {
		t.Errorf("bus %q (len %d) does not mirror the stored log %q (len %d) after a restart",
			tail, cur, stored.Log, len(stored.Log))
	}
}

func TestAgentLogRejectsForeignAgent(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	if w := agentLog(t, mux, build.ID, "impostor", 0, "evil\n"); w.Code != 409 {
		t.Errorf("status %d, want 409 for a build owned by another agent", w.Code)
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != "" {
		t.Errorf("log = %q, want empty", stored.Log)
	}
}

func TestAgentHeartbeatCarriesCancel(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	w := doJSON(t, mux, "POST", buildPath(build.ID, "heartbeat"),
		map[string]interface{}{"agent": "mac-1"})
	if w.Code != 200 {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body.String())
	}
	var beat struct {
		OK     bool `json:"ok"`
		Cancel bool `json:"cancel"`
	}
	decodeJSON(t, w, &beat)
	if !beat.OK || beat.Cancel {
		t.Errorf("heartbeat = %+v, want ok without cancel", beat)
	}

	// Operator cancels from the UI: the build stays running until the agent
	// acts on it, so the answer is 202, not 200.
	cw := doJSON(t, mux, "POST", buildPath(build.ID, "cancel"), nil)
	if cw.Code != 202 {
		t.Fatalf("cancel: status %d (%s), want 202 canceling", cw.Code, cw.Body.String())
	}
	row, _ := s.DB.GetBuild(build.ID)
	if row.Status != models.StatusRunning {
		t.Errorf("status = %s immediately after cancel, want still running", row.Status)
	}

	w = doJSON(t, mux, "POST", buildPath(build.ID, "heartbeat"),
		map[string]interface{}{"agent": "mac-1"})
	decodeJSON(t, w, &beat)
	if !beat.Cancel {
		t.Error("heartbeat did not carry the pending cancel — the agent would never hear about it")
	}

	// A busy agent hears about it on its next log upload too, which is the
	// faster path while output is flowing.
	lw := agentLog(t, mux, build.ID, "mac-1", 0, "still going\n")
	var logged struct {
		Cancel bool `json:"cancel"`
	}
	decodeJSON(t, lw, &logged)
	if !logged.Cancel {
		t.Error("log response did not carry the pending cancel")
	}
}

func TestAgentHeartbeatRejectsForeignAgent(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	w := doJSON(t, mux, "POST", buildPath(build.ID, "heartbeat"),
		map[string]interface{}{"agent": "impostor"})
	if w.Code != 409 {
		t.Errorf("status %d, want 409", w.Code)
	}
}

func TestAgentFinishRecordsStatusAndNotifies(t *testing.T) {
	s, mux := newAgentServer(t)
	n := &fakeNotifier{}
	s.Notifier = n
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)
	agentLog(t, mux, build.ID, "mac-1", 0, "[12:10:00] BUILD SUCCESS\n")

	w := doJSON(t, mux, "POST", buildPath(build.ID, "finish"),
		map[string]interface{}{"agent": "mac-1", "status": "success"})
	if w.Code != 200 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	row, _ := s.DB.GetBuild(build.ID)
	if row.Status != models.StatusSuccess {
		t.Errorf("status = %s, want success", row.Status)
	}
	if row.FinishedAt == nil {
		t.Error("finished_at not stamped on a build that really did finish")
	}
	if len(n.calls) != 1 || n.calls[0] != build.ID {
		t.Errorf("notifier calls = %v, want one for build %d", n.calls, build.ID)
	}
}

func TestAgentFinishRejectsNonTerminalStatus(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	w := doJSON(t, mux, "POST", buildPath(build.ID, "finish"),
		map[string]interface{}{"agent": "mac-1", "status": "running"})
	if w.Code != 400 {
		t.Errorf("status %d, want 400", w.Code)
	}
}

// The agent won a race it did not know it was in: the janitor gave up on it
// first. Recording the fact must not be an error, and must not rewrite the row.
func TestAgentFinishAfterJanitorGaveUp(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	if err := s.DB.FinishBuild(build.ID, models.StatusFailed); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, mux, "POST", buildPath(build.ID, "finish"),
		map[string]interface{}{"agent": "mac-1", "status": "success"})
	if w.Code != 200 {
		t.Fatalf("status %d (%s), want 200 — a late finish is not an error", w.Code, w.Body.String())
	}
	var got struct {
		Status  models.BuildStatus `json:"status"`
		Already bool               `json:"already"`
	}
	decodeJSON(t, w, &got)
	if got.Status != models.StatusFailed || !got.Already {
		t.Errorf("response = %+v, want the recorded failed status marked already", got)
	}
	row, _ := s.DB.GetBuild(build.ID)
	if row.Status != models.StatusFailed {
		t.Errorf("status = %s — a late report must not rewrite the outcome", row.Status)
	}
}

// The routing gate: a remote project's build must never reach the local queue.
func TestTriggerBuildForAgentProjectSkipsLocalQueue(t *testing.T) {
	s, mux := newTestServer(t)
	p := macProject(t, s)

	w := doJSON(t, mux, "POST", "/api/projects/"+fmt.Sprintf("%d", p.ID)+"/build", nil)
	if w.Code != 201 {
		t.Fatalf("trigger: %d %s", w.Code, w.Body.String())
	}
	select {
	case b := <-s.BuildCh:
		t.Fatalf("build %d was put on the local channel; the Docker runner would run a Unity build", b.ID)
	default:
	}
	var created models.Build
	decodeJSON(t, w, &created)
	row, _ := s.DB.GetBuild(created.ID)
	if row.Status != models.StatusPending {
		t.Errorf("status = %s, want pending (waiting for an agent to claim it)", row.Status)
	}
}

func TestTriggerBuildForLocalProjectStillQueues(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "web", RepoURL: "https://github.com/u/web", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "web",
	})
	if w := doJSON(t, mux, "POST", "/api/projects/"+fmt.Sprintf("%d", p.ID)+"/build", nil); w.Code != 201 {
		t.Fatalf("trigger: %d", w.Code)
	}
	select {
	case <-s.BuildCh:
	default:
		t.Fatal("local build did not reach the runner's channel")
	}
}

func TestUpdateProjectExecutorValidation(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "web", RepoURL: "https://github.com/u/web", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "web",
	})
	path := "/api/projects/" + fmt.Sprintf("%d", p.ID)

	if w := doJSON(t, mux, "PUT", path, map[string]interface{}{"executor": "mac"}); w.Code != 200 {
		t.Fatalf("set executor: %d %s", w.Code, w.Body.String())
	}
	got, _ := s.DB.GetProject(p.ID)
	if got.Executor != "mac" {
		t.Errorf("executor = %q, want mac", got.Executor)
	}

	for _, bad := range []string{"Mac", "mac agent", "mac!", strings40()} {
		if w := doJSON(t, mux, "PUT", path, map[string]interface{}{"executor": bad}); w.Code != 400 {
			t.Errorf("executor %q: status %d, want 400", bad, w.Code)
		}
	}
	// Clearing it puts the project back on the local runner.
	if w := doJSON(t, mux, "PUT", path, map[string]interface{}{"executor": ""}); w.Code != 200 {
		t.Fatalf("clear executor: %d", w.Code)
	}
	got, _ = s.DB.GetProject(p.ID)
	if got.Executor != models.ExecutorLocal {
		t.Errorf("executor = %q after clearing, want %q", got.Executor, models.ExecutorLocal)
	}
}

type fakeNotifier struct{ calls []int64 }

func (f *fakeNotifier) NotifyFinished(id int64, status models.BuildStatus, startedAt, finishedAt time.Time) {
	f.calls = append(f.calls, id)
}

func strings40() string {
	s := ""
	for i := 0; i < 40; i++ {
		s += "a"
	}
	return s
}

// End to end over real HTTP: what an agent POSTs must arrive on a browser's
// SSE stream in the right order, with the terminal status last. This is the
// whole point of having agents emit the runner's log grammar — the step rail
// and live log in the UI are a function of these bytes and nothing else.
func TestAgentBuildStreamsOverSSE(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(fmt.Sprintf("%s/api/builds/%d/events", srv.URL, build.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		steps := []string{
			"[12:00:00] ##[step:checkout] Fetching nmr/ship @ main\n",
			"[12:00:30] ##[step:unity] 2022.3.62f2 → StandaloneWindows64\n",
			"[12:09:00] ##[step:steam] app 4790640 → branch \"upload\"\n",
			"[12:11:00] BUILD SUCCESS\n",
		}
		offset := 0
		for _, line := range steps {
			agentLog(t, mux, build.ID, "mac-1", offset, line)
			offset += len(line)
		}
		doJSON(t, mux, "POST", buildPath(build.ID, "finish"),
			map[string]interface{}{"agent": "mac-1", "status": "success"})
	}()

	deadline := time.AfterFunc(10*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()

	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}
	joined := strings.Join(events, "\n")

	for _, want := range []string{"step:checkout", "step:unity", "step:steam", "BUILD SUCCESS"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SSE stream missing %q:\n%s", want, joined)
		}
	}
	// The client closes its EventSource on a terminal status, so anything
	// written after it is lost.
	successIdx := strings.Index(joined, "BUILD SUCCESS")
	statusIdx := strings.Index(joined, `"status":"success"`)
	if successIdx == -1 || statusIdx == -1 {
		t.Fatalf("missing log or status event:\n%s", joined)
	}
	if successIdx > statusIdx {
		t.Error("terminal status was sent before the final log bytes; the client would drop them")
	}
}

// A runaway agent must not be able to pull the server's memory down with one
// request: the body is buffered into a string and concatenated onto the row.
func TestAgentLogRejectsOversizedBody(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	huge := strings.Repeat("x", maxAgentLogBody+1024)
	w := agentLog(t, mux, build.ID, "mac-1", 0, huge)
	if w.Code != 413 {
		t.Fatalf("status %d, want 413 for a body over %d bytes", w.Code, maxAgentLogBody)
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != "" {
		t.Errorf("log = %d bytes, want nothing written for a rejected body", len(stored.Log))
	}
	// A normal chunk still works afterwards.
	if w := agentLog(t, mux, build.ID, "mac-1", 0, "fine\n"); w.Code != 200 {
		t.Errorf("normal append after a rejected one: %d %s", w.Code, w.Body.String())
	}
}

// A build can reach an agent already carrying output: it was requeued while
// its project still built locally, and the project moved to a remote executor
// before anyone claimed it. The agent is told where to start; and an agent
// that starts at 0 anyway must be corrected, never silently swallowed.
func TestAgentClaimReportsLogOffsetOfAPartlyLoggedBuild(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)

	old := "[11:00:00] ##[step:build] previous attempt\n[restart] re-queued\n"
	if err := s.DB.AppendBuildLog(build.ID, old); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"agent": "mac-1", "executors": []string{"mac"}})
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		LogOffset int `json:"log_offset"`
	}
	decodeJSON(t, w, &got)
	if got.LogOffset != len(old) {
		t.Errorf("log_offset = %d, want %d — the agent would start writing at the wrong place",
			got.LogOffset, len(old))
	}

	// An agent that ignores it and starts at 0 gets a resync, not silence.
	fresh := "[12:00:00] ##[step:checkout] Fetching\n"
	lw := agentLog(t, mux, build.ID, "mac-1", 0, fresh)
	if lw.Code != 409 {
		t.Fatalf("append at a stale offset: %d %s, want 409", lw.Code, lw.Body.String())
	}
	var resync struct {
		Len int `json:"len"`
	}
	decodeJSON(t, lw, &resync)
	if resync.Len != len(old) {
		t.Errorf("resync len = %d, want %d", resync.Len, len(old))
	}

	// Retrying at the offset it was given lands the output.
	if lw := agentLog(t, mux, build.ID, "mac-1", resync.Len, fresh); lw.Code != 200 {
		t.Fatalf("append after resync: %d %s", lw.Code, lw.Body.String())
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != old+fresh {
		t.Errorf("stored log = %q, want the previous attempt followed by the agent's output", stored.Log)
	}
}

// The idempotent-retry path must still be a genuine byte match, not "anything
// below the stored length".
func TestAgentLogMismatchedRetryIsRejectedNotSwallowed(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	first := "aaaaaaaa\n"
	if w := agentLog(t, mux, build.ID, "mac-1", 0, first); w.Code != 200 {
		t.Fatalf("first append: %d", w.Code)
	}
	// Same offset and length, different bytes: the two sides disagree about
	// history and the server must say so rather than pick one.
	w := agentLog(t, mux, build.ID, "mac-1", 0, "bbbbbbbb\n")
	if w.Code != 409 {
		t.Fatalf("mismatched replay: %d %s, want 409", w.Code, w.Body.String())
	}
	stored, _ := s.DB.GetBuild(build.ID)
	if stored.Log != first {
		t.Errorf("stored log = %q, want it unchanged", stored.Log)
	}
}

// Moving a project back to the local runner has to bring its waiting builds
// with it. They were deliberately kept off the channel while an agent was
// expected to claim them, so without this they would sit pending until the
// next restart happened to re-queue them — a build that never starts and never
// explains why.
func TestSwitchingProjectBackToLocalAdoptsPendingBuilds(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)

	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	if w.Code != 201 {
		t.Fatalf("trigger: %d %s", w.Code, w.Body.String())
	}
	var build models.Build
	decodeJSON(t, w, &build)
	select {
	case <-s.BuildCh:
		t.Fatal("remote build reached the local channel before the switch")
	default:
	}

	if w := doJSON(t, mux, "PUT", fmt.Sprintf("/api/projects/%d", p.ID),
		map[string]interface{}{"executor": "local"}); w.Code != 200 {
		t.Fatalf("switch to local: %d %s", w.Code, w.Body.String())
	}

	select {
	case got := <-s.BuildCh:
		if got.ID != build.ID {
			t.Errorf("queued build %d, want %d", got.ID, build.ID)
		}
	default:
		t.Fatal("pending build was not adopted by the local runner; it would wait for an agent that is never coming")
	}
}

// The other direction needs no adoption — but it must not double-run either.
// The build is already on the channel; an agent asking for the new executor
// races the worker, and the compare-and-swap decides exactly one winner.
func TestSwitchingProjectToRemoteDoesNotDoubleRun(t *testing.T) {
	s, mux := newAgentServer(t)
	p := createProject(t, s, models.Project{
		Name: "web", RepoURL: "https://github.com/u/web", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "web",
	})
	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	if w.Code != 201 {
		t.Fatalf("trigger: %d", w.Code)
	}
	var build models.Build
	decodeJSON(t, w, &build)
	queued := <-s.BuildCh // the local worker has it

	if w := doJSON(t, mux, "PUT", fmt.Sprintf("/api/projects/%d", p.ID),
		map[string]interface{}{"executor": "mac"}); w.Code != 200 {
		t.Fatalf("switch to mac: %d %s", w.Code, w.Body.String())
	}

	// An agent claims it first; the worker's own claim must then fail.
	claimOne(t, mux)
	if ok, err := s.DB.ClaimBuild(queued.ID); err != nil || ok {
		t.Errorf("local worker also claimed build %d (ok=%v err=%v) — it would run twice",
			queued.ID, ok, err)
	}
}

// The scenario the janitor's own mirror cannot cover: the server restarts, an
// operator opens the build page (which creates an EMPTY topic), and the agent
// never comes back. Nothing seeds that topic, so the sweep's explanation lives
// only in the row — and the stream would otherwise end on a bare "failed",
// leaving the operator watching a build go red for no stated reason.
func TestTerminalStatusBackfillsAReasonWrittenOnlyToTheRow(t *testing.T) {
	s, mux := newAgentServer(t)
	p := macProject(t, s)
	build := pending(t, s, p.ID)
	claimOne(t, mux)

	const streamed = "[12:00:00] ##[step:unity] compiling\n"
	agentLog(t, mux, build.ID, "mac-1", 0, streamed)

	// Restart: the topic that held this build's log is gone.
	s.Bus = logbus.New()

	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(fmt.Sprintf("%s/api/builds/%d/events", srv.URL, build.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(60 * time.Millisecond)
		// Exactly what the janitor does to a build whose agent has gone quiet:
		// the reason is appended to the row in SQL, and the bus buffer this
		// subscriber created is empty, so there is nothing to mirror it onto.
		// (Which builds it decides are stale is db.FailStaleRunning's job and
		// is tested there; what matters here is bytes reaching the row without
		// passing through the bus.)
		if err := s.DB.AppendBuildLog(build.ID, db.AgentLostNote); err != nil {
			t.Errorf("append note: %v", err)
		}
		if err := s.DB.FinishBuild(build.ID, models.StatusFailed); err != nil {
			t.Errorf("finish: %v", err)
		}
		s.Bus.PublishStatus(build.ID, models.StatusFailed, nil, nil)
	}()

	deadline := time.AfterFunc(10*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()

	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}
	joined := strings.Join(events, "\n")

	if !strings.Contains(joined, "stopped responding") {
		t.Errorf("stream ended without the reason the build failed:\n%s", joined)
	}
	noteIdx := strings.Index(joined, "stopped responding")
	statusIdx := strings.Index(joined, `"status":"failed"`)
	if statusIdx == -1 {
		t.Fatalf("no terminal status in stream:\n%s", joined)
	}
	if noteIdx > statusIdx {
		t.Error("reason sent after the terminal status; the client closes on status and would drop it")
	}
	// And exactly once — the backfill must not replay what was already sent.
	if n := strings.Count(joined, "compiling"); n != 1 {
		t.Errorf("earlier output appears %d times, want 1", n)
	}
}
