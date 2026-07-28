package db

import (
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

func agentProject(t *testing.T, d *DB, name, executor string) *models.Project {
	t.Helper()
	p := &models.Project{
		Name: name, RepoURL: "https://github.com/u/" + name, Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: name, Executor: executor,
	}
	if err := d.CreateProject(p); err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
	return p
}

func pendingBuild(t *testing.T, d *DB, projectID int64) *models.Build {
	t.Helper()
	b := &models.Build{ProjectID: projectID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatalf("create build: %v", err)
	}
	return b
}

// setHeartbeat backdates a build's heartbeat to simulate an agent that has
// been quiet for d.
func setHeartbeat(t *testing.T, d *DB, id int64, ago time.Duration) {
	t.Helper()
	if _, err := d.conn.Exec(`UPDATE builds SET last_heartbeat_at=? WHERE id=?`,
		time.Now().UTC().Add(-ago), id); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}
}

func TestProjectExecutorDefaultsToLocal(t *testing.T) {
	d := openTestDB(t)
	p := agentProject(t, d, "app", "")

	if p.Executor != models.ExecutorLocal {
		t.Errorf("CreateProject left executor %q, want %q", p.Executor, models.ExecutorLocal)
	}
	got, err := d.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Executor != models.ExecutorLocal {
		t.Errorf("stored executor = %q, want %q", got.Executor, models.ExecutorLocal)
	}
	if models.Remote(got.Executor) {
		t.Error("local project reported as remote")
	}
}

// The claim is the whole routing mechanism: an agent must get its own
// projects' builds and never the local runner's.
func TestClaimBuildForAgentMatchesExecutorOnly(t *testing.T) {
	d := openTestDB(t)
	local := agentProject(t, d, "web", models.ExecutorLocal)
	mac := agentProject(t, d, "game", "mac")

	localBuild := pendingBuild(t, d, local.ID)
	macBuild := pendingBuild(t, d, mac.ID)

	got, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("claimed nothing, want the mac build")
	}
	if got.ID != macBuild.ID {
		t.Fatalf("claimed build %d, want %d (the mac one)", got.ID, macBuild.ID)
	}
	if got.Status != models.StatusRunning || got.Agent != "mac-1" {
		t.Errorf("claimed build: status=%s agent=%q, want running/mac-1", got.Status, got.Agent)
	}
	if got.StartedAt == nil {
		t.Error("claimed build has no started_at")
	}
	if got.LastHeartbeatAt == nil {
		t.Error("claim did not seed a heartbeat; the janitor would fail it immediately")
	}
	if got.Executor != "mac" {
		t.Errorf("claimed build executor = %q, want mac (joined from the project)", got.Executor)
	}

	// The local project's build is untouched and still waiting for the worker.
	stillPending, err := d.GetBuild(localBuild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillPending.Status != models.StatusPending {
		t.Errorf("local build status = %s, want pending — an agent took the runner's work",
			stillPending.Status)
	}

	// Nothing left for this agent.
	if again, err := d.ClaimBuildForAgent("mac-1", []string{"mac"}); err != nil || again != nil {
		t.Errorf("second claim = %v (err=%v), want nil", again, err)
	}
}

func TestClaimBuildForAgentTakesOldestFirst(t *testing.T) {
	d := openTestDB(t)
	mac := agentProject(t, d, "game", "mac")
	first := pendingBuild(t, d, mac.ID)
	second := pendingBuild(t, d, mac.ID)

	got, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || got == nil {
		t.Fatalf("claim: %v %v", got, err)
	}
	if got.ID != first.ID {
		t.Errorf("claimed %d, want the older build %d", got.ID, first.ID)
	}
	next, err := d.ClaimBuildForAgent("mac-2", []string{"mac"})
	if err != nil || next == nil {
		t.Fatalf("second claim: %v %v", next, err)
	}
	if next.ID != second.ID {
		t.Errorf("claimed %d, want %d", next.ID, second.ID)
	}
}

