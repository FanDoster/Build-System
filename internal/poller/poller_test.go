package poller

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// fixture wires a poller against a temp DB with a stubbed ls-remote, so the
// tests exercise the scheduling and build-decision logic without a network or
// a real git binary.
type fixture struct {
	t       *testing.T
	db      *db.DB
	p       *Poller
	buildCh chan *models.Build

	mu    sync.Mutex
	sha   string
	err   error
	calls int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	f := &fixture{t: t, db: d, buildCh: make(chan *models.Build, 8), sha: "aaaaaaaaaaaa1111"}
	f.p = New(d, f.buildCh)
	f.p.LsRemote = func(ctx context.Context, repoURL, branch string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		return f.sha, f.err
	}
	return f
}

func (f *fixture) setRemote(sha string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sha, f.err = sha, err
}

func (f *fixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// queued drains the build channel.
func (f *fixture) queued() []*models.Build {
	var out []*models.Build
	for {
		select {
		case b := <-f.buildCh:
			out = append(out, b)
		default:
			return out
		}
	}
}

func (f *fixture) project(poll bool, interval int) *models.Project {
	f.t.Helper()
	p := &models.Project{
		Name: "app", RepoURL: "https://example.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		PollEnabled: poll, PollIntervalSecs: interval,
	}
	if err := f.db.CreateProject(p); err != nil {
		f.t.Fatalf("create project: %v", err)
	}
	return p
}

func TestSweepSkipsDisabledProjects(t *testing.T) {
	f := newFixture(t)
	f.project(false, 60)

	f.p.Sweep()

	if f.callCount() != 0 {
		t.Errorf("polled a project with polling disabled (%d calls)", f.callCount())
	}
	if got := f.queued(); len(got) != 0 {
		t.Errorf("queued %d builds, want 0", len(got))
	}
}

func TestFirstPollSeedsWithoutBuilding(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 60)

	f.p.Sweep()

	if got := f.queued(); len(got) != 0 {
		t.Fatalf("first poll queued %d builds, want 0 (it only records the baseline)", len(got))
	}
	after, _ := f.db.GetProject(p.ID)
	if after.LastPolledSHA != "aaaaaaaaaaaa" {
		t.Errorf("LastPolledSHA = %q, want the abbreviated tip", after.LastPolledSHA)
	}
	if after.LastPolledAt == nil {
		t.Error("LastPolledAt not recorded")
	}
}

func TestNewCommitQueuesBuild(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 0) // 0 → default interval

	f.p.Sweep() // seed
	f.setRemote("bbbbbbbbbbbb2222", nil)
	f.forceDue(p.ID)
	f.p.Sweep()

	got := f.queued()
	if len(got) != 1 {
		t.Fatalf("queued %d builds, want 1", len(got))
	}
	if got[0].CommitSHA != "bbbbbbbbbbbb" {
		t.Errorf("CommitSHA = %q, want the new abbreviated tip", got[0].CommitSHA)
	}
	if got[0].ProjectID != p.ID || got[0].Status != models.StatusPending {
		t.Errorf("unexpected build: %+v", got[0])
	}
	if !strings.Contains(got[0].CommitMessage, "main") {
		t.Errorf("CommitMessage = %q, want it to name the branch", got[0].CommitMessage)
	}
}

func TestUnchangedTipDoesNotBuild(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 30)

	f.p.Sweep() // seed
	f.forceDue(p.ID)
	f.p.Sweep() // same sha

	if got := f.queued(); len(got) != 0 {
		t.Errorf("queued %d builds for an unchanged tip, want 0", len(got))
	}
}

