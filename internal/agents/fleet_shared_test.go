package agents

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

func sharedExecutor(projects map[string][]string) db.RemoteExecutor {
	e := db.RemoteExecutor{Name: "unity-windows", ProjectExecutors: projects, Pending: 3}
	for _, name := range []string{"demo", "main", "legacy"} {
		if _, ok := projects[name]; ok {
			e.Projects = append(e.Projects, name)
		}
	}
	return e
}

func TestSharedQueueUsesProfileCoverage(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		profiles     map[string][]string
		agents       map[string][]string
		wantAgents   []string
		wantUnserved []string
		served       bool
	}{
		{"mac serves both projects", map[string][]string{"demo": {"linux", "mac"}, "main": {"linux", "mac"}}, map[string][]string{"mac-1": {"mac"}}, []string{"mac-1"}, nil, true},
		{"linux serves both projects", map[string][]string{"demo": {"linux", "mac"}, "main": {"linux", "mac"}}, map[string][]string{"linux-1": {"linux"}}, []string{"linux-1"}, nil, true},
		{"both profiles do not duplicate an agent", map[string][]string{"demo": {"linux", "mac"}, "main": {"linux", "mac"}}, map[string][]string{"both": {"mac", "linux", "mac"}, "linux-1": {"linux"}}, []string{"both", "linux-1"}, nil, true},
		{"logical queue is not a profile", map[string][]string{"demo": {"linux", "mac"}}, map[string][]string{"wrong": {"unity-windows"}}, nil, []string{"demo"}, false},
		{"unsupported individual project stays unserved", map[string][]string{"demo": {"mac"}, "main": {"linux"}}, map[string][]string{"mac-1": {"mac"}}, []string{"mac-1"}, []string{"main"}, false},
		{"different agents cover different projects", map[string][]string{"demo": {"mac"}, "main": {"linux"}}, map[string][]string{"mac-1": {"mac"}, "linux-1": {"linux"}}, []string{"linux-1", "mac-1"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registryAt(now)
			for name, profiles := range tc.agents {
				reg.PollStarted(name, profiles, "https")
			}
			src := &fakeSource{executors: []db.RemoteExecutor{sharedExecutor(tc.profiles)}}
			f, err := Build(src, reg, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Executors) != 1 {
				t.Fatalf("queues = %+v, want one logical queue", f.Executors)
			}
			e := f.Executors[0]
			if e.Name != "unity-windows" || e.Pending != 3 {
				t.Errorf("queue = %+v", e)
			}
			if e.Served != tc.served || !reflect.DeepEqual(e.Agents, tc.wantAgents) || !reflect.DeepEqual(e.UnservedProjects, tc.wantUnserved) {
				t.Errorf("coverage = %+v, want served=%v agents=%v unserved=%v", e, tc.served, tc.wantAgents, tc.wantUnserved)
			}
		})
	}
}

func TestSharedQueuePauseAndOfflineSemantics(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		allPaused, offline bool
	}{
		{"all paused", true, false}, {"one paused", false, false}, {"offline agents", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := now
			if tc.offline {
				clock = now.Add(-time.Hour)
			}
			reg := registryAt(clock)
			reg.PollStarted("mac-1", []string{"mac"}, "https")()
			reg.PollStarted("linux-1", []string{"linux"}, "https")()
			rows := []db.AgentRow{pausedRow("mac-1", now.Add(time.Hour), "maintenance")}
			if tc.allPaused {
				rows = append(rows, pausedRow("linux-1", now.Add(time.Hour), "maintenance"))
			}
			src := &fakeSource{rows: rows, executors: []db.RemoteExecutor{sharedExecutor(map[string][]string{"demo": {"mac", "linux"}})}}
			f, err := Build(src, reg, now)
			if err != nil {
				t.Fatal(err)
			}
			e := f.Executors[0]
			if e.Served == tc.offline || e.AllPaused != tc.allPaused {
				t.Errorf("coverage = %+v", e)
			}
			if tc.offline && len(e.Agents) != 0 {
				t.Errorf("offline agents cover queue: %+v", e)
			}
		})
	}
}

