package api

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

func newTestServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	s := &Server{DB: database, BuildCh: make(chan *models.Build, 10), Bus: logbus.New()}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	return s, mux
}

// fakeCanceler records Cancel calls and returns scripted answers.
type fakeCanceler struct {
	got    []int64
	answer bool
	step   string // Progress() step for any id when non-empty
}

func (f *fakeCanceler) Cancel(id int64) bool {
	f.got = append(f.got, id)
	return f.answer
}

func (f *fakeCanceler) Progress(id int64) (string, bool) {
	if f.step == "" {
		return "", false
	}
	return f.step, true
}

func createProject(t *testing.T, s *Server, p models.Project) *models.Project {
	t.Helper()
	if err := s.DB.CreateProject(&p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &p
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("X-Builds-Csrf", "1") // required by state-changing endpoints
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// --- Health ---

func TestHealthReportsVersion(t *testing.T) {
	_, mux := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("health status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %q, want ok", got["status"])
	}
	if got["version"] != Version {
		t.Errorf("version = %q, want %q", got["version"], Version)
	}
}

// --- Projects ---

func TestCreateProjectValidation(t *testing.T) {
	_, mux := newTestServer(t)

	w := doJSON(t, mux, "POST", "/api/projects", map[string]string{"name": "x"})
	if w.Code != 400 {
		t.Errorf("missing fields: got %d, want 400", w.Code)
	}

	w = doJSON(t, mux, "POST", "/api/projects", map[string]string{
		"name": "app", "repo_url": "https://github.com/u/app", "image_name": "app",
	})
	if w.Code != 201 {
		t.Fatalf("create: got %d, want 201: %s", w.Code, w.Body)
	}
	var created models.Project
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Branch != "main" || created.DockerfilePath != "Dockerfile" {
		t.Errorf("defaults not applied: %+v", created)
	}

	// Duplicate name
	w = doJSON(t, mux, "POST", "/api/projects", map[string]string{
		"name": "app", "repo_url": "https://github.com/u/app", "image_name": "app",
	})
	if w.Code != 409 {
		t.Errorf("duplicate: got %d, want 409", w.Code)
	}
}

func TestProjectResponsesOmitSecrets(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		WebhookSecret: "whsec", CloneToken: "tok",
	})

	for _, path := range []string{"/api/projects", fmt.Sprintf("/api/projects/%d", p.ID)} {
		w := doJSON(t, mux, "GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s: got %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "whsec") || strings.Contains(w.Body.String(), "tok") {
			t.Errorf("GET %s leaks secrets: %s", path, w.Body)
		}
	}
}

func TestUpdateProjectPartialAndClear(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		DeployComposePath: "/srv/compose.yml", DeployServiceName: "web",
		WebhookSecret: "whsec", CloneToken: "tok",
	})
	path := fmt.Sprintf("/api/projects/%d", p.ID)

	// Omitted fields must be preserved.
	w := doJSON(t, mux, "PUT", path, map[string]string{"branch": "develop"})
	if w.Code != 200 {
		t.Fatalf("update: got %d: %s", w.Code, w.Body)
	}
	got, _ := s.DB.GetProject(p.ID)
	if got.Branch != "develop" {
		t.Errorf("branch not updated: %q", got.Branch)
	}
	if got.DeployComposePath != "/srv/compose.yml" || got.WebhookSecret != "whsec" || got.CloneToken != "tok" {
		t.Errorf("omitted fields were modified: %+v", got)
	}

	// Explicit empty strings clear the deploy config.
	w = doJSON(t, mux, "PUT", path, map[string]string{
		"deploy_compose_path": "", "deploy_service_name": "",
	})
	if w.Code != 200 {
		t.Fatalf("clear: got %d: %s", w.Code, w.Body)
	}
	got, _ = s.DB.GetProject(p.ID)
	if got.DeployComposePath != "" || got.DeployServiceName != "" {
		t.Errorf("deploy config not cleared: %+v", got)
	}

	// Required fields cannot be cleared.
	w = doJSON(t, mux, "PUT", path, map[string]string{"name": ""})
	if w.Code != 400 {
		t.Errorf("clearing name: got %d, want 400", w.Code)
	}
}

// --- Builds ---

