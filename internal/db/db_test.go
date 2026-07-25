package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newProject(t *testing.T, d *DB) *models.Project {
	t.Helper()
	p := &models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		WebhookSecret: "whsec", CloneToken: "tok",
	}
	if err := d.CreateProject(p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func TestProjectRoundtrip(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	got, err := d.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "app" || got.WebhookSecret != "whsec" || got.CloneToken != "tok" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}

	got.Branch = "develop"
	if err := d.UpdateProject(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := d.GetProject(p.ID)
	if got2.Branch != "develop" {
		t.Errorf("update not persisted: %+v", got2)
	}

	// ListProjects intentionally omits secret columns.
	list, err := d.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].WebhookSecret != "" || list[0].CloneToken != "" {
		t.Errorf("ListProjects should not return secrets: %+v", list)
	}

	if err := d.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := d.GetProject(p.ID); err == nil {
		t.Error("project still present after delete")
	}
}

func TestBuildLifecycle(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc123", CommitMessage: "msg"}
	if err := d.CreateBuild(b); err != nil {
		t.Fatalf("create build: %v", err)
	}

	if err := d.UpdateBuildStatus(b.ID, models.StatusRunning, "started\n"); err != nil {
		t.Fatalf("running: %v", err)
	}
	got, _ := d.GetBuild(b.ID)
	if got.Status != models.StatusRunning || got.StartedAt == nil || got.FinishedAt != nil {
		t.Errorf("running state wrong: %+v", got)
	}

	if err := d.AppendBuildLog(b.ID, "line two\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _ = d.GetBuild(b.ID)
	if got.Log != "started\nline two\n" {
		t.Errorf("log append mismatch: %q", got.Log)
	}

	if err := d.UpdateBuildStatus(b.ID, models.StatusSuccess, "full log"); err != nil {
		t.Fatalf("success: %v", err)
	}
	got, _ = d.GetBuild(b.ID)
	if got.Status != models.StatusSuccess || got.StartedAt == nil || got.FinishedAt == nil {
		t.Errorf("finished state wrong: %+v", got)
	}
	if got.ProjectName != "app" {
		t.Errorf("project name not joined: %q", got.ProjectName)
	}
}

func TestListBuildsByStatus(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	for _, st := range []models.BuildStatus{models.StatusPending, models.StatusPending, models.StatusRunning} {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		if st != models.StatusPending {
			if err := d.UpdateBuildStatus(b.ID, st, ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	pending, err := d.ListBuildsByStatus(models.StatusPending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("pending: got %d, want 2", len(pending))
	}
	running, err := d.ListBuildsByStatus(models.StatusRunning)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 1 {
		t.Errorf("running: got %d, want 1", len(running))
	}
}

func TestDeleteProjectCascadesBuilds(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := d.GetBuild(b.ID); err == nil {
		t.Error("build survived project deletion (ON DELETE CASCADE not effective)")
	}
	// GetBuild JOINs projects, so an orphaned row reads as deleted whether or
	// not the cascade fired. Count the raw row to actually test the cascade.
	var n int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM builds WHERE id = ?`, b.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("orphaned build row left behind: ON DELETE CASCADE did not fire")
	}
}

// TestPragmasApplied guards the connection DSN. The pragma spelling is
// driver-specific and modernc.org/sqlite ignores unrecognized params without
// error, so a wrong DSN degrades silently rather than failing to open.
func TestPragmasApplied(t *testing.T) {
	d := openTestDB(t)

	var journal string
	if err := d.conn.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want %q", journal, "wal")
	}

	var fk int
	if err := d.conn.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (ON DELETE CASCADE depends on it)", fk)
	}

	var busy int
	if err := d.conn.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy == 0 {
		t.Error("busy_timeout = 0, want a nonzero timeout")
	}
}

func TestDeleteOrphanedBuilds(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	keep := &models.Build{ProjectID: p.ID, Status: models.StatusSuccess}
	if err := d.CreateBuild(keep); err != nil {
		t.Fatal(err)
	}
	// Simulate a row stranded by a project delete from before the foreign-key
	// pragma worked. The constraint now rejects such an insert, so drop it for
	// the setup only — this is exactly the state the sweep exists to clean up.
	if _, err := d.conn.Exec(`PRAGMA foreign_keys=off`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(
		`INSERT INTO builds (project_id, status, log) VALUES (?, ?, ?)`,
		p.ID+999, models.StatusSuccess, "stale log",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(`PRAGMA foreign_keys=on`); err != nil {
		t.Fatal(err)
	}

	n, err := d.DeleteOrphanedBuilds()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1", n)
	}
	if _, err := d.GetBuild(keep.ID); err != nil {
		t.Errorf("live build was swept: %v", err)
	}

	// Idempotent: a second pass has nothing left to do.
	if n, err := d.DeleteOrphanedBuilds(); err != nil || n != 0 {
		t.Errorf("second sweep: n=%d err=%v, want 0/nil", n, err)
	}
}

func TestFailStaleRunningPreservesUnknownFinishTime(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	mk := func() *models.Build {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.ClaimBuild(b.ID); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		return b
	}
	current, stale := mk(), mk()

	failed, err := d.FailStaleRunning(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0] != stale.ID {
		t.Fatalf("failed ids = %v, want [%d]", failed, stale.ID)
	}

	got, _ := d.GetBuild(stale.ID)
	if got.Status != models.StatusFailed {
		t.Errorf("stale status = %s, want failed", got.Status)
	}
	if got.FinishedAt != nil {
		t.Errorf("stale build got a fabricated finished_at: %v (poisons history durations)", got.FinishedAt)
	}
	if !strings.Contains(got.Log, "interrupted by server restart") {
		t.Errorf("missing sweep note in log: %q", got.Log)
	}
	if got.Duration() != "" {
		t.Errorf("duration should be unknown, got %q", got.Duration())
	}

	cur, _ := d.GetBuild(current.ID)
	if cur.Status != models.StatusRunning {
		t.Errorf("current build was swept: %s", cur.Status)
	}
}

func TestRepairInterruptedDurations(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	// Simulate a row swept by the OLD code: failed with the marker AND a
	// bogus finished_at stamped at restart time.
	d.ClaimBuild(b.ID)
	d.UpdateBuildStatus(b.ID, models.StatusFailed, "some log\n[ERROR] Build interrupted by server restart\n")
	if got, _ := d.GetBuild(b.ID); got.FinishedAt == nil {
		t.Fatal("precondition: finished_at should be set by old-style sweep")
	}

	n, err := d.RepairInterruptedDurations()
	if err != nil || n != 1 {
		t.Fatalf("repair: n=%d err=%v", n, err)
	}
	got, _ := d.GetBuild(b.ID)
	if got.FinishedAt != nil {
		t.Errorf("finished_at not cleared: %v", got.FinishedAt)
	}
	// Idempotent.
	if n, _ := d.RepairInterruptedDurations(); n != 0 {
		t.Errorf("second repair touched %d rows, want 0", n)
	}
}

func TestExpectedDuration(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	if _, ok := d.ExpectedDuration(p.ID); ok {
		t.Error("no history should mean no estimate")
	}

	// Seed successful builds with controlled 30s/60s durations.
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	for i, secs := range []int{30, 60} {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		start := base.Add(time.Duration(i) * time.Hour)
		if _, err := d.conn.Exec(
			`UPDATE builds SET status=?, started_at=?, finished_at=? WHERE id=?`,
			models.StatusSuccess, start, start.Add(time.Duration(secs)*time.Second), b.ID,
		); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := d.ExpectedDuration(p.ID)
	if !ok || got != 45*time.Second {
		t.Errorf("expected duration = %v ok=%v, want 45s", got, ok)
	}
}

func TestProjectNoCacheRoundtripAndMigration(t *testing.T) {
	d := openTestDB(t)

	// Default is false.
	p := &models.Project{
		Name: "app", RepoURL: "https://x", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	}
	if err := d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetProject(p.ID)
	if got.NoCache {
		t.Error("no_cache should default to false")
	}

	// Toggle on, verify via Get, GetByName, and List.
	got.NoCache = true
	if err := d.UpdateProject(got); err != nil {
		t.Fatal(err)
	}
	if g, _ := d.GetProject(p.ID); !g.NoCache {
		t.Error("no_cache not persisted via GetProject")
	}
	if g, _ := d.GetProjectByName("app"); !g.NoCache {
		t.Error("no_cache not returned by GetProjectByName")
	}
	list, _ := d.ListProjects()
	if len(list) != 1 || !list[0].NoCache {
		t.Errorf("no_cache not returned by ListProjects: %+v", list)
	}
}

// migrate() must add no_cache to a DB created without it, without clobbering
// existing rows.
func TestMigrationAddsNoCacheColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Build a pre-migration schema: projects table with no no_cache column.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE projects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE, repo_url TEXT NOT NULL,
		branch TEXT NOT NULL DEFAULT 'main', dockerfile_path TEXT NOT NULL DEFAULT 'Dockerfile',
		image_name TEXT NOT NULL, deploy_compose_path TEXT DEFAULT '',
		deploy_service_name TEXT DEFAULT '', webhook_secret TEXT NOT NULL DEFAULT '',
		clone_token TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now')));`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`INSERT INTO projects (name, repo_url, image_name) VALUES ('legacy','https://x','img')`)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	// Open through the normal path — migrate() must add the column.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open/migrate legacy db: %v", err)
	}
	defer d.Close()

	got, err := d.GetProjectByName("legacy")
	if err != nil {
		t.Fatalf("legacy row unreadable after migration: %v", err)
	}
	if got.NoCache {
		t.Error("migrated column should default to false")
	}
	// Idempotent: opening again must not error on a duplicate column.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second open (idempotent migration) failed: %v", err)
	}
	d2.Close()
}

// A SIGKILL after the stop grace period leaves rows stranded as 'running' —
// that path never gets to write anything, so recovery happens at startup.
func TestRequeueStaleRunning(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	newRunning := func() *models.Build {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		if _, err := d.conn.Exec(`UPDATE builds SET status=? WHERE id=?`, models.StatusRunning, b.ID); err != nil {
			t.Fatal(err)
		}
		return b
	}

	fresh := newRunning()
	exhausted := newRunning()
	if _, err := d.conn.Exec(`UPDATE builds SET requeues=? WHERE id=?`, models.MaxBuildRequeues, exhausted.ID); err != nil {
		t.Fatal(err)
	}

	requeued, err := d.RequeueStaleRunning(models.MaxBuildRequeues)
	if err != nil {
		t.Fatalf("requeue sweep: %v", err)
	}
	if len(requeued) != 1 || requeued[0] != fresh.ID {
		t.Fatalf("requeued = %v, want just build %d", requeued, fresh.ID)
	}

	got, _ := d.GetBuild(fresh.ID)
	if got.Status != models.StatusPending || got.Requeues != 1 {
		t.Errorf("fresh build: status=%s requeues=%d, want pending/1", got.Status, got.Requeues)
	}
	if !strings.Contains(got.Log, "re-queued") {
		t.Errorf("no restart seam in log: %q", got.Log)
	}

	// The exhausted one is left running, for FailStaleRunning to mark failed.
	stuck, _ := d.GetBuild(exhausted.ID)
	if stuck.Status != models.StatusRunning {
		t.Errorf("exhausted build: status=%s, want it left for FailStaleRunning", stuck.Status)
	}
	if failed, err := d.FailStaleRunning(0); err != nil || len(failed) != 1 || failed[0] != exhausted.ID {
		t.Errorf("FailStaleRunning = %v (err=%v), want just build %d", failed, err, exhausted.ID)
	}
}

// RequeueBuild only acts on running rows: a finished build must never be
// resurrected by a late restart.
func TestRequeueBuildIgnoresTerminalBuilds(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)
	b := &models.Build{ProjectID: p.ID, Status: models.StatusSuccess}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(`UPDATE builds SET status=? WHERE id=?`, models.StatusSuccess, b.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.RequeueBuild(b.ID, models.MaxBuildRequeues); err != nil || ok {
		t.Errorf("RequeueBuild on a success = %v (err=%v), want false", ok, err)
	}
	got, _ := d.GetBuild(b.ID)
	if got.Status != models.StatusSuccess {
		t.Errorf("status = %s, want success untouched", got.Status)
	}
}
