package notifications

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

const testReceiptID = "0123456789abcdef0123456789abcdef"

type submission struct{ Key, Body string }
type relayFixture struct {
	mu                  sync.Mutex
	route               string
	routeCode, postCode int
	retry               string
	response            string
	deliveryStatus      string
	posts               []submission
	routeCalls          int
	onRoute             func()
	onPost              func()
}

func (f *relayFixture) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer relay-test-key" {
		http.Error(w, "bad key", 401)
		return
	}
	switch {
	case r.URL.Path == "/v1/route":
		f.routeCalls++
		if f.onRoute != nil {
			f.onRoute()
		}
		if f.retry != "" {
			w.Header().Set("Retry-After", f.retry)
		}
		code := f.routeCode
		if code == 0 {
			code = 200
		}
		w.WriteHeader(code)
		route := f.route
		if route == "" {
			route = `{"configured":true,"group":true,"preserve_title":false}`
		}
		io.WriteString(w, route)
	case r.Method == "POST" && r.URL.Path == "/v1/notifications":
		if f.onPost != nil {
			f.onPost()
		}
		body, _ := io.ReadAll(r.Body)
		f.posts = append(f.posts, submission{r.Header.Get("Idempotency-Key"), string(body)})
		code := f.postCode
		if code == 0 {
			code = 202
		}
		if f.retry != "" {
			w.Header().Set("Retry-After", f.retry)
		}
		w.WriteHeader(code)
		response := f.response
		if response == "" {
			response = `{"id":"` + testReceiptID + `","status":"pending"}`
		}
		io.WriteString(w, response)
	case r.URL.Path == "/v1/notifications/"+testReceiptID:
		status := f.deliveryStatus
		if status == "" {
			status = "sent"
		}
		io.WriteString(w, `{"id":"`+testReceiptID+`","status":"`+status+`"}`)
	default:
		http.NotFound(w, r)
	}
}
func (f *relayFixture) change(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }
func (f *relayFixture) snapshot() ([]submission, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]submission{}, f.posts...), f.routeCalls
}
func workerDB(t *testing.T) (*db.DB, string, *models.Project) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	p := &models.Project{Name: "Human Project", RepoURL: "https://private.example/repo", Branch: "main", ImageName: "test", TelegramNotifications: true}
	if err := d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	return d, path, p
}
func completed(t *testing.T, d *db.DB, p *models.Project, status models.BuildStatus) *models.Build {
	t.Helper()
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "0123456789abcdef"}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimBuild(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishBuild(b.ID, status); err != nil {
		t.Fatal(err)
	}
	return b
}
func fixtureWorker(t *testing.T, d *db.DB, f *relayFixture) (*Worker, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(f.handler))
	t.Cleanup(server.Close)
	w, err := New(d, agents.NewRegistry(), Config{URL: server.URL, Key: "relay-test-key", PublicURL: "https://public.example/builds", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return w, server
}
func sweep(t *testing.T, w *Worker, now time.Time) {
	t.Helper()
	if err := w.Sweep(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}

func TestRelayRetryRestartExactPayloadAndDelivery(t *testing.T) {
	d, path, p := workerDB(t)
	f := &relayFixture{postCode: 429, retry: "120"}
	w, server := fixtureWorker(t, d, f)
	// Separate global ID and human project build number.
	other := &models.Project{Name: "Other", RepoURL: "x", ImageName: "other"}
	if err := d.CreateProject(other); err != nil {
		t.Fatal(err)
	}
	completed(t, d, other, models.StatusSuccess)
	b := completed(t, d, p, models.StatusSuccess)
	now := time.Now().Add(time.Second)
	sweep(t, w, now)
	posts, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatal(posts)
	}
	var content struct{ Text, Format string }
	if err := json.Unmarshal([]byte(posts[0].Body), &content); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(content.Text, "**✅ Human Project #1 — SUCCEEDED**\n") || content.Format != "markdown" {
		t.Fatal(content)
	}
	for _, want := range []string{"Agent: Local runner", "Queue: local", "Branch: main", "Commit: 0123456789ab", "Duration:", "https://public.example/builds/builds/2"} {
		if !strings.Contains(content.Text, want) {
			t.Errorf("missing %q in %s", want, content.Text)
		}
	}
	due, err := d.DueNotifications(now.Add(119*time.Second), 1)
	if err != nil || len(due) != 0 {
		t.Fatal("ignored retry-after", due, err)
	}
	due, err = d.DueNotifications(now.Add(120*time.Second), 1)
	if err != nil || len(due) != 1 {
		t.Fatal(due, err)
	}
	id := due[0].ID
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p.Name = "Changed after failure"
	p.Branch = "different"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	w, err = New(d, agents.NewRegistry(), Config{URL: server.URL, Key: "relay-test-key", PublicURL: "https://different.example", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	f.change(func() { f.postCode = 202; f.retry = "" })
	sweep(t, w, now.Add(119*time.Second))
	posts, _ = f.snapshot()
	if len(posts) != 1 {
		t.Fatal("early retry")
	}
	sweep(t, w, now.Add(120*time.Second))
	posts, _ = f.snapshot()
	if len(posts) != 2 || posts[0] != posts[1] {
		t.Fatalf("payload/key changed across retry: %#v", posts)
	}
	n, err := d.GetNotification(id)
	if err != nil || n.State != "submitted" || n.RelayID != testReceiptID {
		t.Fatal(n, err)
	}
	sweep(t, w, now.Add(151*time.Second))
	n, err = d.GetNotification(id)
	if err != nil || n.State != "delivered" {
		t.Fatal(n, err)
	}
	sweep(t, w, now.Add(time.Hour))
	posts, _ = f.snapshot()
	if len(posts) != 2 {
		t.Fatal("delivered event resubmitted")
	}
	if b.Number != 1 || b.ID != 2 {
		t.Fatal(b)
	}
}

func TestProjectMuteUnmuteNeverRevivesQueuedIntent(t *testing.T) {
	d, _, p := workerDB(t)
	f := &relayFixture{}
	w, _ := fixtureWorker(t, d, f)
	completed(t, d, p, models.StatusFailed)
	now := time.Now().Add(time.Second)
	due, err := d.DueNotifications(now, 1)
	if err != nil || len(due) != 1 {
		t.Fatal(due, err)
	}
	stale := due[0]
	p.TelegramNotifications = false
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	p.TelegramNotifications = true
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	// Process an already-fetched pending snapshot after quick mute/unmute.
	if err := w.process(context.Background(), now, stale); err != nil {
		t.Fatal(err)
	}
	sweep(t, w, now)
	posts, _ := f.snapshot()
	if len(posts) != 0 {
		t.Fatal("old intent revived", posts)
	}
	n, err := d.GetNotification(stale.ID)
	if err != nil || n.State != "muted" {
		t.Fatal(n, err)
	}
	completed(t, d, p, models.StatusCanceled)
	sweep(t, w, now.Add(time.Second))
	posts, _ = f.snapshot()
	if len(posts) != 1 || !strings.Contains(posts[0].Body, "❌ Human Project #2 — CANCELED") {
		t.Fatal(posts)
	}
}

func TestRouteFailClosedAndExplicitDisableSuppressesBacklog(t *testing.T) {
	for _, route := range []string{`{"configured":true,"group":false,"preserve_title":false}`, `{"configured":false,"group":true,"preserve_title":false}`, `{"configured":true,"group":true,"preserve_title":true}`, `{"configured":true,"group":true}`, `{}`} {
		t.Run(route, func(t *testing.T) {
			d, _, p := workerDB(t)
			f := &relayFixture{route: route}
			w, _ := fixtureWorker(t, d, f)
			completed(t, d, p, models.StatusSuccess)
			now := time.Now().Add(time.Second)
			sweep(t, w, now)
			if w.Configured() {
				t.Fatal("denied route configured")
			}
			posts, _ := f.snapshot()
			if len(posts) != 0 {
				t.Fatal(posts)
			}
			// Work ending while setup is disabled is observed, not accumulated.
			completed(t, d, p, models.StatusFailed)
			f.change(func() { f.route = `{"configured":true,"group":true,"preserve_title":false}` })
			sweep(t, w, now.Add(time.Minute))
			posts, _ = f.snapshot()
			if len(posts) != 0 {
				t.Fatal("initial setup replayed backlog", posts)
			}
			completed(t, d, p, models.StatusSuccess)
			sweep(t, w, now.Add(61*time.Second))
			posts, _ = f.snapshot()
			if len(posts) != 1 {
				t.Fatal("new event not sent", posts)
			}
		})
	}
}

func TestRoute429DurableBackoffRetainsOutageEvents(t *testing.T) {
	d, path, p := workerDB(t)
	f := &relayFixture{routeCode: 429, retry: "300"}
	w, server := fixtureWorker(t, d, f)
	completed(t, d, p, models.StatusSuccess)
	now := time.Now().Add(time.Second)
	sweep(t, w, now)
	posts, calls := f.snapshot()
	if len(posts) != 0 || calls != 1 {
		t.Fatal(posts, calls)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	w, err = New(d, agents.NewRegistry(), Config{URL: server.URL, Key: "relay-test-key", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	f.change(func() { f.routeCode = 200; f.retry = "" })
	sweep(t, w, now.Add(299*time.Second))
	posts, calls = f.snapshot()
	if len(posts) != 0 || calls != 1 {
		t.Fatal("route Retry-After lost at restart", posts, calls)
	}
	sweep(t, w, now.Add(300*time.Second))
	posts, _ = f.snapshot()
	if len(posts) != 1 {
		t.Fatal("outage event lost", posts)
	}
}

func TestMalformedReceiptsRetryInsteadOfClaimingDelivery(t *testing.T) {
	for _, response := range []string{`{"id":"short","status":"pending"}`, `{"id":"` + testReceiptID + `"}`, `{"id":"` + testReceiptID + `","status":"delivered"}`, `{"id":"ABCDEF0123456789abcdef0123456789ab","status":"sent"}`} {
		t.Run(response, func(t *testing.T) {
			d, _, p := workerDB(t)
			f := &relayFixture{response: response}
			w, _ := fixtureWorker(t, d, f)
			completed(t, d, p, models.StatusFailed)
			now := time.Now().Add(time.Second)
			sweep(t, w, now)
			due, err := d.DueNotifications(now.Add(10*time.Second), 1)
			if err != nil || len(due) != 1 || due[0].State != "pending" || due[0].RelayID != "" {
				t.Fatal(due, err)
			}
			f.change(func() { f.response = "" })
			sweep(t, w, now.Add(10*time.Second))
			posts, _ := f.snapshot()
			if len(posts) != 2 || posts[0] != posts[1] {
				t.Fatal(posts)
			}
		})
	}
}

func TestDisabledKeyNoRequestsOrLaterReplayAndSubmittedStillTracked(t *testing.T) {
	d, _, p := workerDB(t)
	f := &relayFixture{}
	w, server := fixtureWorker(t, d, f)
	completed(t, d, p, models.StatusSuccess)
	now := time.Now().Add(time.Second)
	sweep(t, w, now)
	due, err := d.DueNotifications(now.Add(time.Minute), 1)
	if err != nil || len(due) != 1 {
		t.Fatal(due, err)
	}
	id := due[0].ID
	p.TelegramNotifications = false
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	// Muting does not pretend a previously accepted request can be recalled.
	sweep(t, w, now.Add(time.Minute))
	n, err := d.GetNotification(id)
	if err != nil || n.State != "delivered" {
		t.Fatal(n, err)
	}
	p.TelegramNotifications = true
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	completed(t, d, p, models.StatusSuccess)
	disabled, err := New(d, agents.NewRegistry(), Config{URL: server.URL, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, before := f.snapshot()
	sweep(t, disabled, now.Add(61*time.Second))
	_, after := f.snapshot()
	if before != after || disabled.Configured() {
		t.Fatal("disabled worker made request")
	}
	completed(t, d, p, models.StatusSuccess)
	sweep(t, w, now.Add(62*time.Second))
	posts, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatal("disabled backlog replayed", posts)
	}
}

func TestRedirectsRefusedHTTPSAndBoundedRequests(t *testing.T) {
	d, _, p := workerDB(t)
	var mu sync.Mutex
	destinationCalls := 0
	dest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); destinationCalls++; mu.Unlock() }))
	defer dest.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 302) }))
	defer redirect.Close()
	w, err := New(d, agents.NewRegistry(), Config{URL: redirect.URL, Key: "relay-test-key", Client: redirect.Client()})
	if err != nil {
		t.Fatal(err)
	}
	completed(t, d, p, models.StatusSuccess)
	sweep(t, w, time.Now().Add(time.Second))
	mu.Lock()
	calls := destinationCalls
	mu.Unlock()
	if calls != 0 || w.Configured() {
		t.Fatal("redirect followed")
	}
	if w.client.Timeout != 5*time.Second {
		t.Fatal("unbounded HTTP timeout")
	}
	for _, bad := range []string{"http://notify.example", "https://user:pass@notify.example", "https://notify.example/path", "https://notify.example/?token=x", "https://notify.example/#x"} {
		if _, err := New(d, nil, Config{URL: bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRawMetadataScrubBeforeUTF16BoundAndActualOwnerWins(t *testing.T) {
	d, _, p := workerDB(t)
	f := &relayFixture{}
	w, _ := fixtureWorker(t, d, f)
	secret := "arbitraryProjectToken_012345678901234567890123456789"
	p.CloneToken = secret
	p.WebhookSecret = "another_secret_abcdefghijklmnop"
	p.Name = strings.Repeat("a", 110) + secret
	p.Branch = strings.Repeat("b", 170) + secret + "\n**danger** ftp://private.example/repo //private.example/repo"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	w.secrets = append(w.secrets, "server_secret_abcdefghijklmnopqrstuvwxyz")
	b := completed(t, d, p, models.StatusFailed)
	// Actual ownership remains truthful even if the current project queue is local.
	event := buildEvent{ID: b.ID, Number: b.Number, Project: p.Name, Executor: "local", Agent: "actual-machine", Status: models.StatusFailed}
	if !strings.Contains(w.buildText(event), "Agent: actual-machine") {
		t.Fatal("recorded remote agent replaced by current queue")
	}
	sweep(t, w, time.Now().Add(time.Second))
	posts, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatal(posts)
	}
	var content struct{ Text, Format string }
	if err := json.Unmarshal([]byte(posts[0].Body), &content); err != nil {
		t.Fatal(err)
	}
	for _, notWant := range []string{secret, secret[:10], "private.example", "**danger**", "another_secret"} {
		if strings.Contains(content.Text, notWant) {
			t.Errorf("leaked %q in %s", notWant, content.Text)
		}
	}
	if !strings.HasPrefix(content.Text, "**❌ ") || !strings.Contains(strings.Split(content.Text, "\n")[0], " — FAILED**") {
		t.Fatal("bold heading corrupted", content.Text)
	}
	if len(utf16.Encode([]rune(content.Text))) > 3300 {
		t.Fatal("message too long")
	}
	// The capture cap can cut a secret. A partial suffix is still masked before
	// renderer limits, and raw metadata scrub does not lose doctor remedy chars.
	if got := w.scrub(*p, strings.Repeat("x", 240)+secret[:16]); strings.Contains(got, secret[:16]) {
		t.Fatal("captured secret prefix leaked", got)
	}
	if got := cleanPlain("brew install git-lfs; check agent_name and `/opt/homebrew/bin`", 500); !strings.Contains(got, "agent_name") || !strings.Contains(got, "`/opt/homebrew/bin`") {
		t.Fatal("doctor remedy rewritten", got)
	}
}

func TestFromEnvServerOnlyKeyFile(t *testing.T) {
	d, _, _ := workerDB(t)
	t.Setenv("BUILDS_TELEGRAM_KEY_FILE", "")
	t.Setenv("BUILDS_TELEGRAM_URL", "")
	t.Setenv("BUILDS_PUBLIC_URL", "")
	w, err := FromEnv(d, nil)
	if err != nil || w.key != "" || w.Configured() {
		t.Fatal(w, err)
	}
	keyPath := filepath.Join(t.TempDir(), "relay.key")
	if err := os.WriteFile(keyPath, []byte("relay-test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDS_TELEGRAM_KEY_FILE", keyPath)
	w, err = FromEnv(d, nil)
	if err != nil || w.key != "relay-test-key" || w.Configured() {
		t.Fatal("key alone incorrectly configured", err)
	}
	t.Setenv("BUILDS_TELEGRAM_KEY_FILE", keyPath+"-missing")
	_, err = FromEnv(d, nil)
	if err == nil || strings.Contains(err.Error(), keyPath) {
		t.Fatal("key path exposed", err)
	}
}

func TestSweepStopsPromptlyOnCancellation(t *testing.T) {
	d, _, _ := workerDB(t)
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer server.Close()
	w, err := New(d, nil, Config{URL: server.URL, Key: "relay-test-key", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Sweep(ctx, time.Now()) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notification request held shutdown open")
	}
}

func TestConcurrentMuteAfterPostStartedStillTracksAcceptedReceipt(t *testing.T) {
	d, _, p := workerDB(t)
	f := &relayFixture{}
	var muteErr error
	f.onPost = func() { p.TelegramNotifications = false; muteErr = d.UpdateProject(p) }
	w, _ := fixtureWorker(t, d, f)
	completed(t, d, p, models.StatusSuccess)
	now := time.Now().Add(time.Second)
	due, err := d.DueNotifications(now, 1)
	if err != nil || len(due) != 1 {
		t.Fatal(due, err)
	}
	id := due[0].ID
	sweep(t, w, now)
	var observedMuteErr error
	f.change(func() { observedMuteErr = muteErr })
	if observedMuteErr != nil {
		t.Fatal(observedMuteErr)
	}
	n, err := d.GetNotification(id)
	if err != nil || n.State != "submitted" || n.RelayID != testReceiptID {
		t.Fatal("lost irrevocable acceptance on concurrent mute", n, err)
	}
	sweep(t, w, now.Add(time.Minute))
	n, err = d.GetNotification(id)
	if err != nil || n.State != "delivered" {
		t.Fatal(n, err)
	}
	posts, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatal("muted accepted receipt was reposted", posts)
	}
}

func TestPost429GatesWholeRelayQueueAcrossRestart(t *testing.T) {
	d, path, p := workerDB(t)
	f := &relayFixture{postCode: 429, retry: "120"}
	w, server := fixtureWorker(t, d, f)
	completed(t, d, p, models.StatusSuccess)
	completed(t, d, p, models.StatusFailed)
	now := time.Now().Add(time.Second)
	sweep(t, w, now)
	posts, calls := f.snapshot()
	if len(posts) != 1 {
		t.Fatal("429 did not stop remaining submissions", posts)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	w, err = New(d, nil, Config{URL: server.URL, Key: "relay-test-key", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sweep(t, w, now.Add(119*time.Second))
	posts, after := f.snapshot()
	if len(posts) != 1 || after != calls {
		t.Fatal("global429 gate lost on restart", posts, after, calls)
	}
	f.change(func() { f.postCode = 202; f.retry = "" })
	sweep(t, w, now.Add(120*time.Second))
	posts, _ = f.snapshot()
	if len(posts) != 3 {
		t.Fatal("queued events not resumed after429", posts)
	}
}
