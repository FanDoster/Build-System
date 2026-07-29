package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// fakeSource stands in for the database so these tests need no SQLite and can
// place a build at any point in time.
type fakeSource struct {
	names     []string
	running   map[string]*models.Build
	recent    map[string][]models.Build
	executors []db.RemoteExecutor
	logs      map[int64]string
	rows      []db.AgentRow
	rowsErr   error
}

func (f *fakeSource) AgentNames() ([]string, error) { return f.names, nil }
func (f *fakeSource) RunningBuildForAgent(a string) (*models.Build, error) {
	return f.running[a], nil
}
func (f *fakeSource) RecentBuildsForAgent(a string, n int) ([]models.Build, error) {
	return f.recent[a], nil
}
func (f *fakeSource) RemoteExecutors() ([]db.RemoteExecutor, error) { return f.executors, nil }
func (f *fakeSource) LogTailBytes(id int64, n int) ([]byte, error) {
	return []byte(f.logs[id]), nil
}
func (f *fakeSource) ListAgentRows() ([]db.AgentRow, error) { return f.rows, f.rowsErr }

func at(t time.Time) *time.Time { return &t }

func registryAt(now time.Time) *Registry {
	r := NewRegistry()
	r.SetClock(func() time.Time { return now })
	return r
}

// The three ways an agent can prove it is alive. Every one of them is
// load-bearing; the third especially, because an agent stops polling for the
// whole of a build.
func TestLivenessHasThreeSources(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	t.Run("a held poll is alive", func(t *testing.T) {
		reg := registryAt(now)
		reg.PollStarted("mac", []string{"mac"}, "https") // never closed: still open
		f, err := Build(&fakeSource{}, reg, now)
		if err != nil {
			t.Fatal(err)
		}
		if f.Agents[0].State != StateOnline {
			t.Errorf("state = %q, want online", f.Agents[0].State)
		}
		if !f.Agents[0].Polling {
			t.Error("an open claim is the strongest liveness evidence there is; it was not recorded")
		}
	})

	t.Run("a recent poll is alive", func(t *testing.T) {
		reg := registryAt(now.Add(-30 * time.Second))
		reg.PollStarted("mac", []string{"mac"}, "https")()
		f, _ := Build(&fakeSource{}, reg, now)
		if f.Agents[0].State != StateOnline {
			t.Errorf("state = %q, want online 30s after a poll", f.Agents[0].State)
		}
	})

	t.Run("a building agent is alive on its heartbeat alone", func(t *testing.T) {
		// The agent claimed five minutes ago and has not polled since, because
		// it is building. Only the heartbeat says it is alive.
		reg := registryAt(now.Add(-5 * time.Minute))
		reg.PollStarted("mac", []string{"mac"}, "https")()

		src := &fakeSource{running: map[string]*models.Build{
			"mac": {ID: 7, ProjectName: "game", Status: models.StatusRunning,
				LastHeartbeatAt: at(now.Add(-10 * time.Second))},
		}}
		f, _ := Build(src, reg, now)
		a := f.Agents[0]
		if a.State != StateBusy {
			t.Fatalf("state = %q, want busy — an agent does not poll while it builds, so without the heartbeat term every busy agent reads as offline", a.State)
		}
		if a.LastSeenFrom != "build heartbeat" {
			t.Errorf("last seen from %q, want the heartbeat (the newer evidence)", a.LastSeenFrom)
		}
	})

	t.Run("silence past the tolerance is offline", func(t *testing.T) {
		reg := registryAt(now.Add(-10 * time.Minute))
		reg.PollStarted("mac", []string{"mac"}, "https")()
		f, _ := Build(&fakeSource{}, reg, now)
		if f.Agents[0].State != StateOffline {
			t.Errorf("state = %q, want offline", f.Agents[0].State)
		}
	})
}

// After a restart the registry is empty through no fault of any agent. Painting
// the fleet red would be technically defensible and practically a lie.
func TestAfterARestartAgentsAreWaitingNotOffline(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now) // started just now; nothing has polled

	src := &fakeSource{names: []string{"mac"}}
	f, err := Build(src, reg, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("listed %d agents; one that has built here before must still be listed", len(f.Agents))
	}
	if got := f.Agents[0].State; got != StateWaiting {
		t.Errorf("state = %q, want waiting — the server has only just started", got)
	}

	// Once the grace has passed with still no contact, it really is offline.
	later, _ := Build(src, reg, now.Add(5*time.Minute))
	if got := later.Agents[0].State; got != StateOffline {
		t.Errorf("state = %q after the grace period, want offline", got)
	}
}

