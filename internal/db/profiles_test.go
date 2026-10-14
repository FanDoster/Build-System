package db

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/FanDoster/Build-System/internal/models"
)

func sharedProject(t *testing.T, d *DB, name string) *models.Project {
	t.Helper()
	p := &models.Project{
		Name: name, RepoURL: "https://github.com/u/" + name, Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: name, Executor: "unity-windows",
		BuildCommand: "unsafe base", BuildPath: "base", UploadScript: "base upload",
		ExecutorProfiles: map[string]models.ExecutorProfile{
			"linux": {BuildCommand: "linux build", BuildPath: "linux output"},
			"mac":   {BuildCommand: "mac build", BuildPath: "mac output", UploadScript: "mac upload"},
		},
	}
	if err := d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecutorProfilesPersistence(t *testing.T) {
	d := openTestDB(t)
	p := sharedProject(t, d, "game")
	p.WebhookSecret, p.CloneToken = "secret", "token"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ExecutorProfiles, p.ExecutorProfiles) {
		t.Fatalf("profiles not persisted: %+v", got.ExecutorProfiles)
	}
	byName, err := d.GetProjectByName(p.Name)
	if err != nil || !reflect.DeepEqual(byName.ExecutorProfiles, p.ExecutorProfiles) {
		t.Fatalf("GetProjectByName = %+v, %v", byName, err)
	}
	list, err := d.ListProjects()
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].ExecutorProfiles, p.ExecutorProfiles) {
		t.Fatalf("ListProjects = %+v, %v", list, err)
	}
	if list[0].WebhookSecret != "" || list[0].CloneToken != "" {
		t.Fatal("ListProjects leaked secrets")
	}
	// Empty upload remains deliberately build-only despite a base upload.
	profile, ok := got.ProfileFor([]string{"linux"})
	if !ok || profile.UploadScript != "" {
		t.Fatalf("linux upload inherited base: %+v", profile)
	}
	got.ExecutorProfiles = map[string]models.ExecutorProfile{"mac": {BuildCommand: "new", BuildPath: "new output"}}
	if err := d.UpdateProject(got); err != nil {
		t.Fatal(err)
	}
	again, err := d.GetProject(p.ID)
	if err != nil || !reflect.DeepEqual(again.ExecutorProfiles, got.ExecutorProfiles) {
		t.Fatalf("updated profiles = %+v, %v", again, err)
	}
	// Clearing profiles restores legacy eligibility and stores an object.
	got.ExecutorProfiles = nil
	if err := d.UpdateProject(got); err != nil {
		t.Fatal(err)
	}
	again, err = d.GetProject(p.ID)
	if err != nil || len(again.ExecutorProfiles) != 0 {
		t.Fatalf("clear profiles = %+v, %v", again, err)
	}
	var stored string
	if err := d.conn.QueryRow(`SELECT executor_profiles FROM projects WHERE id=?`, p.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "{}" {
		t.Errorf("empty stored profiles = %q, want {}", stored)
	}
}

func TestExecutorProfilesMigrationFromOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := agentProject(t, d, "legacy", "mac")
	b := pendingBuild(t, d, p.ID)
	if _, err := d.conn.Exec(`ALTER TABLE projects DROP COLUMN executor_profiles`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatalf("open old schema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.migrate(); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	got, err := d.GetProject(p.ID)
	if err != nil || len(got.ExecutorProfiles) != 0 || got.Executor != "mac" {
		t.Fatalf("legacy project after migration = %+v, %v", got, err)
	}
	var stored string
	if err := d.conn.QueryRow(`SELECT executor_profiles FROM projects WHERE id=?`, p.ID).Scan(&stored); err != nil || stored != "{}" {
		t.Fatalf("migrated default = %q, %v", stored, err)
	}
	claimed, err := d.ClaimBuildForAgent("mac-1", []string{"mac"})
	if err != nil || claimed == nil || claimed.ID != b.ID || claimed.Number != 1 {
		t.Fatalf("legacy claim after migration = %+v, %v", claimed, err)
	}
	// A migrated row can save and use the new profiles.
	got.Executor, got.ExecutorProfiles = "unity-windows", map[string]models.ExecutorProfile{"linux": {BuildCommand: "linux"}}
	if err := d.UpdateProject(got); err != nil {
		t.Fatal(err)
	}
	next := pendingBuild(t, d, p.ID)
	claimed, err = d.ClaimBuildForAgent("linux-1", []string{"linux"})
	if err != nil || claimed == nil || claimed.ID != next.ID || claimed.Number != 2 {
		t.Fatalf("new profile claim after migration = %+v, %v", claimed, err)
	}
}

func TestSharedQueueClaimAllowlistAndLocalIsolation(t *testing.T) {
	d := openTestDB(t)
	local := agentProject(t, d, "local", "local")
	local.ExecutorProfiles = map[string]models.ExecutorProfile{"linux": {BuildCommand: "not remote"}}
	if err := d.UpdateProject(local); err != nil {
		t.Fatal(err)
	}
	localBuild := pendingBuild(t, d, local.ID)
	shared := sharedProject(t, d, "game")
	sharedBuild := pendingBuild(t, d, shared.ID)
	for _, executors := range [][]string{{"local"}, {""}, {"windows"}, {"unity-windows"}, {"local", "unity-windows"}} {
		got, err := d.ClaimBuildForAgent("unsupported", executors)
		if err != nil || got != nil {
			t.Fatalf("ineligible executors %v claimed %+v, %v", executors, got, err)
		}
	}
	got, err := d.ClaimBuildForAgent("linux-1", []string{"local", "linux"})
	if err != nil || got == nil || got.ID != sharedBuild.ID || got.Executor != "unity-windows" {
		t.Fatalf("linux shared claim = %+v, %v", got, err)
	}
	untouched, err := d.GetBuild(localBuild.ID)
	if err != nil || untouched.Status != models.StatusPending {
		t.Fatalf("local build touched: %+v, %v", untouched, err)
	}
}

func TestSharedQueueClaimsOldestEligibleBuild(t *testing.T) {
	d := openTestDB(t)
	nonmatch := agentProject(t, d, "other", "linux")
	nonmatch.ExecutorProfiles = map[string]models.ExecutorProfile{"mac": {BuildCommand: "mac only"}}
	if err := d.UpdateProject(nonmatch); err != nil {
		t.Fatal(err)
	}
	skipped := pendingBuild(t, d, nonmatch.ID)
	p := sharedProject(t, d, "game")
	newer := pendingBuild(t, d, p.ID)
	older := pendingBuild(t, d, p.ID)
	// Age wins over row ID; ID remains the tie-breaker at equal ages.
	if _, err := d.conn.Exec(`UPDATE builds SET created_at='2020-01-01 00:00:00' WHERE id=?`, older.ID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []*models.Build{older, newer} {
		got, err := d.ClaimBuildForAgent("linux-1", []string{"linux"})
		if err != nil || got == nil || got.ID != want.ID {
			t.Fatalf("oldest eligible = %+v, %v; want %d", got, err, want.ID)
		}
	}
	got, err := d.GetBuild(skipped.ID)
	if err != nil || got.Status != models.StatusPending {
		t.Fatalf("nonmatching profile project touched: %+v, %v", got, err)
	}
}

func TestSharedQueueConcurrentClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	linuxDB, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { linuxDB.Close() })
	macDB, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { macDB.Close() })
	p := sharedProject(t, linuxDB, "game")
	first := pendingBuild(t, linuxDB, p.ID)
	second := pendingBuild(t, linuxDB, p.ID)
	type result struct {
		build *models.Build
		err   error
	}
	claim := func() []result {
		start := make(chan struct{})
		results := make([]result, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		for i, worker := range []struct {
			db             *DB
			name, executor string
		}{{linuxDB, "linux-1", "linux"}, {macDB, "mac-1", "mac"}} {
			go func(i int, d *DB, name, executor string) {
				defer wg.Done()
				<-start
				results[i].build, results[i].err = d.ClaimBuildForAgent(name, []string{executor})
			}(i, worker.db, worker.name, worker.executor)
		}
		close(start)
		wg.Wait()
		return results
	}
	seen := map[int64]bool{}
	for _, result := range claim() {
		b := result.build
		if result.err != nil || b == nil {
			t.Fatalf("concurrent claim = %+v, %v", b, result.err)
		}
		if seen[b.ID] {
			t.Fatal("same build claimed twice")
		}
		seen[b.ID] = true
		if b.Status != models.StatusRunning || b.StartedAt == nil || b.LastHeartbeatAt == nil || b.Executor != "unity-windows" {
			t.Fatalf("claim lost ownership metadata: %+v", b)
		}
		if (b.ID == first.ID && b.Number != 1) || (b.ID == second.ID && b.Number != 2) {
			t.Fatalf("claim changed project build number: %+v", b)
		}
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatal("concurrent workers did not take both builds from the same project")
	}
	// Competing platforms must also remain exclusive with only one row left.
	third := pendingBuild(t, linuxDB, p.ID)
	claimed := 0
	for _, result := range claim() {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.build != nil {
			claimed++
			if result.build.ID != third.ID || result.build.Number != 3 {
				t.Fatalf("unexpected final claim: %+v", result.build)
			}
		}
	}
	if claimed != 1 {
		t.Fatalf("single build was claimed %d times, want 1", claimed)
	}
}
