package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

// testEnv wires a Runner against a real temp DB, a real local git repo, and
// a stub `docker` binary on PATH that respects DOCKER_STUB_SLEEP/EXIT.
type testEnv struct {
	db  *db.DB
	bus *logbus.Bus
	r   *Runner
}

func newTestEnv(t *testing.T) (*testEnv, *models.Build) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Stub docker on PATH.
	binDir := t.TempDir()
	script := "#!/bin/sh\necho \"stub docker $*\"\nsleep ${DOCKER_STUB_SLEEP:-0}\nexit ${DOCKER_STUB_EXIT:-0}\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Local git repo with a Dockerfile.
	repoDir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "init")

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	p := &models.Project{
		Name: "app", RepoURL: "file://" + repoDir, Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "manual", CommitMessage: "test"}
	if err := database.CreateBuild(b); err != nil {
		t.Fatal(err)
	}

	bus := logbus.New()
	r := New(database, make(chan *models.Build), bus)
	return &testEnv{db: database, bus: bus, r: r}, b
}

func TestRunBuildSuccessEmitsGrammar(t *testing.T) {
	env, build := newTestEnv(t)

	env.r.process(build) // synchronous: returns when the build finishes

	got, err := env.db.GetBuild(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusSuccess {
		t.Fatalf("status = %s, want success; log:\n%s", got.Status, got.Log)
	}
	if got.StartedAt == nil || got.FinishedAt == nil {
		t.Errorf("timestamps missing: %+v", got)
	}
	for _, want := range []string{
		"##[step:clone] Cloning file://",
		"##[step:build] Building Docker image:",
		"##[step:push] Pushing image:",
		"BUILD SUCCESS",
		"stub docker build -t registry.fandoster.com/app:latest",
	} {
		if !strings.Contains(got.Log, want) {
			t.Errorf("log missing %q; log:\n%s", want, got.Log)
		}
	}
	// Manual builds don't emit a checkout step.
	if strings.Contains(got.Log, "##[step:checkout]") {
		t.Error("manual build should not emit checkout sentinel")
	}
	// The bus topic must be closed (terminal) and mirror the DB log.
	if env.bus.Live(build.ID) {
		t.Error("bus topic still live after terminal status")
	}
	tail, _, ok := env.bus.LogTail(build.ID, 0)
	if !ok || string(tail) != got.Log {
		t.Errorf("bus buffer and DB log diverged (ok=%v, bus=%d bytes, db=%d bytes)", ok, len(tail), len(got.Log))
	}
}

func TestRunBuildFailurePropagates(t *testing.T) {
	env, build := newTestEnv(t)
	t.Setenv("DOCKER_STUB_EXIT", "7")

	env.r.process(build)

	got, _ := env.db.GetBuild(build.ID)
	if got.Status != models.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if !strings.Contains(got.Log, "[ERROR] Docker build failed") {
		t.Errorf("log missing error marker:\n%s", got.Log)
	}
}

// A build canceled while still queued must be skipped by the worker: the
// claim fails and nothing runs.
func TestCanceledWhileQueuedIsSkipped(t *testing.T) {
	env, build := newTestEnv(t)

	ok, err := env.db.CancelPendingBuild(build.ID)
	if err != nil || !ok {
		t.Fatalf("cancel pending: ok=%v err=%v", ok, err)
	}
	env.r.process(build)

	got, _ := env.db.GetBuild(build.ID)
	if got.Status != models.StatusCanceled {
		t.Fatalf("status = %s, want canceled", got.Status)
	}
	if strings.Contains(got.Log, "Starting build") {
		t.Errorf("canceled build was executed:\n%s", got.Log)
	}
}

// INVARIANT: the cancel registry is populated before ClaimBuild, so a cancel
// arriving in that window still cancels the run. This mirrors process() with
// the cancel injected at the worst possible moment.
func TestCancelBetweenDequeueAndClaim(t *testing.T) {
	env, build := newTestEnv(t)
	r := env.r

	ctx, cancelCause := context.WithCancelCause(r.ctx)
	r.mu.Lock()
	r.currentID = build.ID
	r.cancelCurrent = cancelCause
	r.mu.Unlock()

	// User cancel lands after registration but before the claim.
	if !r.Cancel(build.ID) {
		t.Fatal("Cancel should reach the registered build")
	}

	claimed, err := r.DB.ClaimBuild(build.ID)
	if err != nil || !claimed {
		t.Fatalf("claim should still succeed: claimed=%v err=%v", claimed, err)
	}
	r.runBuild(ctx, build, time.Now().UTC())

	got, _ := env.db.GetBuild(build.ID)
	if got.Status != models.StatusCanceled {
		t.Fatalf("status = %s, want canceled; log:\n%s", got.Status, got.Log)
	}
	if !strings.Contains(got.Log, "Build canceled by user") {
		t.Errorf("log missing cancel marker:\n%s", got.Log)
	}
}

func TestCancelDuringRun(t *testing.T) {
	env, build := newTestEnv(t)
	t.Setenv("DOCKER_STUB_SLEEP", "30")

	done := make(chan struct{})
	go func() {
		env.r.process(build)
		close(done)
	}()

	// Wait until the docker build step is underway (visible on the bus).
	deadline := time.Now().Add(10 * time.Second)
	for {
		tail, _, _ := env.bus.LogTail(build.ID, 0)
		if strings.Contains(string(tail), "##[step:build]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("build step never started; log so far:\n%s", tail)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !env.r.Cancel(build.ID) {
		t.Fatal("Cancel returned false for the running build")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("build did not stop after cancel")
	}

	got, _ := env.db.GetBuild(build.ID)
	if got.Status != models.StatusCanceled {
		t.Fatalf("status = %s, want canceled; log:\n%s", got.Status, got.Log)
	}
	if !strings.Contains(got.Log, "Build canceled by user") {
		t.Errorf("log missing cancel marker:\n%s", got.Log)
	}
}

func TestBuildTimeout(t *testing.T) {
	env, build := newTestEnv(t)
	t.Setenv("DOCKER_STUB_SLEEP", "30")
	env.r.Timeout = 2 * time.Second

	env.r.process(build)

	got, _ := env.db.GetBuild(build.ID)
	if got.Status != models.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if !strings.Contains(got.Log, "(build timed out)") {
		t.Errorf("log missing timeout hint:\n%s", got.Log)
	}
}

// The janitor must sweep running rows that are not the current build and
// leave the in-flight one alone; Progress must report the current step.
func TestJanitorSweepsStaleAndProgressReports(t *testing.T) {
	env, build := newTestEnv(t)

	// A stale running row from a "crashed" process.
	stale := &models.Build{ProjectID: build.ProjectID, Status: models.StatusPending}
	if err := env.db.CreateBuild(stale); err != nil {
		t.Fatal(err)
	}
	if ok, _ := env.db.ClaimBuild(stale.ID); !ok {
		t.Fatal("claim stale")
	}

	t.Setenv("DOCKER_STUB_SLEEP", "30")
	done := make(chan struct{})
	go func() {
		env.r.process(build)
		close(done)
	}()

	// Wait for the real build to be mid-docker-build.
	deadline := time.Now().Add(10 * time.Second)
	for {
		tail, _, _ := env.bus.LogTail(build.ID, 0)
		if strings.Contains(string(tail), "##[step:build]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("build step never started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if step, ok := env.r.Progress(build.ID); !ok || step != "build" {
		t.Errorf("Progress = %q,%v want build,true", step, ok)
	}
	if _, ok := env.r.Progress(stale.ID); ok {
		t.Error("Progress should not report for a non-current build")
	}

	swept := env.r.SweepStale()
	if len(swept) != 1 || swept[0] != stale.ID {
		t.Fatalf("swept = %v, want [%d]", swept, stale.ID)
	}
	got, _ := env.db.GetBuild(stale.ID)
	if got.Status != models.StatusFailed || got.FinishedAt != nil {
		t.Errorf("stale after sweep: status=%s finished=%v", got.Status, got.FinishedAt)
	}
	// The live build must be untouched and cancelable.
	if cur, _ := env.db.GetBuild(build.ID); cur.Status != models.StatusRunning {
		t.Fatalf("current build swept: %s", cur.Status)
	}
	env.r.Cancel(build.ID)
	<-done
}

// The docker build command gets --no-cache exactly when the project opts in.
// The stub docker echoes its args into the log, so we can assert on them.
func TestNoCacheFlagPassedWhenEnabled(t *testing.T) {
	for _, noCache := range []bool{false, true} {
		env, build := newTestEnv(t)
		p, _ := env.db.GetProject(build.ProjectID)
		p.NoCache = noCache
		if err := env.db.UpdateProject(p); err != nil {
			t.Fatal(err)
		}

		env.r.process(build)

		got, _ := env.db.GetBuild(build.ID)
		if got.Status != models.StatusSuccess {
			t.Fatalf("noCache=%v: status=%s log:\n%s", noCache, got.Status, got.Log)
		}
		hasFlag := strings.Contains(got.Log, "docker build --no-cache") ||
			strings.Contains(got.Log, "build -t") && strings.Contains(got.Log, "--no-cache")
		if hasFlag != noCache {
			t.Errorf("noCache=%v but --no-cache present=%v; build line:\n%s",
				noCache, hasFlag, firstLineContaining(got.Log, "stub docker build"))
		}
	}
}

func firstLineContaining(log, sub string) string {
	for _, ln := range strings.Split(log, "\n") {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return "(no matching line)"
}

// A server restart must not destroy work in progress: the in-flight build
// goes back on the queue instead of being marked failed. This is what stops a
// Watchtower redeploy (triggered by the build server pushing its own image)
// from killing whatever it happened to be building.
func TestShutdownRequeuesInFlightBuild(t *testing.T) {
	env, build := newTestEnv(t)
	t.Setenv("DOCKER_STUB_SLEEP", "10")

	done := make(chan struct{})
	go func() {
		defer close(done)
		env.r.process(build)
	}()
	waitForStatus(t, env, build.ID, models.StatusRunning)

	env.r.Stop() // SIGTERM equivalent
	<-done

	got, err := env.db.GetBuild(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusPending {
		t.Fatalf("status = %s, want pending (re-queued); log:\n%s", got.Status, got.Log)
	}
	if got.Requeues != 1 {
		t.Errorf("requeues = %d, want 1", got.Requeues)
	}
	if got.StartedAt != nil || got.FinishedAt != nil {
		t.Errorf("timestamps not reset for the next attempt: started=%v finished=%v", got.StartedAt, got.FinishedAt)
	}
	if !strings.Contains(got.Log, "re-queued") {
		t.Errorf("log missing the restart seam; log:\n%s", got.Log)
	}
	if strings.Contains(got.Log, "[ERROR]") {
		t.Errorf("re-queued build recorded an error; log:\n%s", got.Log)
	}
	// The seam must go through the sink, not straight to the row, or the
	// live-log buffer and the stored log drift apart.
	tail, _, ok := env.bus.LogTail(build.ID, 0)
	if !ok || string(tail) != got.Log {
		t.Errorf("bus buffer and DB log diverged (ok=%v, bus=%d bytes, db=%d bytes)", ok, len(tail), len(got.Log))
	}
}

// The re-queue is bounded: a build that takes the server down with it every
// time must eventually be failed rather than retried on every boot forever.
func TestShutdownRequeueIsBounded(t *testing.T) {
	env, build := newTestEnv(t)
	t.Setenv("DOCKER_STUB_SLEEP", "10")

	// Each iteration is a fresh process restarting and picking the build up.
	for i := 0; i <= models.MaxBuildRequeues; i++ {
		r := New(env.db, make(chan *models.Build), env.bus)
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.process(build)
		}()
		waitForStatus(t, env, build.ID, models.StatusRunning)
		r.Stop()
		<-done
	}

	got, _ := env.db.GetBuild(build.ID)
	if got.Status != models.StatusFailed {
		t.Fatalf("status = %s after %d restarts, want failed once re-queues run out",
			got.Status, models.MaxBuildRequeues+1)
	}
	if got.Requeues != models.MaxBuildRequeues {
		t.Errorf("requeues = %d, want the cap of %d", got.Requeues, models.MaxBuildRequeues)
	}
	if !strings.Contains(got.Log, "no re-queues left") {
		t.Errorf("log doesn't explain why it stopped retrying; log:\n%s", got.Log)
	}
}

func waitForStatus(t *testing.T, env *testEnv, id int64, want models.BuildStatus) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := env.db.GetBuild(id); err == nil && b.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("build %d never reached status %s", id, want)
}