// Two agents polling at the same instant must not both get the same build.
func TestClaimBuildForAgentIsExclusive(t *testing.T) {
	d := openTestDB(t)
	mac := agentProject(t, d, "game", "mac")
	build := pendingBuild(t, d, mac.ID)

	a, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.ClaimBuildForAgent("mac-2", []string{"mac"})
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || a.ID != build.ID {
		t.Fatalf("first claim = %v, want build %d", a, build.ID)
	}
	if b != nil {
		t.Fatalf("second agent also claimed build %d — the compare-and-swap is not exclusive", b.ID)
	}
	owner, _ := d.GetBuild(build.ID)
	if owner.Agent != "mac-1" {
		t.Errorf("owner = %q, want mac-1", owner.Agent)
	}
}

func TestClaimBuildForAgentMultipleExecutors(t *testing.T) {
	d := openTestDB(t)
	win := agentProject(t, d, "game-win", "windows")
	pendingBuild(t, d, win.ID)

	got, err := d.ClaimBuildForAgent("multi", []string{"mac", "windows"})
	if err != nil || got == nil {
		t.Fatalf("claim across two executors: %v %v", got, err)
	}
	if got.Executor != "windows" {
		t.Errorf("executor = %q, want windows", got.Executor)
	}
}

// The invariant M0 exists for: the janitor must not kill a build that is
// running fine on another machine.
func TestFailStaleRunningSparesLiveAgentBuilds(t *testing.T) {
	d := openTestDB(t)
	local := agentProject(t, d, "web", models.ExecutorLocal)
	mac := agentProject(t, d, "game", "mac")

	// A local build nobody is running: stale by the single-worker rule.
	orphan := pendingBuild(t, d, local.ID)
	if ok, err := d.ClaimBuild(orphan.ID); err != nil || !ok {
		t.Fatalf("claim orphan: %v %v", ok, err)
	}

	// A live agent build: claimed and heartbeating a moment ago.
	pendingBuild(t, d, mac.ID)
	live, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || live == nil {
		t.Fatalf("claim live: %v %v", live, err)
	}

	// A dead agent build: claimed, then silent for well over the TTL.
	pendingBuild(t, d, mac.ID)
	dead, err := d.ClaimBuildForAgent("mac-2", []string{"mac"})
	if err != nil || dead == nil {
		t.Fatalf("claim dead: %v %v", dead, err)
	}
	setHeartbeat(t, d, dead.ID, 10*models.AgentHeartbeatTTL)

	// floor well in the past: this process has been up a while, so the stored
	// heartbeats are the only evidence that counts.
	floor := time.Now().Add(-time.Hour)
	failed, err := d.FailStaleRunning(0, floor)
	if err != nil {
		t.Fatal(err)
	}

	failedSet := map[int64]string{}
	for _, sb := range failed {
		failedSet[sb.ID] = sb.Note
	}
	if _, ok := failedSet[orphan.ID]; !ok {
		t.Error("local orphan survived the sweep")
	}
	if _, ok := failedSet[dead.ID]; !ok {
		t.Error("build of a long-silent agent survived the sweep")
	}
	if _, ok := failedSet[live.ID]; ok {
		t.Fatal("sweep failed a heartbeating agent's build — this is the bug M0 exists to prevent")
	}

	liveRow, _ := d.GetBuild(live.ID)
	if liveRow.Status != models.StatusRunning {
		t.Errorf("live agent build status = %s, want running", liveRow.Status)
	}

	// The two failure modes are told apart in the log, because the remedies
	// differ: a restart is this server's fault, a lost agent is not.
	orphanRow, _ := d.GetBuild(orphan.ID)
	if got := orphanRow.Log; got != RestartNote {
		t.Errorf("orphan log = %q, want the restart note", got)
	}
	deadRow, _ := d.GetBuild(dead.ID)
	if got := deadRow.Log; got != AgentLostNote {
		t.Errorf("dead-agent log = %q, want the agent-lost note", got)
	}
	if deadRow.FinishedAt != nil {
		t.Error("finished_at was stamped on an interrupted build; the real end time is unknown")
	}
}