func TestTriggerBuild(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	if w.Code != 201 {
		t.Fatalf("trigger: got %d: %s", w.Code, w.Body)
	}
	select {
	case b := <-s.BuildCh:
		if b.ProjectID != p.ID || b.Status != models.StatusPending {
			t.Errorf("queued build wrong: %+v", b)
		}
	default:
		t.Error("build was not queued")
	}

	w = doJSON(t, mux, "POST", "/api/projects/999/build", nil)
	if w.Code != 404 {
		t.Errorf("unknown project: got %d, want 404", w.Code)
	}

	// Trigger requires the CSRF header like cancel/rerun.
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("trigger without csrf header: got %d, want 403", rec.Code)
	}
}

func TestTriggerBuildQueueFull(t *testing.T) {
	s, mux := newTestServer(t)
	s.BuildCh = make(chan *models.Build) // unbuffered, nothing reading = always full
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	if w.Code != 503 {
		t.Fatalf("full queue: got %d, want 503: %s", w.Code, w.Body)
	}
	// The created build must be marked failed, not left pending forever.
	builds, _ := s.DB.ListBuildsByProject(p.ID, 0)
	if len(builds) != 1 || builds[0].Status != models.StatusFailed {
		t.Errorf("build not marked failed: %+v", builds)
	}
}

// --- Webhook ---

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushPayload(t *testing.T, ref, cloneURL, sha, msg string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"ref":         ref,
		"repository":  map[string]string{"clone_url": cloneURL, "full_name": "u/app"},
		"head_commit": map[string]string{"id": sha, "message": msg},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postWebhook(mux *http.ServeMux, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestWebhookSignature(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app.git", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app", WebhookSecret: "s3cret",
	})

	body := pushPayload(t, "refs/heads/main", "https://github.com/u/app.git",
		"0123456789abcdef0123456789abcdef01234567", "fix stuff")

	// Missing signature → rejected
	w := postWebhook(mux, body, nil)
	if w.Code != 403 {
		t.Errorf("missing signature: got %d, want 403: %s", w.Code, w.Body)
	}

	// Wrong signature → rejected
	w = postWebhook(mux, body, map[string]string{"X-Hub-Signature-256": sign("wrong", body)})
	if w.Code != 403 {
		t.Errorf("bad signature: got %d, want 403: %s", w.Code, w.Body)
	}
	if builds, _ := s.DB.ListBuildsByProject(p.ID, 0); len(builds) != 0 {
		t.Errorf("rejected webhook still created builds: %+v", builds)
	}

	// Valid signature → queued
	w = postWebhook(mux, body, map[string]string{"X-Hub-Signature-256": sign("s3cret", body)})
	if w.Code != 200 {
		t.Fatalf("valid signature: got %d: %s", w.Code, w.Body)
	}
	builds, _ := s.DB.ListBuildsByProject(p.ID, 0)
	if len(builds) != 1 {
		t.Fatalf("expected 1 build, got %d", len(builds))
	}
	if builds[0].CommitSHA != "0123456789ab" {
		t.Errorf("sha not truncated to 12: %q", builds[0].CommitSHA)
	}
	select {
	case <-s.BuildCh:
	default:
		t.Error("build not queued on channel")
	}
}

func TestWebhookNoSecretConfigured(t *testing.T) {
	s, mux := newTestServer(t)
	createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	body := pushPayload(t, "refs/heads/main", "https://github.com/u/app.git",
		"0123456789abcdef0123456789abcdef01234567", "hello")
	w := postWebhook(mux, body, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "queued") {
		t.Errorf("no-secret project should accept push: got %d: %s", w.Code, w.Body)
	}
}

func TestWebhookNoHeadCommit(t *testing.T) {
	_, mux := newTestServer(t)

	// Branch deletion pushes have "head_commit": null — this used to panic.
	body := []byte(`{"ref":"refs/heads/main","repository":{"clone_url":"https://github.com/u/app.git"},"head_commit":null,"deleted":true}`)
	w := postWebhook(mux, body, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "no head_commit") {
		t.Errorf("null head_commit: got %d: %s", w.Code, w.Body)
	}

	// Short SHA must not panic either.
	body = pushPayload(t, "refs/heads/main", "https://github.com/u/app.git", "abc", "short")
	w = postWebhook(mux, body, nil)
	if w.Code != 200 {
		t.Errorf("short sha: got %d: %s", w.Code, w.Body)
	}
}