// The reason this page exists. A one-character typo in a project's executor
// produces a healthy agent, a build stuck forever, and no error anywhere.
func TestAnUnservedQueueIsReported(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac-1", []string{"mac"}, "https")

	src := &fakeSource{executors: []db.RemoteExecutor{
		{Name: "mac", Projects: []string{"game"}, Pending: 0},
		{Name: "macos", Projects: []string{"typo'd project"}, Pending: 3,
			OldestPending: now.Add(-2 * time.Hour)},
	}}
	f, err := Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Executors) != 2 {
		t.Fatalf("listed %d queues, want 2", len(f.Executors))
	}

	served, unserved := f.Executors[0], f.Executors[1]
	if !served.Served || len(served.Agents) != 1 || served.Agents[0] != "mac-1" {
		t.Errorf("queue %q: served=%v by %v, want served by mac-1", served.Name, served.Served, served.Agents)
	}
	if unserved.Served {
		t.Errorf("queue %q reported as served; no agent asks for it", unserved.Name)
	}
	if unserved.Pending != 3 {
		t.Errorf("pending = %d, want 3 — the count is what makes it urgent", unserved.Pending)
	}
	if unserved.OldestPending == nil {
		t.Error("no oldest-pending time; how long it has been stuck is the point")
	}
}

// An offline agent must not count as serving a queue, or the page would say a
// queue is covered by a machine that is switched off.
func TestAnOfflineAgentDoesNotCoverAQueue(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now.Add(-time.Hour))
	reg.PollStarted("mac-1", []string{"mac"}, "https")()

	src := &fakeSource{executors: []db.RemoteExecutor{{Name: "mac", Pending: 2}}}
	f, _ := Build(src, reg, now)

	if f.Agents[0].State != StateOffline {
		t.Fatalf("agent state = %q, want offline", f.Agents[0].State)
	}
	if f.Executors[0].Served {
		t.Error("a switched-off agent was counted as serving its queue")
	}
}

// The current step comes from the log the agent already writes — no protocol
// field, and it works on builds that are running right now.
func TestCurrentStepIsReadFromTheLog(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{
		running: map[string]*models.Build{
			"mac": {ID: 42, ProjectName: "game", Status: models.StatusRunning,
				LastHeartbeatAt: at(now)},
		},
		logs: map[int64]string{
			42: "[12:00:00] ##[step:checkout] fetching\nsome output\n" +
				"[12:03:00] ##[step:unity] 2022.3.62f2 → StandaloneWindows64\nmore output\n",
		},
	}
	f, _ := Build(src, reg, now)
	cur := f.Agents[0].Current
	if cur == nil {
		t.Fatal("no current build")
	}
	// The LAST step, not the first: the rail moves forward.
	if cur.Step != "unity" {
		t.Errorf("step = %q, want unity", cur.Step)
	}
	if cur.StepDetail == "" {
		t.Error("no step detail; the detail is what makes it readable")
	}
	if cur.ID != 42 || cur.Project != "game" {
		t.Errorf("current = %+v", cur)
	}
}

// Three reds in a row on one machine is the fastest available signal for
// "the box is broken" rather than "the code is broken".
func TestConsecutiveFailuresCountFromTheNewest(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{recent: map[string][]models.Build{
		"mac": {
			{ID: 5, Status: models.StatusFailed},
			{ID: 4, Status: models.StatusFailed},
			{ID: 3, Status: models.StatusSuccess},
			{ID: 2, Status: models.StatusFailed},
		},
	}}
	f, _ := Build(src, reg, now)
	if got := f.Agents[0].ConsecutiveFailures; got != 2 {
		t.Errorf("consecutive failures = %d, want 2 — the run stops at the success", got)
	}
	if len(f.Agents[0].Recent) != 4 {
		t.Errorf("listed %d recent builds, want 4", len(f.Agents[0].Recent))
	}
}