func TestSharedQueueRememberedAgentRestartGrace(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	src := &fakeSource{
		rows:      []db.AgentRow{{Name: "mac-1", Executors: []string{"mac"}, LastSeenAt: at(now.Add(-time.Hour))}},
		executors: []db.RemoteExecutor{sharedExecutor(map[string][]string{"demo": {"linux", "mac"}})},
	}
	f, err := Build(src, reg, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if f.Agents[0].State != StateWaiting || !f.Executors[0].Served {
		t.Fatalf("lost restart grace: %+v", f)
	}
	f, err = Build(src, reg, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if f.Agents[0].State != StateOffline || f.Executors[0].Served {
		t.Fatalf("coverage after restart grace: %+v", f)
	}
}

func TestBusySharedAgentProvesOnlyItsProjectWithoutAdvertisedProfiles(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	reg := registryAt(now)
	src := &fakeSource{
		names:     []string{"busy-mac"},
		running:   map[string]*models.Build{"busy-mac": {ID: 1, ProjectName: "demo", Executor: "unity-windows", Status: models.StatusRunning, LastHeartbeatAt: at(now)}},
		executors: []db.RemoteExecutor{sharedExecutor(map[string][]string{"demo": {"mac"}, "main": {"linux"}, "legacy": {"unity-windows"}})},
	}
	f, err := Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	e := f.Executors[0]
	if f.Agents[0].State != StateBusy || len(f.Agents[0].Executors) != 0 {
		t.Errorf("busy agent inferred a profile it never advertised: %+v", f.Agents[0])
	}
	if e.Served || !reflect.DeepEqual(e.Agents, []string{"busy-mac"}) || !reflect.DeepEqual(e.UnservedProjects, []string{"main", "legacy"}) {
		t.Errorf("coverage = %+v", e)
	}
	src.executors[0] = sharedExecutor(map[string][]string{"demo": {"mac"}})
	f, err = Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Executors[0].Served {
		t.Errorf("running project is not covered: %+v", f.Executors[0])
	}
}

// Exercise the production Source, including persisted profiles and pending
// builds, so a shared row cannot accidentally become two queues or two builds.
func TestDatabaseFleetGroupsSharedProjectsAndCountsPendingOnce(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	profiles := map[string]models.ExecutorProfile{"mac": {BuildCommand: "mac-build"}, "linux": {BuildCommand: "linux-build"}}
	projects := []models.Project{
		{Name: "main", Executor: "unity-windows", ExecutorProfiles: profiles},
		{Name: "demo", Executor: "unity-windows", ExecutorProfiles: profiles},
		{Name: "legacy-mac", Executor: "mac"},
		{Name: "legacy-linux", Executor: "linux"},
		{Name: "local", Executor: models.ExecutorLocal},
	}
	for i := range projects {
		if err := d.CreateProject(&projects[i]); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			status := models.StatusPending
			if n == 1 && i != 0 {
				status = models.StatusSuccess
			}
			if err := d.CreateBuild(&models.Build{ProjectID: projects[i].ID, Status: status, CommitSHA: "abc"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now().UTC()
	reg := registryAt(now)
	reg.PollStarted("mac-1", []string{"mac", "linux"}, "https")
	f, err := Build(d, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Executors) != 3 {
		t.Fatalf("queues = %+v, want shared plus two legacy", f.Executors)
	}
	for _, e := range f.Executors {
		if !e.Served || !reflect.DeepEqual(e.Agents, []string{"mac-1"}) {
			t.Errorf("coverage = %+v", e)
		}
		if e.Name == "unity-windows" {
			if !reflect.DeepEqual(e.Projects, []string{"demo", "main"}) || e.Pending != 3 || e.OldestPending == nil {
				t.Errorf("shared queue = %+v", e)
			}
		} else if e.Pending != 1 {
			t.Errorf("legacy queue = %+v", e)
		}
	}
}