func TestWebhookIgnoresNonPushAndOtherBranch(t *testing.T) {
	s, mux := newTestServer(t)
	createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	body := pushPayload(t, "refs/heads/feature", "https://github.com/u/app.git",
		"0123456789abcdef0123456789abcdef01234567", "wip")
	w := postWebhook(mux, body, nil)
	if !strings.Contains(w.Body.String(), "no matching project") {
		t.Errorf("other branch should be ignored: %s", w.Body)
	}

	req := httptest.NewRequest("POST", "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "ping")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("ping event should be ignored: %s", rec.Body)
	}
	if builds, _ := s.DB.ListRecentBuilds(10); len(builds) != 0 {
		t.Errorf("ignored events created builds: %+v", builds)
	}
}

// --- Helpers ---

func TestRepoURLMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://github.com/u/app", "https://github.com/u/app.git", true},
		{"http://github.com/u/app.git", "https://github.com/u/app", true},
		{"https://github.com/u/app", "https://github.com/u/other.git", false},
	}
	for _, c := range cases {
		if got := repoURLMatch(c.a, c.b); got != c.want {
			t.Errorf("repoURLMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 100, "short"},
		{"first line\nsecond line", 100, "first line"},
		{strings.Repeat("a", 10), 8, "aaaaa..."},
		// Multi-byte runes must not be split mid-character.
		{strings.Repeat("é", 10), 8, "ééééé..."},
		{strings.Repeat("日", 10), 8, "日日日日日..."},
		{"héllo", 2, "hé"},
	}
	for _, c := range cases {
		got := truncate(c.in, c.n)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if !validSignature("secret", body, sign("secret", body)) {
		t.Error("valid signature rejected")
	}
	if validSignature("secret", body, sign("other", body)) {
		t.Error("invalid signature accepted")
	}
	if validSignature("secret", body, "") {
		t.Error("empty signature accepted")
	}
}

// --- New build-page endpoints ---

func seedBuild(t *testing.T, s *Server, status models.BuildStatus, log string) (*models.Project, *models.Build) {
	t.Helper()
	p, err := s.DB.GetProjectByName("app")
	if err != nil {
		p = createProject(t, s, models.Project{
			Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
			DockerfilePath: "Dockerfile", ImageName: "app",
		})
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc123def456", CommitMessage: "msg"}
	if err := s.DB.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if status != models.StatusPending {
		if err := s.DB.UpdateBuildStatus(b.ID, status, log); err != nil {
			t.Fatal(err)
		}
	} else if log != "" {
		s.DB.AppendBuildLog(b.ID, log)
	}
	b.Status = status
	return p, b
}

func TestGetBuildMeta(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusPending, "hello log\n")

	w := doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d?meta=1", b.ID), nil)
	if w.Code != 200 {
		t.Fatalf("meta: got %d", w.Code)
	}
	var got models.Build
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Log != "" {
		t.Errorf("meta response must omit log, got %q", got.Log)
	}
	if got.LogLen != int64(len("hello log\n")) {
		t.Errorf("log_len = %d, want %d", got.LogLen, len("hello log\n"))
	}
	if got.QueuePosition < 1 {
		t.Errorf("queue_position = %d, want >= 1 for pending", got.QueuePosition)
	}

	// Non-meta keeps the log.
	w = doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d", b.ID), nil)
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Log == "" {
		t.Error("plain GET should include log")
	}
}

func TestBuildLogEndpoint(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusSuccess, "0123456789")

	w := doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log", b.ID), nil)
	if w.Code != 200 || w.Body.String() != "0123456789" {
		t.Fatalf("full log: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Log-Offset") != "10" || w.Header().Get("X-Build-Status") != "success" {
		t.Errorf("headers: offset=%q status=%q", w.Header().Get("X-Log-Offset"), w.Header().Get("X-Build-Status"))
	}

	// Incremental tail.
	w = doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log?offset=6", b.ID), nil)
	if w.Body.String() != "6789" {
		t.Errorf("offset tail = %q, want 6789", w.Body.String())
	}
	// Offset beyond end → empty, not error.
	w = doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log?offset=99", b.ID), nil)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("beyond-end: %d %q", w.Code, w.Body.String())
	}
	// Bad offset → 400.
	w = doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log?offset=-1", b.ID), nil)
	if w.Code != 400 {
		t.Errorf("negative offset: got %d, want 400", w.Code)
	}
	// Download disposition.
	w = doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log?download=1", b.ID), nil)
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, fmt.Sprintf("build-%d.log", b.ID)) {
		t.Errorf("disposition = %q", cd)
	}

	// Live bus buffer is preferred over the (stale) DB row.
	_, b2 := seedBuild(t, s, models.StatusRunning, "db-old")
	s.Bus.Publish(b2.ID, []byte("bus-fresh-content"))
	w = doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log", b2.ID), nil)
	if w.Body.String() != "bus-fresh-content" {
		t.Errorf("live log = %q, want bus content", w.Body.String())
	}
}