// The build at the head of the list is usually the retry of the very failures
// this counter is warning about. Letting it reset the count would blank the
// warning at the moment somebody is actually watching the machine.
func TestABuildInFlightDoesNotResetTheFailureRun(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{recent: map[string][]models.Build{
		"mac": {
			{ID: 9, Status: models.StatusRunning},
			{ID: 8, Status: models.StatusFailed},
			{ID: 7, Status: models.StatusFailed},
			{ID: 6, Status: models.StatusSuccess},
		},
	}}
	f, _ := Build(src, reg, now)
	if got := f.Agents[0].ConsecutiveFailures; got != 2 {
		t.Errorf("consecutive failures = %d, want 2 — the running build is not an outcome and must be stepped over, not counted as a break", got)
	}
}

// A cancel is a human deciding to stop, not the machine failing. It ends the
// run so an operator's own intervention cannot look like a broken box.
func TestACancelEndsTheFailureRun(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{recent: map[string][]models.Build{
		"mac": {
			{ID: 3, Status: models.StatusFailed},
			{ID: 2, Status: models.StatusCanceled},
			{ID: 1, Status: models.StatusFailed},
		},
	}}
	f, _ := Build(src, reg, now)
	if got := f.Agents[0].ConsecutiveFailures; got != 1 {
		t.Errorf("consecutive failures = %d, want 1", got)
	}
}

// An agent whose requests arrive in clear has already exposed its token.
func TestPlainHTTPIsRecordedAgainstTheAgent(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("careless", []string{"mac"}, "http")

	f, _ := Build(&fakeSource{}, reg, now)
	if f.Agents[0].Scheme != "http" {
		t.Errorf("scheme = %q, want http recorded so the page can warn", f.Agents[0].Scheme)
	}
}

// The production case that this page got wrong on its first deploy.
//
// The server had just redeployed, so the registry was empty. The Mac agent was
// mid-build and therefore not polling — an agent polls only between builds — so
// it was known only from history and advertised nothing. The coverage panel
// concluded that nothing served the "mac" queue and said so in red, while the
// agent was at that moment building from it.
func TestABusyAgentWithNoSightingStillCoversItsQueue(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now) // empty: the server has just restarted

	src := &fakeSource{
		names: []string{"mac-m4max-dan"}, // known only from build history
		running: map[string]*models.Build{
			"mac-m4max-dan": {ID: 122, ProjectName: "Cruise Control Demo Debug",
				Status: models.StatusRunning, Executor: "mac",
				LastHeartbeatAt: at(now.Add(-8 * time.Second))},
		},
		executors: []db.RemoteExecutor{{Name: "mac", Projects: []string{"Cruise Control Demo Debug"}}},
	}
	f, err := Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}

	a := f.Agents[0]
	if a.State != StateBusy {
		t.Fatalf("state = %q, want busy", a.State)
	}
	if !contains(a.Executors, "mac") {
		t.Errorf("executors = %v, want mac inferred from the build in flight", a.Executors)
	}
	if !f.Executors[0].Served {
		t.Error("coverage says nothing serves the mac queue while an agent is building from it")
	}
	if len(f.Executors[0].Agents) != 1 || f.Executors[0].Agents[0] != "mac-m4max-dan" {
		t.Errorf("queue served by %v, want the building agent named", f.Executors[0].Agents)
	}
}

// A local build must never be mistaken for queue coverage.
func TestALocalBuildIsNotQueueCoverage(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)

	src := &fakeSource{
		names: []string{"odd"},
		running: map[string]*models.Build{
			"odd": {ID: 1, ProjectName: "p", Status: models.StatusRunning,
				Executor: models.ExecutorLocal, LastHeartbeatAt: at(now)},
		},
	}
	f, _ := Build(src, reg, now)
	if len(f.Agents[0].Executors) != 0 {
		t.Errorf("executors = %v, want none — local is not a queue", f.Agents[0].Executors)
	}
}

func pausedRow(name string, until time.Time, note string) db.AgentRow {
	u := until
	return db.AgentRow{Name: name, Executors: []string{"mac"}, PausedUntil: &u, PauseNote: note}
}