// Regression: build 61 in production. The webhook and the poller race on
// every push when both are enabled. The poller deferred one sweep while the
// webhook's build ran, then — seeing its baseline still behind — queued a
// second build of a commit that had just been built successfully.
func TestCommitAlreadyBuiltByWebhookIsNotRebuilt(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 30)
	f.p.Sweep() // seed at aaaa…

	// The webhook gets there first with the new commit.
	webhook := &models.Build{ProjectID: p.ID, Status: models.StatusRunning, CommitSHA: "dddddddddddd"}
	if err := f.db.CreateBuild(webhook); err != nil {
		t.Fatal(err)
	}
	f.setRemote("dddddddddddd4444", nil)

	// Sweep while it runs: covered, so the baseline is adopted outright — not
	// merely deferred, which is what produced the duplicate.
	f.forceDue(p.ID)
	f.p.Sweep()
	if got := f.queued(); len(got) != 0 {
		t.Fatalf("queued %d builds for a commit the webhook was building, want 0", len(got))
	}
	after, _ := f.db.GetProject(p.ID)
	if after.LastPolledSHA != "dddddddddddd" {
		t.Errorf("LastPolledSHA = %q, want the baseline adopted", after.LastPolledSHA)
	}

	// And still nothing once that build finishes — this is the sweep that
	// used to queue the duplicate.
	if err := f.db.FinishBuild(webhook.ID, models.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	f.forceDue(p.ID)
	f.p.Sweep()
	if got := f.queued(); len(got) != 0 {
		t.Errorf("queued %d duplicate builds after the webhook's build finished, want 0", len(got))
	}
}

// A commit whose build failed must not be retried on every sweep — the
// baseline advance plus the already-built check both have to hold for that.
func TestFailedCommitIsNotRetried(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 30)
	f.p.Sweep() // seed

	f.setRemote("eeeeeeeeeeee5555", nil)
	f.forceDue(p.ID)
	f.p.Sweep()
	got := f.queued()
	if len(got) != 1 {
		t.Fatalf("queued %d builds, want 1", len(got))
	}
	if err := f.db.FinishBuild(got[0].ID, models.StatusFailed); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		f.forceDue(p.ID)
		f.p.Sweep()
	}
	if n := len(f.queued()); n != 0 {
		t.Errorf("queued %d retries of a failed commit, want 0", n)
	}
}

// A commit that lands mid-build must not stack a second build behind the
// running one — but it must not be forgotten either: the in-flight build has
// its own commit pinned and will never produce an image for this one.
func TestActiveBuildDefersRatherThanDropping(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 30)

	f.p.Sweep() // seed
	running := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
	if err := f.db.CreateBuild(running); err != nil {
		t.Fatal(err)
	}

	f.setRemote("cccccccccccc3333", nil)
	f.forceDue(p.ID)
	f.p.Sweep()

	if got := f.queued(); len(got) != 0 {
		t.Fatalf("queued %d builds while one was running, want 0", len(got))
	}
	// Baseline held at the old tip — that is what makes the retry possible.
	after, _ := f.db.GetProject(p.ID)
	if after.LastPolledSHA != "aaaaaaaaaaaa" {
		t.Errorf("LastPolledSHA = %q, want the baseline held while deferred", after.LastPolledSHA)
	}

	// Once the build finishes, the deferred commit is picked up.
	if err := f.db.FinishBuild(running.ID, models.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	f.forceDue(p.ID)
	f.p.Sweep()

	got := f.queued()
	if len(got) != 1 {
		t.Fatalf("queued %d builds after the running one finished, want 1", len(got))
	}
	if got[0].CommitSHA != "cccccccccccc" {
		t.Errorf("CommitSHA = %q, want the deferred commit", got[0].CommitSHA)
	}
}

// A failed probe must not disturb the baseline: keeping the last known tip is
// what stops a recovering remote from queueing a build for an old commit.
func TestPollErrorPreservesBaseline(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 30)

	f.p.Sweep() // seed at aaaa…
	f.setRemote("", context.DeadlineExceeded)
	f.forceDue(p.ID)
	f.p.Sweep()

	after, _ := f.db.GetProject(p.ID)
	if after.LastPolledSHA != "aaaaaaaaaaaa" {
		t.Errorf("LastPolledSHA = %q, want the baseline preserved across a failure", after.LastPolledSHA)
	}
	if after.LastPollError == "" {
		t.Error("LastPollError not recorded")
	}

	// Recovery at the same tip is a no-op, and clears the error.
	f.setRemote("aaaaaaaaaaaa1111", nil)
	f.forceDue(p.ID)
	f.p.Sweep()
	if got := f.queued(); len(got) != 0 {
		t.Errorf("queued %d builds after recovery at an unchanged tip, want 0", len(got))
	}
	after, _ = f.db.GetProject(p.ID)
	if after.LastPollError != "" {
		t.Errorf("LastPollError = %q, want cleared after a successful poll", after.LastPollError)
	}
}