func TestCancelEndpoint(t *testing.T) {
	s, mux := newTestServer(t)
	rn := &fakeCanceler{}
	s.Runner = rn

	// Missing CSRF header → 403.
	_, pending := seedBuild(t, s, models.StatusPending, "")
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/builds/%d/cancel", pending.ID), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("no csrf: got %d, want 403", w.Code)
	}

	csrfPost := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("X-Builds-Csrf", "1")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// Pending → tombstoned directly, 200.
	w = csrfPost(fmt.Sprintf("/api/builds/%d/cancel", pending.ID))
	if w.Code != 200 {
		t.Fatalf("cancel pending: got %d: %s", w.Code, w.Body)
	}
	got, _ := s.DB.GetBuild(pending.ID)
	if got.Status != models.StatusCanceled || got.FinishedAt == nil {
		t.Errorf("pending not canceled: %+v", got)
	}
	if len(rn.got) != 0 {
		t.Errorf("runner should not be consulted for pending builds")
	}
	// The tombstone the SQL appended must be mirrored on the bus so live
	// subscribers and the DB row stay byte-identical.
	if tail, _, ok := s.Bus.LogTail(pending.ID, 0); !ok || !strings.Contains(string(tail), "[canceled while queued]") {
		t.Errorf("tombstone not mirrored to bus: ok=%v tail=%q", ok, tail)
	}
	if !strings.Contains(got.Log, "[canceled while queued]") {
		t.Errorf("tombstone missing from DB log: %q", got.Log)
	}

	// Running + runner accepts → 202.
	rn.answer = true
	_, running := seedBuild(t, s, models.StatusRunning, "")
	w = csrfPost(fmt.Sprintf("/api/builds/%d/cancel", running.ID))
	if w.Code != 202 {
		t.Fatalf("cancel running: got %d: %s", w.Code, w.Body)
	}
	if len(rn.got) != 1 || rn.got[0] != running.ID {
		t.Errorf("runner.Cancel calls = %v", rn.got)
	}

	// Finished → 409.
	rn.answer = false
	_, doneB := seedBuild(t, s, models.StatusSuccess, "")
	w = csrfPost(fmt.Sprintf("/api/builds/%d/cancel", doneB.ID))
	if w.Code != 409 {
		t.Errorf("cancel finished: got %d, want 409", w.Code)
	}
	// Already canceled → 200 with already flag.
	w = csrfPost(fmt.Sprintf("/api/builds/%d/cancel", pending.ID))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "already") {
		t.Errorf("re-cancel: %d %s", w.Code, w.Body)
	}
	// Unknown → 404.
	w = csrfPost("/api/builds/99999/cancel")
	if w.Code != 404 {
		t.Errorf("unknown: got %d, want 404", w.Code)
	}
}

