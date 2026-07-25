package live

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// stubProgress stands in for the runner's live step registry.
type stubProgress struct {
	id   int64
	step string
}

func (s stubProgress) Progress(id int64) (string, bool) {
	if s.id == id {
		return s.step, true
	}
	return "", false
}

func newHub(t *testing.T, progress Progresser) (*Hub, *db.DB, *models.Project) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	p := &models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	}
	if err := d.CreateProject(p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	h := New(d, progress)
	h.Tick = 10 * time.Millisecond // tests must not wait a real second
	return h, d, p
}

func newBuild(t *testing.T, d *db.DB, projectID int64, status models.BuildStatus) *models.Build {
	t.Helper()
	b := &models.Build{ProjectID: projectID, Status: models.StatusPending, CommitSHA: "abc123", CommitMessage: "msg"}
	if err := d.CreateBuild(b); err != nil {
		t.Fatalf("create build: %v", err)
	}
	if status == models.StatusRunning {
		if ok, err := d.ClaimBuild(b.ID); err != nil || !ok {
			t.Fatalf("claim build: ok=%v err=%v", ok, err)
		}
		b.Status = models.StatusRunning
	}
	return b
}

func decode(t *testing.T, raw []byte) Message {
	t.Helper()
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if msg.Type != "builds" {
		t.Fatalf("message type = %q, want builds", msg.Type)
	}
	return msg
}

// recv waits for the next snapshot, failing the test on timeout.
func recv(t *testing.T, ch <-chan []byte) Message {
	t.Helper()
	select {
	case raw, open := <-ch:
		if !open {
			t.Fatal("subscription closed unexpectedly")
		}
		return decode(t, raw)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a snapshot")
		return Message{}
	}
}

func TestSnapshotCarriesLiveProgressWithoutLogs(t *testing.T) {
	h, d, p := newHub(t, nil)
	running := newBuild(t, d, p.ID, models.StatusRunning)
	queued := newBuild(t, d, p.ID, models.StatusPending)
	h.progress = stubProgress{id: running.ID, step: "build"}

	// A stored log must never ride along in the feed.
	if err := d.AppendBuildLog(running.ID, "lots and lots of log bytes\n"); err != nil {
		t.Fatalf("append log: %v", err)
	}

	raw, err := h.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	msg := decode(t, raw)
	if len(msg.Builds) != 2 {
		t.Fatalf("got %d builds, want 2", len(msg.Builds))
	}
	byID := map[int64]models.Build{}
	for _, b := range msg.Builds {
		if b.Log != "" {
			t.Errorf("build %d carried %d bytes of log", b.ID, len(b.Log))
		}
		if b.ProjectName != "app" {
			t.Errorf("build %d project_name = %q, want app", b.ID, b.ProjectName)
		}
		byID[b.ID] = b
	}
	if got := byID[running.ID].CurrentStep; got != "build" {
		t.Errorf("running build current_step = %q, want build", got)
	}
	// Position 2: the running build occupies the single worker slot.
	if got := byID[queued.ID].QueuePosition; got != 2 {
		t.Errorf("queued build queue_position = %d, want 2", got)
	}
	// Newest first, matching the order the pages render.
	if msg.Builds[0].ID != queued.ID {
		t.Errorf("snapshot order = %d first, want newest (%d)", msg.Builds[0].ID, queued.ID)
	}
}

func TestSubscribeDeliversCurrentStateImmediately(t *testing.T) {
	h, d, p := newHub(t, nil)
	b := newBuild(t, d, p.ID, models.StatusRunning)

	ch, unsub := h.Subscribe()
	defer unsub()

	msg := recv(t, ch)
	if len(msg.Builds) != 1 || msg.Builds[0].ID != b.ID {
		t.Fatalf("first snapshot = %+v, want the running build", msg.Builds)
	}

	// A second subscriber is served the cached snapshot, not a tick later.
	ch2, unsub2 := h.Subscribe()
	defer unsub2()
	select {
	case <-ch2:
	case <-time.After(time.Second):
		t.Fatal("second subscriber waited for the next tick")
	}
}

func TestNewBuildIsBroadcastToIdlePage(t *testing.T) {
	// The bug this whole feed exists for: a page loaded with nothing running
	// must learn about a build that starts afterwards.
	h, d, p := newHub(t, nil)

	ch, unsub := h.Subscribe()
	defer unsub()
	if msg := recv(t, ch); len(msg.Builds) != 0 {
		t.Fatalf("first snapshot = %+v, want empty", msg.Builds)
	}

	b := newBuild(t, d, p.ID, models.StatusPending)
	msg := recv(t, ch)
	if len(msg.Builds) != 1 || msg.Builds[0].ID != b.ID {
		t.Fatalf("snapshot after create = %+v, want the new build", msg.Builds)
	}

	// ...and its transition to running.
	if ok, err := d.ClaimBuild(b.ID); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if msg := recv(t, ch); msg.Builds[0].Status != models.StatusRunning {
		t.Fatalf("status after claim = %q, want running", msg.Builds[0].Status)
	}
}

func TestUnchangedStateIsNotRebroadcast(t *testing.T) {
	h, d, p := newHub(t, nil)
	newBuild(t, d, p.ID, models.StatusRunning)

	ch, unsub := h.Subscribe()
	defer unsub()
	recv(t, ch) // the initial snapshot

	// Many ticks pass with nothing changing.
	select {
	case raw := <-ch:
		t.Fatalf("re-broadcast an unchanged snapshot: %s", raw)
	case <-time.After(20 * h.Tick):
	}
}

func TestLoopStopsWithNoSubscribers(t *testing.T) {
	h, d, p := newHub(t, nil)
	newBuild(t, d, p.ID, models.StatusPending)

	ch, unsub := h.Subscribe()
	recv(t, ch)
	unsub()

	// The sampler must not keep querying the DB for nobody.
	deadline := time.After(2 * time.Second)
	for {
		h.mu.Lock()
		running := h.running
		h.mu.Unlock()
		if !running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("sampling loop still running after the last unsubscribe")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Unsubscribing twice is a no-op, not a double close.
	unsub()

	// Resubscribing restarts it and still delivers state immediately.
	ch2, unsub2 := h.Subscribe()
	defer unsub2()
	if msg := recv(t, ch2); len(msg.Builds) != 1 {
		t.Fatalf("snapshot after resubscribe = %+v, want the build", msg.Builds)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	h, d, p := newHub(t, nil)

	ch, unsub := h.Subscribe()
	defer unsub()

	// Never read: after subBuffer distinct snapshots the hub gives up on us.
	for i := 0; i < subBuffer+2; i++ {
		b := newBuild(t, d, p.ID, models.StatusPending)
		d.ClaimBuild(b.ID)
		time.Sleep(3 * h.Tick)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed, as intended
			}
		case <-deadline:
			t.Fatal("a subscriber that never reads was not dropped")
		}
	}
}