// A paused agent must read as connected-but-paused. If pause made it look
// offline the distinction would be destroyed, and an operator could not tell
// "I stopped this" from "this machine died".
func TestAPausedAgentIsStillConnected(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https") // still polling, as it must

	src := &fakeSource{rows: []db.AgentRow{pausedRow("mac", now.Add(time.Hour), "updating Unity")}}
	f, err := Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	a := f.Agents[0]
	if a.State != StatePaused {
		t.Errorf("state = %q, want paused", a.State)
	}
	if !a.Polling {
		t.Error("a paused agent must keep polling — that is what keeps it visibly connected")
	}
	if !a.Paused || a.PausedUntil == nil {
		t.Errorf("pause not surfaced: paused=%v until=%v", a.Paused, a.PausedUntil)
	}
	if a.PauseNote != "updating Unity" {
		t.Errorf("note = %q", a.PauseNote)
	}
}

// Busy outranks paused in the headline. A paused agent mid-build really is
// building, and hiding that would make the running build invisible on the page
// that lists what every machine is doing.
func TestPausedAndBuildingShowsBoth(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{
		rows: []db.AgentRow{pausedRow("mac", now.Add(time.Hour), "")},
		running: map[string]*models.Build{
			"mac": {ID: 5, ProjectName: "game", Status: models.StatusRunning,
				Executor: "mac", LastHeartbeatAt: at(now)},
		},
	}
	f, _ := Build(src, reg, now)
	a := f.Agents[0]
	if a.State != StateBusy {
		t.Errorf("state = %q, want busy — the build in flight is the more urgent fact", a.State)
	}
	if !a.Paused {
		t.Error("the pause was lost; both facts must be shown")
	}
	if a.Current == nil {
		t.Error("the running build vanished behind the pause")
	}
}

// An expired pause is no pause. Nobody thinks to check a pause they did not
// set, so this is the property that stops a forgotten pause becoming a dead CI
// that looks healthy.
func TestAnExpiredPauseIsIgnored(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{rows: []db.AgentRow{pausedRow("mac", now.Add(-time.Minute), "forgot about this")}}
	f, _ := Build(src, reg, now)
	a := f.Agents[0]
	if a.Paused || a.State != StateOnline {
		t.Errorf("paused=%v state=%q, want an expired pause to be ignored", a.Paused, a.State)
	}
	if a.PauseNote != "" {
		t.Errorf("note %q shown for an expired pause", a.PauseNote)
	}
}

// The biggest risk in persisting agents. A redeploy takes about two minutes —
// longer than the 90s tolerance — so a remembered agent whose last poll
// predates the restart must NOT be treated as known, or every deploy paints the
// fleet offline and drops it out of queue coverage.
func TestARememberedAgentStillGetsTheRestartGrace(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now) // the server has just started; nothing has polled

	seen := now.Add(-5 * time.Minute) // last poll, well before the redeploy
	src := &fakeSource{
		rows:      []db.AgentRow{{Name: "mac", Executors: []string{"mac"}, LastSeenAt: &seen}},
		executors: []db.RemoteExecutor{{Name: "mac", Pending: 2}},
	}
	f, err := Build(src, reg, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a := f.Agents[0]
	if a.State != StateWaiting {
		t.Errorf("state = %q, want waiting — it has had no chance to poll since this process started", a.State)
	}
	if !a.Remembered {
		t.Error("the persisted row was not recognised")
	}
	if !f.Executors[0].Served {
		t.Error("the queue lost its coverage during the restart grace; this is the panel's loudest warning firing wrongly")
	}
}

// The reason to persist at all: last-seen must outlive the process.
func TestLastSeenSurvivesARestart(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)

	seen := now.Add(-40 * time.Second)
	src := &fakeSource{rows: []db.AgentRow{{Name: "mac", Executors: []string{"mac"}, LastSeenAt: &seen}}}
	f, _ := Build(src, reg, now)
	a := f.Agents[0]
	if a.LastSeen == nil || !a.LastSeen.Equal(seen) {
		t.Fatalf("last seen = %v, want the persisted %v", a.LastSeen, seen)
	}
	if a.LastSeenFrom != "last poll before restart" {
		t.Errorf("last seen from %q; the page must say which evidence it is showing", a.LastSeenFrom)
	}
}