// A redeploy makes every heartbeat look old — the agents had nowhere to send
// them. The floor is what stops the restart from failing healthy builds.
func TestFailStaleRunningGracesAgentsAfterRestart(t *testing.T) {
	d := openTestDB(t)
	mac := agentProject(t, d, "game", "mac")
	pendingBuild(t, d, mac.ID)
	build, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || build == nil {
		t.Fatalf("claim: %v %v", build, err)
	}
	// The server was down for an hour; the agent kept building throughout.
	setHeartbeat(t, d, build.ID, time.Hour)

	// Startup sweep: floor is now, because this process just came up.
	failed, err := d.FailStaleRunning(0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, sb := range failed {
		if sb.ID == build.ID {
			t.Fatal("startup recovery failed a live agent's build; it cannot heartbeat a server that is down")
		}
	}
	row, _ := d.GetBuild(build.ID)
	if row.Status != models.StatusRunning {
		t.Errorf("status = %s, want running (survives the restart)", row.Status)
	}
}

// Agent builds are never handed back to the queue: the agent may already have
// uploaded to Steam, and this server cannot know how far it got.
func TestRequeueStaleRunningSkipsAgentBuilds(t *testing.T) {
	d := openTestDB(t)
	local := agentProject(t, d, "web", models.ExecutorLocal)
	mac := agentProject(t, d, "game", "mac")

	localBuild := pendingBuild(t, d, local.ID)
	if ok, err := d.ClaimBuild(localBuild.ID); err != nil || !ok {
		t.Fatalf("claim local: %v %v", ok, err)
	}
	pendingBuild(t, d, mac.ID)
	agentBuild, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || agentBuild == nil {
		t.Fatalf("claim agent: %v %v", agentBuild, err)
	}

	requeued, err := d.RequeueStaleRunning(models.MaxBuildRequeues)
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0] != localBuild.ID {
		t.Fatalf("requeued = %v, want just the local build %d", requeued, localBuild.ID)
	}
	row, _ := d.GetBuild(agentBuild.ID)
	if row.Status != models.StatusRunning || row.Requeues != 0 {
		t.Errorf("agent build: status=%s requeues=%d, want running/0", row.Status, row.Requeues)
	}
}