func TestRerunEndpoint(t *testing.T) {
	s, mux := newTestServer(t)
	_, src := seedBuild(t, s, models.StatusFailed, "old log")

	// CSRF required.
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/builds/%d/rerun", src.ID), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("no csrf: got %d", w.Code)
	}

	req = httptest.NewRequest("POST", fmt.Sprintf("/api/builds/%d/rerun", src.ID), nil)
	req.Header.Set("X-Builds-Csrf", "1")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("rerun: got %d: %s", w.Code, w.Body)
	}
	var nb models.Build
	json.Unmarshal(w.Body.Bytes(), &nb)
	if nb.ID == src.ID || nb.CommitSHA != src.CommitSHA || nb.Status != models.StatusPending {
		t.Errorf("new build wrong: %+v", nb)
	}
	if !strings.Contains(nb.CommitMessage, fmt.Sprintf("Re-run of #%d", src.ID)) {
		t.Errorf("commit message = %q", nb.CommitMessage)
	}
	select {
	case q := <-s.BuildCh:
		if q.ID != nb.ID {
			t.Errorf("queued %d, want %d", q.ID, nb.ID)
		}
	default:
		t.Error("rerun build not queued")
	}
}

// SSE: a finished build replays its log from the requested offset and closes.
func TestSSEFinishedBuildReplaysAndCloses(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusSuccess, "0123456789")

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(fmt.Sprintf("%s/api/builds/%d/events?offset=4", srv.URL, b.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body) // returns because handler closes
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"status":"success"`) {
		t.Errorf("missing terminal status event:\n%s", text)
	}
	if !strings.Contains(text, `"t":"456789"`) {
		t.Errorf("missing offset replay:\n%s", text)
	}
}

// SSE: a running build streams chunks live and closes on terminal status.
func TestSSELiveStream(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusRunning, "")

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(fmt.Sprintf("%s/api/builds/%d/events", srv.URL, b.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		s.Bus.Publish(b.ID, []byte("chunk-one\n"))
		s.Bus.Publish(b.ID, []byte("chunk-two\n"))
		now := time.Now().UTC()
		s.DB.FinishBuild(b.ID, models.StatusSuccess)
		s.Bus.PublishStatus(b.ID, models.StatusSuccess, &now, &now)
	}()

	deadline := time.AfterFunc(10*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()

	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "chunk-one") || !strings.Contains(joined, "chunk-two") {
		t.Errorf("missing live chunks:\n%s", joined)
	}
	if !strings.Contains(joined, `"status":"success"`) {
		t.Errorf("missing terminal status:\n%s", joined)
	}
}

func TestSSEUnknownBuild(t *testing.T) {
	_, mux := newTestServer(t)
	w := doJSON(t, mux, "GET", "/api/builds/424242/events", nil)
	if w.Code != 404 {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// Terminal SSE responses must write log bytes BEFORE the terminal status
// event (the client closes its EventSource on terminal status), and must not
// create a logbus topic for a build whose run is long over.
func TestSSETerminalOrderingAndNoTopicLeak(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusSuccess, "0123456789")

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(fmt.Sprintf("%s/api/builds/%d/events", srv.URL, b.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	logIdx := strings.Index(text, `"t":"0123456789"`)
	statusIdx := strings.Index(text, `"status":"success"`)
	if logIdx == -1 || statusIdx == -1 {
		t.Fatalf("missing events:\n%s", text)
	}
	if logIdx > statusIdx {
		t.Errorf("log event written after terminal status (client would drop it):\n%s", text)
	}

	// No topic may have been created for the finished build.
	if _, _, ok := s.Bus.LogTail(b.ID, 0); ok {
		t.Error("SSE on a terminal build leaked a logbus topic")
	}
	if s.Bus.Live(b.ID) {
		t.Error("terminal build reported live after SSE request")
	}
}

func TestAsciiFilename(t *testing.T) {
	cases := map[string]string{
		"app":         "app",
		"héllo→wörld": "hllowrld",
		`a"b\c`:       "abc", // quotes and backslashes stripped
		"日本語":         "build",
		"my_app-2.0":  "my_app-2.0",
	}
	for in, want := range cases {
		if got := asciiFilename(in); got != want {
			t.Errorf("asciiFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestActiveBuildsEndpoint(t *testing.T) {
	s, mux := newTestServer(t)
	s.Runner = &fakeCanceler{step: "push"}
	_, running := seedBuild(t, s, models.StatusRunning, "some log text")
	_, pend1 := seedBuild(t, s, models.StatusPending, "")
	_, pend2 := seedBuild(t, s, models.StatusPending, "")
	seedBuild(t, s, models.StatusSuccess, "done") // terminal: must not appear

	w := doJSON(t, mux, "GET", "/api/builds/active", nil)
	if w.Code != 200 {
		t.Fatalf("active: got %d: %s", w.Code, w.Body)
	}
	var list []models.Build
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 3 {
		t.Fatalf("expected 3 active builds, got %d: %s", len(list), w.Body)
	}
	byID := map[int64]models.Build{}
	for _, b := range list {
		byID[b.ID] = b
		if b.Log != "" {
			t.Errorf("active list leaked a log body for build %d", b.ID)
		}
	}
	if got := byID[running.ID]; got.Status != models.StatusRunning || got.CurrentStep != "push" {
		t.Errorf("running build wrong: %+v", got)
	}
	if got := byID[pend1.ID]; got.QueuePosition < 2 {
		t.Errorf("first pending queue_position = %d, want >= 2 (running ahead)", got.QueuePosition)
	}
	if a, b := byID[pend1.ID].QueuePosition, byID[pend2.ID].QueuePosition; b != a+1 {
		t.Errorf("queue positions not consecutive: %d then %d", a, b)
	}
}

func TestGetBuildMetaIncludesProgress(t *testing.T) {
	s, mux := newTestServer(t)
	s.Runner = &fakeCanceler{step: "build"}
	_, running := seedBuild(t, s, models.StatusRunning, "")

	w := doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d?meta=1", running.ID), nil)
	var got models.Build
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.CurrentStep != "build" {
		t.Errorf("current_step = %q, want build", got.CurrentStep)
	}
}

// Project create/update/delete are state-changing and must require the
// CSRF header like every other mutating endpoint.
func TestProjectCRUDRequiresCsrf(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	cases := []struct{ method, path string }{
		{"POST", "/api/projects"},
		{"PUT", fmt.Sprintf("/api/projects/%d", p.ID)},
		{"DELETE", fmt.Sprintf("/api/projects/%d", p.ID)},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("%s %s without csrf: got %d, want 403", c.method, c.path, w.Code)
		}
	}
	// Project must have survived the rejected DELETE.
	if _, err := s.DB.GetProject(p.ID); err != nil {
		t.Error("project deleted despite missing csrf header")
	}
}

func TestUpdateProjectPollSettings(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})
	path := fmt.Sprintf("/api/projects/%d", p.ID)

	w := doJSON(t, mux, "PUT", path, map[string]interface{}{
		"poll_enabled": true, "poll_interval_secs": 120,
	})
	if w.Code != 200 {
		t.Fatalf("enable polling: got %d: %s", w.Code, w.Body)
	}
	got, _ := s.DB.GetProject(p.ID)
	if !got.PollEnabled || got.PollIntervalSecs != 120 {
		t.Errorf("poll settings not persisted: %+v", got)
	}

	// Below the floor is rejected rather than silently clamped, so the UI can
	// say why.
	w = doJSON(t, mux, "PUT", path, map[string]interface{}{"poll_interval_secs": 1})
	if w.Code != 400 {
		t.Errorf("sub-minimum interval: got %d, want 400", w.Code)
	}
	got, _ = s.DB.GetProject(p.ID)
	if got.PollIntervalSecs != 120 {
		t.Errorf("rejected update still applied: %d", got.PollIntervalSecs)
	}

	// An unrelated edit must not disturb the poller's own bookkeeping.
	if err := s.DB.UpdatePollState(p.ID, "abc123abc123", ""); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, mux, "PUT", path, map[string]string{"image_name": "app2"}); w.Code != 200 {
		t.Fatalf("rename image: got %d: %s", w.Code, w.Body)
	}
	got, _ = s.DB.GetProject(p.ID)
	if got.LastPolledSHA != "abc123abc123" {
		t.Errorf("settings save clobbered the poll baseline: %q", got.LastPolledSHA)
	}

	// Changing the watched ref must reset the baseline: the stored SHA belongs
	// to the old branch and comparing it against the new tip would build a
	// commit that isn't new.
	if w := doJSON(t, mux, "PUT", path, map[string]string{"branch": "develop"}); w.Code != 200 {
		t.Fatalf("change branch: got %d: %s", w.Code, w.Body)
	}
	got, _ = s.DB.GetProject(p.ID)
	if got.LastPolledSHA != "" || got.LastPolledAt != nil {
		t.Errorf("poll baseline survived a branch change: %+v", got)
	}
}