// A live sighting is better evidence than a stored one and must win.
func TestALiveSightingBeatsTheStoredRow(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac", "ios"}, "https")

	old := now.Add(-time.Hour)
	src := &fakeSource{rows: []db.AgentRow{
		{Name: "mac", Executors: []string{"stale-queue"}, LastSeenAt: &old, LastScheme: "http"},
	}}
	f, _ := Build(src, reg, now)
	a := f.Agents[0]
	if a.LastSeenFrom != "claim poll" || !a.LastSeen.Equal(now) {
		t.Errorf("last seen %v from %q, want the live poll", a.LastSeen, a.LastSeenFrom)
	}
	if len(a.Executors) != 2 {
		t.Errorf("executors = %v, want what the open connection advertises", a.Executors)
	}
	if a.Scheme != "https" {
		t.Errorf("scheme = %q, want the live connection's, not the stored one's", a.Scheme)
	}
}

// One machine, three sources, one row.
func TestThreeSourcesMergeIntoOneAgent(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{
		names: []string{"mac"}, // build history
		rows:  []db.AgentRow{{Name: "mac", Executors: []string{"mac"}}},
		recent: map[string][]models.Build{
			"mac": {{ID: 1, Status: models.StatusSuccess}},
		},
	}
	f, _ := Build(src, reg, now)
	if len(f.Agents) != 1 {
		t.Fatalf("listed %d agents for one machine: %+v", len(f.Agents), f.Agents)
	}
	a := f.Agents[0]
	if !a.Known || !a.Remembered || len(a.Recent) != 1 {
		t.Errorf("merge lost a source: known=%v remembered=%v recent=%d", a.Known, a.Remembered, len(a.Recent))
	}
}

// A queue whose only agents are paused is a real problem, but a different one
// from a queue nothing serves. Collapsing them would send an operator hunting
// for a typo they never made.
func TestAQueueServedOnlyByPausedAgentsSaysSo(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{
		rows:      []db.AgentRow{pausedRow("mac", now.Add(time.Hour), "")},
		executors: []db.RemoteExecutor{{Name: "mac", Pending: 4}},
	}
	f, _ := Build(src, reg, now)
	e := f.Executors[0]
	if !e.Served {
		t.Error("reported as unserved; that means a name nothing answers to, which is a different fix")
	}
	if !e.AllPaused {
		t.Error("nothing says the queue is stalled behind a pause")
	}
	if e.Pending != 4 {
		t.Errorf("pending = %d", e.Pending)
	}
}

// With one paused and one running agent, the queue is fine.
func TestOnePausedAgentDoesNotStallAQueueAnotherServes(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac-a", []string{"mac"}, "https")
	reg.PollStarted("mac-b", []string{"mac"}, "https")

	src := &fakeSource{
		rows:      []db.AgentRow{pausedRow("mac-a", now.Add(time.Hour), "")},
		executors: []db.RemoteExecutor{{Name: "mac", Pending: 1}},
	}
	f, _ := Build(src, reg, now)
	e := f.Executors[0]
	if !e.Served || e.AllPaused {
		t.Errorf("served=%v allPaused=%v, want a working queue", e.Served, e.AllPaused)
	}
}

// The page an operator opens when nothing is building must not 500 because one
// agent row would not scan.
func TestAnUnreadableAgentTableDegradesRatherThanFails(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	src := &fakeSource{
		rowsErr:   errors.New("malformed row"),
		executors: []db.RemoteExecutor{{Name: "mac", Pending: 3}},
	}
	f, err := Build(src, reg, now)
	if err != nil {
		t.Fatalf("the whole page failed because the agents table did not read: %v", err)
	}
	if f.Degraded == "" {
		t.Error("degraded silently; a page missing its pause state reads as nothing being paused")
	}
	if len(f.Agents) != 1 || !f.Executors[0].Served {
		t.Error("live state was lost along with the stored state")
	}
}

// The stored timestamp is written on the same poll the registry records, a few
// microseconds later. Taking whichever is newer therefore relabels a live,
// actively-polling agent as one last seen before a restart — which is exactly
// what it did the first time this ran against a real server.
func TestALivePollIsNotLabelledAsPreRestart(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	reg.PollStarted("mac", []string{"mac"}, "https")

	// Stored a hair later than the registry's sighting, as the real write is.
	stored := now.Add(50 * time.Microsecond)
	src := &fakeSource{rows: []db.AgentRow{
		{Name: "mac", Executors: []string{"mac"}, LastSeenAt: &stored},
	}}
	f, _ := Build(src, reg, now.Add(time.Second))

	if got := f.Agents[0].LastSeenFrom; got != "claim poll" {
		t.Errorf("last seen from %q, want %q — this agent is on the other end of an open socket right now", got, "claim poll")
	}
}
