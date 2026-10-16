package notifications

import (
	"reflect"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

type narrowNotificationFleet struct {
	rows     []db.AgentRow
	running  *models.Build
	statuses []models.BuildStatus
}

func (s *narrowNotificationFleet) AgentNames() ([]string, error)         { return []string{"owner"}, nil }
func (s *narrowNotificationFleet) ListAgentRows() ([]db.AgentRow, error) { return s.rows, nil }
func (s *narrowNotificationFleet) RunningBuildHealthForAgent(string) (*models.Build, error) {
	return s.running, nil
}
func (s *narrowNotificationFleet) RecentBuildStatusesForAgent(_ string, limit int) ([]models.BuildStatus, error) {
	if len(s.statuses) > limit {
		return s.statuses[:limit], nil
	}
	return s.statuses, nil
}

func TestFleetHealthNotificationExactOwnershipAndAdvertisedProfiles(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	reg := agents.NewRegistry()
	reg.SetClock(func() time.Time { return now.Add(-time.Hour) })
	src := &narrowNotificationFleet{running: &models.Build{ID: 42, ProjectName: "current", Executor: "unity-windows", LastHeartbeatAt: &now}}
	projects := []models.Project{
		{Name: "current", Executor: "unity-windows", ExecutorProfiles: map[string]models.ExecutorProfile{"mac": {}}},
		{Name: "sibling", Executor: "unity-windows", ExecutorProfiles: map[string]models.ExecutorProfile{"linux": {}}},
		{Name: "legacy", Executor: "unity-windows"},
		{Name: "current", Executor: "other"},
		{Name: "local", Executor: models.ExecutorLocal},
	}
	matches := func(want []bool) agents.Agent {
		t.Helper()
		f, err := agents.BuildHealth(src, reg, now)
		if err != nil {
			t.Fatal(err)
		}
		a := f.Agents[0]
		got := make([]bool, len(projects))
		for i, p := range projects {
			got[i] = notificationAgentMatches(p, a)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("project eligibility = %v, want %v; agent %+v", got, want, a)
		}
		return a
	}
	a := matches([]bool{true, false, false, false, false})
	if len(a.Executors) != 0 {
		t.Fatal("ownership became a capability")
	}
	src.rows = []db.AgentRow{{Name: "owner", Executors: []string{"linux"}}}
	matches([]bool{true, true, false, false, false})
	// A current registry advertisement overrides old durable capabilities.
	reg.SetClock(func() time.Time { return now })
	reg.PollStarted("owner", []string{"mac"}, "https")()
	matches([]bool{true, false, false, false, false})
	// When ownership changes, it must not keep matching the previous project.
	src.running.ProjectName = "elsewhere"
	src.running.Executor = "other"
	matches([]bool{true, false, false, false, false}) // current still matches advertised mac
	src.rows = nil
	reg = agents.NewRegistry()
	reg.SetClock(func() time.Time { return now.Add(-time.Hour) })
	matches([]bool{false, false, false, false, false})
}

func TestFleetHealthNotificationFingerprintUsesStatusWindowAndNotActivity(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	reg := agents.NewRegistry()
	reg.SetClock(func() time.Time { return now.Add(-time.Hour) })
	src := &narrowNotificationFleet{running: &models.Build{ID: 1, ProjectName: "game", Executor: "mac", LastHeartbeatAt: &now},
		statuses: []models.BuildStatus{models.StatusRunning, models.StatusFailed, models.StatusPending, models.StatusFailed, models.StatusFailed}}
	observe := func() (string, agents.Agent) {
		t.Helper()
		f, err := agents.BuildHealth(src, reg, now)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := agentFingerprint(f.Agents[0])
		return fingerprint, f.Agents[0]
	}
	first, a := observe()
	if !a.FailureRun || a.ConsecutiveFailures != 3 || a.State != agents.StateBusy {
		t.Fatalf("failure/run liveness lost: %+v", a)
	}
	src.running.ID++
	src.statuses = append([]models.BuildStatus{models.StatusRunning}, src.statuses...)
	src.statuses = append(src.statuses, models.StatusFailed)
	second, _ := observe()
	if second != first {
		t.Fatal("build activity or growing failure streak changed semantic fingerprint")
	}
	src.statuses = []models.BuildStatus{models.StatusRunning, models.StatusSuccess, models.StatusFailed, models.StatusFailed, models.StatusFailed}
	recovered, a := observe()
	if recovered == first || a.FailureRun {
		t.Fatal("successful outcome did not clear failure incident")
	}
	src.running.LastHeartbeatAt = nil
	offline, a := observe()
	if offline == recovered || a.State != agents.StateOffline {
		t.Fatal("loss of heartbeat failed to create offline incident")
	}
}
