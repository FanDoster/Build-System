package agents

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// The narrow source has no UI methods. Compilation pins that BuildHealth can
// operate without any way to read logs, full summaries or queue depths.
type healthFixture struct {
	full                    *fakeSource
	runningErr, statusesErr error
}

func (s healthFixture) AgentNames() ([]string, error)         { return s.full.AgentNames() }
func (s healthFixture) ListAgentRows() ([]db.AgentRow, error) { return s.full.ListAgentRows() }
func (s healthFixture) RunningBuildHealthForAgent(name string) (*models.Build, error) {
	if s.runningErr != nil {
		return nil, s.runningErr
	}
	return s.full.RunningBuildForAgent(name)
}
func (s healthFixture) RecentBuildStatusesForAgent(name string, limit int) ([]models.BuildStatus, error) {
	if s.statusesErr != nil {
		return nil, s.statusesErr
	}
	recent := s.full.recent[name]
	if len(recent) > limit {
		recent = recent[:limit]
	}
	statuses := make([]models.BuildStatus, len(recent))
	for i := range recent {
		statuses[i] = recent[i].Status
	}
	return statuses, nil
}

func TestFleetHealthMatchesUIHealthWithoutHistoryOrSteps(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now.Add(-10 * time.Minute))
	// Registry and persisted-row inputs intentionally disagree. Live poll
	// capability/scheme wins, while persisted pause and self-report still apply.
	reg.mu.Lock()
	reg.seen["machine"] = &Sighting{Name: "machine", Executors: []string{"mac"}, Scheme: "https",
		LastPoll: now.Add(-5 * time.Minute), SkewKnown: true, Skew: 2 * time.Minute,
		DoubleSince: now.Add(-2 * time.Minute), LastDouble: now.Add(-time.Minute)}
	reg.mu.Unlock()
	old := now.Add(-time.Hour)
	until := now.Add(time.Hour)
	src := &fakeSource{
		names: []string{"machine", "historical"},
		rows: []db.AgentRow{{Name: "machine", Executors: []string{"linux"}, LastScheme: "http",
			LastSeenAt: &old, PausedUntil: &until, PauseNote: "maintenance",
			DiskFreeGB: 1, DiskFloorGB: 40, LastStatusAt: &old,
			Status: &models.AgentStatus{Checks: []models.AgentCheck{{Name: "git-lfs", Detail: "Install git-lfs", NeedsOperator: true}}}}},
		running:   map[string]*models.Build{"machine": {ID: 7, ProjectName: "game", Executor: "unity-windows", LastHeartbeatAt: at(now.Add(-time.Second))}},
		recent:    map[string][]models.Build{"machine": {{Status: models.StatusRunning}, {Status: models.StatusFailed}, {Status: models.StatusFailed}, {Status: models.StatusPending}, {Status: models.StatusFailed}, {Status: models.StatusCanceled}, {Status: models.StatusFailed}}},
		logs:      map[int64]string{7: "##[step:unity] display only"},
		executors: []db.RemoteExecutor{{Name: "unity-windows", Projects: []string{"game"}, ProjectExecutors: map[string][]string{"game": {"mac", "linux"}}}},
	}
	full, err := Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	health, err := BuildHealth(healthFixture{full: src}, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Agents) != len(health.Agents) {
		t.Fatal("historical agent lost")
	}
	for i := range full.Agents {
		want, got := full.Agents[i], health.Agents[i]
		if len(got.Recent) != 0 {
			t.Fatal("health path retained display history")
		}
		want.Recent = nil
		if want.Current != nil {
			current := *want.Current
			want.Current = &current
			if got.Current.Step != "" || got.Current.StepDetail != "" {
				t.Fatal("health path derived a step")
			}
			want.Current.Step, want.Current.StepDetail = "", ""
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("health differs from UI rules:\ngot %+v\nwant %+v", got, want)
		}
	}
	a := health.Agents[1] // alphabetical: historical, machine
	if a.State != StateBusy || !a.Paused || !a.DiskLow || !a.StatusStale || !a.Duplicate || !a.ClockOff || !a.FailureRun || a.ConsecutiveFailures != 3 {
		t.Fatalf("lost health dimensions: %+v", a)
	}
	if a.Scheme != "https" || !reflect.DeepEqual(a.Executors, []string{"mac"}) || a.LastSeenFrom != "build heartbeat" {
		t.Fatalf("merge precedence changed: %+v", a)
	}
	if full.Agents[1].Current.Step != "unity" || len(full.Agents[1].Recent) == 0 {
		t.Fatal("UI steps/history were removed")
	}
}

func TestFleetHealthLivenessAndRestartBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                        string
		age                         time.Duration
		open, remembered, heartbeat bool
		want                        State
	}{
		{"held poll despite old timestamp", time.Hour, true, false, false, StateOnline},
		{"recent poll", models.AgentHeartbeatTTL - time.Nanosecond, false, false, false, StateOnline},
		{"poll expires at ttl", models.AgentHeartbeatTTL, false, false, false, StateOffline},
		{"heartbeat only", time.Second, false, true, true, StateBusy},
		{"heartbeat expires at ttl", models.AgentHeartbeatTTL, false, true, true, StateOffline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registryAt(now.Add(-time.Hour))
			src := &fakeSource{names: []string{"machine"}}
			seen := now.Add(-tc.age)
			if tc.remembered {
				old := now.Add(-time.Hour)
				src.rows = []db.AgentRow{{Name: "machine", LastSeenAt: &old}}
			} else {
				reg.mu.Lock()
				reg.seen["machine"] = &Sighting{Name: "machine", LastPoll: seen}
				if tc.open {
					reg.seen["machine"].Polling = 1
				}
				reg.mu.Unlock()
			}
			if tc.heartbeat {
				src.running = map[string]*models.Build{"machine": {ID: 1, LastHeartbeatAt: &seen}}
			}
			fleet, err := BuildHealth(healthFixture{full: src}, reg, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := fleet.Agents[0].State; got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
			if tc.remembered && fleet.Agents[0].Known {
				t.Fatal("persisted agent became known")
			}
		})
	}
	reg := registryAt(now)
	old := now.Add(-time.Hour)
	src := healthFixture{full: &fakeSource{rows: []db.AgentRow{{Name: "machine", LastSeenAt: &old}}}}
	for _, tc := range []struct {
		after time.Duration
		want  State
	}{{models.AgentHeartbeatTTL - time.Nanosecond, StateWaiting}, {models.AgentHeartbeatTTL, StateOffline}} {
		fleet, err := BuildHealth(src, reg, now.Add(tc.after))
		if err != nil {
			t.Fatal(err)
		}
		if fleet.Agents[0].State != tc.want {
			t.Fatalf("restart state at %s = %s", tc.after, fleet.Agents[0].State)
		}
	}
}

func TestFleetHealthDoesNotInventCapabilitiesFromOwnership(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	src := &fakeSource{names: []string{"machine"}, running: map[string]*models.Build{
		"machine": {ID: 1, ProjectName: "game", Executor: "mac", LastHeartbeatAt: &now},
	}}
	health, err := BuildHealth(healthFixture{full: src}, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(health.Agents[0].Executors) != 0 || health.Agents[0].Current.Project != "game" || health.Agents[0].Current.Executor != "mac" {
		t.Fatal("ownership became a queue-wide advertised capability")
	}
	full, err := Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(full.Agents[0].Executors, "mac") {
		t.Fatal("UI legacy coverage inference changed")
	}
}

func TestFleetHealthStatusWindowAndFailureBoundaries(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		statuses []models.BuildStatus
		want     int
	}{
		{"skip in-flight", []models.BuildStatus{models.StatusRunning, models.StatusFailed, models.StatusPending, models.StatusFailed}, 2},
		{"success stops", []models.BuildStatus{models.StatusFailed, models.StatusSuccess, models.StatusFailed}, 1},
		{"cancel stops", []models.BuildStatus{models.StatusFailed, models.StatusCanceled, models.StatusFailed}, 1},
		{"unknown outcome stops", []models.BuildStatus{models.StatusFailed, "unknown", models.StatusFailed}, 1},
		{"newest ten include in-flight", []models.BuildStatus{models.StatusRunning, models.StatusPending, models.StatusRunning, models.StatusPending, models.StatusRunning, models.StatusPending, models.StatusRunning, models.StatusPending, models.StatusRunning, models.StatusFailed, models.StatusFailed, models.StatusFailed}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{names: []string{"machine"}, recent: map[string][]models.Build{}}
			for _, status := range tc.statuses {
				src.recent["machine"] = append(src.recent["machine"], models.Build{Status: status})
			}
			f, err := BuildHealth(healthFixture{full: src}, registryAt(now), now)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Agents[0].ConsecutiveFailures; got != tc.want {
				t.Fatalf("failures = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFleetHealthErrorsDoNotBecomeHealthTransitions(t *testing.T) {
	now := time.Now()
	failure := errors.New("synthetic read failure")
	for _, src := range []healthFixture{
		{full: &fakeSource{names: []string{"machine"}}, runningErr: failure},
		{full: &fakeSource{names: []string{"machine"}}, statusesErr: failure},
	} {
		if _, err := BuildHealth(src, registryAt(now), now); !errors.Is(err, failure) {
			t.Fatalf("read error lost: %v", err)
		}
	}
	// As in the UI, agent-table failure degrades/fails open, rather than
	// pretending the fleet is paused. The worker must refuse degraded results.
	f, err := BuildHealth(healthFixture{full: &fakeSource{names: []string{"machine"}, rowsErr: failure}}, registryAt(now), now)
	if err != nil {
		t.Fatal(err)
	}
	if f.Degraded == "" || f.Agents[0].Paused {
		t.Fatal("row failure lost degraded/fail-open semantics")
	}
}

func BenchmarkFleetHealthAssembly(b *testing.B) {
	now := time.Now()
	reg := registryAt(now)
	src := &fakeSource{names: []string{"machine"}, running: map[string]*models.Build{"machine": {ID: 1, ProjectName: "game", Executor: "mac", LastHeartbeatAt: &now}}}
	for i := 0; i < b.N; i++ {
		if _, err := BuildHealth(healthFixture{full: src}, reg, now); err != nil {
			b.Fatal(err)
		}
	}
}