func TestSweepRespectsInterval(t *testing.T) {
	f := newFixture(t)
	f.project(true, 3600)

	f.p.Sweep()
	f.p.Sweep() // immediately again — not due
	if f.callCount() != 1 {
		t.Errorf("polled %d times, want 1 (the second sweep is inside the interval)", f.callCount())
	}
}

func TestSweepForgetsDeletedProjects(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 3600)

	f.p.Sweep()
	if err := f.db.DeleteProject(p.ID); err != nil {
		t.Fatal(err)
	}
	f.p.Sweep()

	f.p.mu.Lock()
	_, still := f.p.due[p.ID]
	f.p.mu.Unlock()
	if still {
		t.Error("schedule entry for a deleted project was not dropped")
	}
}

func TestProjectPollIntervalFloors(t *testing.T) {
	cases := []struct {
		secs int
		want time.Duration
	}{
		{0, models.DefaultPollIntervalSecs * time.Second},
		{5, models.MinPollIntervalSecs * time.Second},
		{-1, models.DefaultPollIntervalSecs * time.Second},
		{120, 120 * time.Second},
	}
	for _, c := range cases {
		p := &models.Project{PollIntervalSecs: c.secs}
		if got := p.PollInterval(); got != c.want {
			t.Errorf("PollInterval(%d) = %s, want %s", c.secs, got, c.want)
		}
	}
}

// forceDue makes the project eligible for the next sweep without sleeping
// through its real interval.
func (f *fixture) forceDue(id int64) {
	f.p.mu.Lock()
	f.p.due[id] = time.Now().Add(-time.Second)
	f.p.mu.Unlock()
}

// TestGitLsRemote exercises the real git probe against a local repository —
// the ls-remote output parsing is the one part the stubbed tests never cover.
func TestGitLsRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = repo
	want, err := head.Output()
	if err != nil {
		t.Fatal(err)
	}

	got, err := gitLsRemote(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("sha = %q, want %q", got, strings.TrimSpace(string(want)))
	}

	// A branch that doesn't exist is an error, not an empty SHA that would
	// read as "the tip changed".
	if _, err := gitLsRemote(context.Background(), repo, "nope"); err == nil {
		t.Error("expected an error for a missing branch")
	}
}

// A polled project on a remote executor still gets a build row — that row IS
// the agent's queue — but the row must not be pushed at the local worker.
func TestPollCreatesBuildWithoutQueueingForRemoteExecutor(t *testing.T) {
	f := newFixture(t)
	p := f.project(true, 60)
	p.Executor = "mac"
	if err := f.db.UpdateProject(p); err != nil {
		t.Fatal(err)
	}

	f.p.Sweep() // seeds the baseline
	f.setRemote("bbbbbbbbbbbb2222", nil)
	f.forceDue(p.ID)
	f.p.Sweep()

	if got := f.queued(); len(got) != 0 {
		t.Fatalf("poller put %d remote-executor build(s) on the local channel; "+
			"the Docker runner would have run them", len(got))
	}
	builds, err := f.db.ListBuildsByStatus(models.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 {
		t.Fatalf("pending builds = %d, want 1 waiting for the agent to claim", len(builds))
	}
	if builds[0].Executor != "mac" {
		t.Errorf("build executor = %q, want mac", builds[0].Executor)
	}
	// And an agent can pick it up.
	claimed, err := f.db.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || claimed == nil {
		t.Fatalf("agent could not claim the polled build: %v %v", claimed, err)
	}
}