func TestHeartbeatBuild(t *testing.T) {
	d := openTestDB(t)
	mac := agentProject(t, d, "game", "mac")
	pendingBuild(t, d, mac.ID)
	build, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || build == nil {
		t.Fatalf("claim: %v %v", build, err)
	}
	setHeartbeat(t, d, build.ID, time.Hour)

	ok, cancel, err := d.HeartbeatBuild(build.ID, "mac-1")
	if err != nil || !ok {
		t.Fatalf("heartbeat: ok=%v err=%v", ok, err)
	}
	if cancel {
		t.Error("cancel reported without anyone asking for one")
	}
	row, _ := d.GetBuild(build.ID)
	if row.LastHeartbeatAt == nil || time.Since(*row.LastHeartbeatAt) > time.Minute {
		t.Errorf("heartbeat not renewed: %v", row.LastHeartbeatAt)
	}

	// Another agent must not be able to renew — or steal — someone's lease.
	if ok, _, err := d.HeartbeatBuild(build.ID, "impostor"); err != nil || ok {
		t.Errorf("heartbeat from the wrong agent: ok=%v err=%v, want false", ok, err)
	}

	// A cancel request reaches the owner on its next beat.
	if ok, err := d.RequestAgentCancel(build.ID); err != nil || !ok {
		t.Fatalf("request cancel: ok=%v err=%v", ok, err)
	}
	if _, cancel, err := d.HeartbeatBuild(build.ID, "mac-1"); err != nil || !cancel {
		t.Errorf("heartbeat after cancel: cancel=%v err=%v, want true", cancel, err)
	}

	// Once terminal, the lease is gone.
	if err := d.FinishBuild(build.ID, models.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := d.HeartbeatBuild(build.ID, "mac-1"); err != nil || ok {
		t.Errorf("heartbeat on a finished build: ok=%v, want false", ok)
	}
}

func TestRequestAgentCancelIgnoresLocalBuilds(t *testing.T) {
	d := openTestDB(t)
	local := agentProject(t, d, "web", models.ExecutorLocal)
	b := pendingBuild(t, d, local.ID)
	if ok, err := d.ClaimBuild(b.ID); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	// The local runner is cancelled in-process, not through this flag.
	if ok, err := d.RequestAgentCancel(b.ID); err != nil || ok {
		t.Errorf("RequestAgentCancel on a local build = %v (err=%v), want false", ok, err)
	}
}

// Log offsets are byte offsets. SQLite's length() counts characters, which
// diverges the moment a build prints anything non-ASCII — and Unity logs are
// full of it.
func TestBuildLogLenCountsBytesNotCharacters(t *testing.T) {
	d := openTestDB(t)
	p := agentProject(t, d, "game", "mac")
	b := pendingBuild(t, d, p.ID)

	const line = "[12:00:00] ##[step:unity] Bâtiment — 建物\n"
	if err := d.AppendBuildLog(b.ID, line); err != nil {
		t.Fatal(err)
	}
	got, err := d.BuildLogLen(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != len(line) {
		t.Errorf("BuildLogLen = %d, want %d (Go byte length)", got, len(line))
	}
	if got == len([]rune(line)) {
		t.Error("BuildLogLen is counting characters — offsets will drift from the agent's")
	}
}

// A DB created before the agent columns existed must migrate in place.
func TestMigrationAddsAgentColumns(t *testing.T) {
	d := openTestDB(t)
	for _, col := range []struct{ table, name string }{
		{"projects", "executor"},
		{"builds", "agent"},
		{"builds", "last_heartbeat_at"},
		{"builds", "cancel_requested"},
	} {
		if _, err := d.conn.Exec("ALTER TABLE " + col.table + " DROP COLUMN " + col.name); err != nil {
			t.Fatalf("drop %s.%s: %v", col.table, col.name, err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	// Idempotent: running it again must not fail.
	if err := d.migrate(); err != nil {
		t.Fatalf("re-migrate twice: %v", err)
	}

	p := agentProject(t, d, "game", "mac")
	got, err := d.GetProject(p.ID)
	if err != nil {
		t.Fatalf("read back after migration: %v", err)
	}
	if got.Executor != "mac" {
		t.Errorf("executor = %q after migration, want mac", got.Executor)
	}
	b := pendingBuild(t, d, p.ID)
	claimed, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || claimed == nil || claimed.ID != b.ID {
		t.Fatalf("claim after migration: %v %v", claimed, err)
	}
}

// Queue position is per-executor. The two queues do not wait on each other, so
// counting them together tells an operator their Docker build is seventh in
// line when it is about to start.
func TestQueuePositionIsScopedToItsOwnExecutor(t *testing.T) {
	d := openTestDB(t)
	local := agentProject(t, d, "web", models.ExecutorLocal)
	mac := agentProject(t, d, "game", "mac")

	// The Mac has been offline all weekend: five builds stacked up for it.
	for i := 0; i < 5; i++ {
		pendingBuild(t, d, mac.ID)
	}
	// Plus one actually running on an agent.
	pendingBuild(t, d, mac.ID)
	if _, err := d.ClaimBuildForAgent("mac-1", []string{"mac"}); err != nil {
		t.Fatal(err)
	}

	// A Docker build triggered now is next in line, with nothing running.
	localBuild := pendingBuild(t, d, local.ID)
	pos, err := d.QueuePosition(localBuild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 {
		t.Errorf("local build position = %d, want 1 — it is not behind the Mac's backlog", pos)
	}

	// And a Mac build queued now is behind its own five, plus the running one.
	macBuild := pendingBuild(t, d, mac.ID)
	pos, err = d.QueuePosition(macBuild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 7 {
		t.Errorf("mac build position = %d, want 7 (5 waiting + 1 running + itself)", pos)
	}

	// A second local build is behind the first, and still unaffected by the Mac.
	second := pendingBuild(t, d, local.ID)
	if pos, err := d.QueuePosition(second.ID); err != nil || pos != 2 {
		t.Errorf("second local build position = %d (err=%v), want 2", pos, err)
	}
}
